// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
)

type removeParams struct {
	NodeID   *string `json:"nodeId"`
	NodeUUID *string `json:"nodeUuid"`
}

// handleNodesRemove removes a node from the cluster (§7.0): it best-effort
// notifies the peer over mTLS (while the pin still authenticates the
// connection), then deletes the pin and drops the membership. A non-member is a
// removed:false no-op.
func (m *Manager) handleNodesRemove(msg *Message) {
	var p removeParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
		return
	}
	if p.NodeID == nil && p.NodeUUID == nil {
		m.codec.RespondErrorData(msg.ID, codeInvalidParams, "nodeId or nodeUuid is required",
			map[string]string{"field": "nodeId"})
		return
	}

	var node *ClusterNode
	var proof RemovalProof
	var cid0 string
	var selfEpoch0 uint64
	var prepareErr error

	// Bind proof creation to the exact live cluster composition. Without this
	// boundary, a leave/rejoin between lookup and persistence could introduce an
	// old-cluster proof into the new admission.
	m.rosterMu.Lock()
	if p.NodeUUID != nil {
		node, _ = m.memberByNodeID(*p.NodeUUID)
	}
	if node == nil && p.NodeID != nil {
		node, _ = m.memberByNodeID(*p.NodeID)
	}
	if node == nil {
		m.rosterMu.Unlock()
		id := ""
		if p.NodeID != nil {
			id = *p.NodeID
		}
		m.codec.Respond(msg.ID, map[string]any{"nodeId": id, "removed": false})
		return
	}
	// Removing self is not a peer removal — it would evict this node from
	// everyone else while leaving it locally still clustered and still trusting
	// all peers. Self-departure is cluster:leave.
	if node.NodeUUID == m.identity.NodeUUID {
		m.rosterMu.Unlock()
		m.codec.RespondErrorData(msg.ID, codeInvalidParams,
			"cannot remove self; use cluster:leave to unjoin the cluster",
			map[string]string{"field": "nodeUuid"})
		return
	}

	cid0, selfEpoch0 = m.currentAdmission()
	liveCID, _ := m.clusterIdentity()
	pin, pinned := m.trust.Get(node.NodeUUID)
	if cid0 == "" || liveCID != cid0 || selfEpoch0 == 0 ||
		node.ClusterID != cid0 || node.AdmissionEpoch == 0 ||
		!pinned || pin.ClusterID != cid0 || pin.AdmissionEpoch != node.AdmissionEpoch {
		prepareErr = fmt.Errorf("member admission is not authenticated in the active cluster")
	} else if proof, prepareErr = m.newRemovalProof(node.NodeUUID, node.AdmissionEpoch); prepareErr == nil {
		if _, prepareErr = m.putRemovalProof(proof); prepareErr == nil {
			// Keep the legacy tombstone mirror for old peers. It is never local
			// admission authority.
			m.putTombstone(proof.Tombstone)
		}
	}
	m.rosterMu.Unlock()
	if prepareErr != nil {
		m.codec.RespondErrorData(msg.ID, codeInternalError, "prepare removal: "+prepareErr.Error(), nil)
		return
	}
	if m.testRemovalPrepared != nil {
		close(m.testRemovalPrepared)
		<-m.testRemovalContinue
	}

	// Notify first, while the peer's pin is still present to authenticate mTLS.
	if node.IPAddress != "" {
		m.notifyPeerRemoval(net.JoinHostPort(node.IPAddress, strconv.Itoa(node.Port)), node.NodeUUID, proof)
	}
	// De-pin and drop membership under the cluster-composition boundary so the
	// removal is atomic against a concurrent self-remove teardown — the two can't
	// interleave and leave a half-cleared roster. The network notify above stays
	// outside the lock (it must not stall other commits for a round trip), and
	// Remove is idempotent, so a teardown that cleared the pin first just makes
	// this a no-op.
	var rmErr error
	stale := false
	m.rosterMu.Lock()
	{
		cid, selfEpoch := m.currentAdmission()
		liveCID, _ := m.clusterIdentity()
		current, ok := m.memberByNodeID(node.NodeUUID)
		pin, pinned := m.trust.Get(node.NodeUUID)
		if cid != cid0 || selfEpoch != selfEpoch0 || liveCID != cid0 ||
			!ok || !pinned ||
			current.ClusterID != cid0 || pin.ClusterID != cid0 ||
			current.AdmissionEpoch != node.AdmissionEpoch || pin.AdmissionEpoch != node.AdmissionEpoch {
			stale = true
		} else if rmErr = m.trust.Remove(node.NodeUUID); rmErr == nil {
			m.removeMemberByUUID(node.NodeUUID)
		}
	}
	m.rosterMu.Unlock()
	if stale {
		m.codec.RespondErrorData(msg.ID, codeInternalError,
			"member admission changed while removal was in progress; current admission was preserved", nil)
		return
	}
	if rmErr != nil {
		m.codec.RespondErrorData(msg.ID, codeInternalError, "remove pin: "+rmErr.Error(), nil)
		return
	}
	m.emitNodesChanged()
	m.codec.Respond(msg.ID, map[string]any{"nodeId": node.ID, "removed": true})
	// Fan the tombstone out to the remaining members.
	go m.reconcileAllMembers()
}
