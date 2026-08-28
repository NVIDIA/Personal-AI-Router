// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
)

// handleGetNodeID returns this node's own identity (§7.0). Always available —
// the identity is minted at startup.
func (m *Manager) handleGetNodeID(msg *Message) {
	id, _ := m.clusterIdentity()
	m.codec.Respond(msg.ID, map[string]any{
		"nodeUuid":        m.identity.NodeUUID,
		"nodeId":          m.identity.NodeID,
		"name":            m.identity.Name,
		"certFingerprint": m.identity.CertFingerprint,
		"clusterId":       id,
	})
}

type setIdentityParams struct {
	ClusterID           *string `json:"clusterId"`
	ClusterFriendlyName *string `json:"clusterFriendlyName"`
}

// handleSetIdentity reflects an existing cluster identity supplied from
// nvpair-node-settings (§7.0). It never touches nodeUuid or membership and never
// emits cluster:identity-changed (the value already came from node-settings).
func (m *Manager) handleSetIdentity(msg *Message) {
	var p setIdentityParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			m.codec.RespondErrorData(msg.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
			return
		}
	}
	id, friendly := m.clusterIdentity()
	if p.ClusterID != nil {
		id = *p.ClusterID
	}
	if p.ClusterFriendlyName != nil {
		friendly = *p.ClusterFriendlyName
	}
	restored := false
	var admissionErr error
	m.withClusterComposition(func() {
		// Teardown boundary for identity restore. Once a teardown has unclustered
		// this node in this process, refuse to restore a *different* nonempty id:
		// a set-identity queued behind that teardown carries the now-stale
		// persisted value and would otherwise resurrect a members-less "zombie"
		// cluster. A clean startup restore (no teardown yet) and a no-op re-assert
		// of the id we already hold both still apply; re-clustering after a
		// teardown happens through pairing/create, which set the identity directly.
		cur, _ := m.clusterIdentity()
		if id != "" && id != cur && m.clusterTornDown {
			return
		}
		if id == "" && cur != "" {
			// A committed active admission is the local durable authority. An
			// empty/stale settings reflection cannot silently clear identity
			// while pins and membership remain; cluster:leave owns teardown.
			id = cur
		}
		if id != "" {
			var epoch uint64
			if epoch, admissionErr = m.ensureAdmission(id); admissionErr != nil {
				return
			}
			if admissionErr = m.migrateLegacyAdmissions(id, epoch); admissionErr != nil {
				return
			}
			if admissionErr = m.pruneUnauthenticatedMembers(id, epoch); admissionErr != nil {
				return
			}
		}
		m.setClusterIdentity(id, friendly)
		restored = true
	})
	if admissionErr != nil {
		m.codec.RespondErrorData(msg.ID, codeInternalError, "restore admission: "+admissionErr.Error(), nil)
		return
	}
	if !restored {
		reqID := ""
		if p.ClusterID != nil {
			reqID = *p.ClusterID
		}
		log.Printf("set-identity: ignoring stale restore of %q after this node was unclustered by a teardown; staying unclustered", reqID)
		id, friendly = m.clusterIdentity()
	} else if m.restoreInviteCreatedCluster(id) {
		// The process restarted while an invite-created cluster still had no
		// confirmed peer. Pairing sessions are in-memory and therefore gone, so
		// no invite can complete; clean up the orphan immediately.
		m.maybeLeaveInviteCreatedCluster()
		id, friendly = m.clusterIdentity()
	}
	m.codec.Respond(msg.ID, map[string]any{
		"clusterId":           id,
		"clusterFriendlyName": friendly,
	})
}

type createParams struct {
	ClusterFriendlyName *string `json:"clusterFriendlyName"`
}

// handleCreate founds a brand-new cluster of one (§7.0). Valid only when
// unclustered; always mints a fresh, globally-unique UUID v4 clusterId and emits
// cluster:identity-changed so the Broker persists it.
func (m *Manager) handleCreate(msg *Message) {
	var p createParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			m.codec.RespondErrorData(msg.ID, codeInvalidParams, "invalid params: "+err.Error(), nil)
			return
		}
	}
	if id, _ := m.clusterIdentity(); id != "" {
		m.codec.RespondErrorData(msg.ID, codePrecondition, "node is already clustered",
			map[string]string{"reason": "already-clustered"})
		return
	}
	friendly := ""
	if p.ClusterFriendlyName != nil {
		friendly = *p.ClusterFriendlyName
	}
	newID, name, err := m.foundCluster(friendly)
	if err != nil {
		m.codec.RespondErrorData(msg.ID, codeInternalError, "mint cluster id: "+err.Error(), nil)
		return
	}
	// Explicit cluster:create is intentional and must survive a declined invite.
	m.setInviteCreatedCluster(false)
	m.codec.Respond(msg.ID, map[string]any{
		"clusterId":           newID,
		"clusterFriendlyName": name,
	})
}

// foundCluster mints a brand-new cluster of one: a fresh, globally-unique
// clusterId (UUID v4), records this node as the founding member, and emits
// cluster:identity-changed so the Broker persists it. friendly is the desired
// cluster friendly name; an empty value defaults to this node's name. The caller
// must have already established that the node is unclustered. Shared by
// cluster:create (§7.0) and the auto-found path of cluster:invite-node (§7.2).
//
// The identity and self-member commit run together under the composition
// boundary (rosterMu) so founding is atomic against a self-remove teardown — a
// rejoin racing the "did everyone reject me?" check either commits fully before
// it or blocks until teardown finishes, never interleaving to leave a
// half-founded cluster.
func (m *Manager) foundCluster(friendly string) (id, name string, err error) {
	newID, err := newUUIDv4()
	if err != nil {
		return "", "", err
	}
	epoch, err := m.reserveAdmissionEpoch()
	if err != nil {
		return "", "", fmt.Errorf("reserve admission: %w", err)
	}
	name = friendly
	if name == "" {
		name = m.identity.Name
	}
	var admissionErr error
	m.withClusterComposition(func() {
		if m.teardownPending.Load() {
			admissionErr = fmt.Errorf("cluster teardown is pending")
			return
		}
		m.setClusterIdentity(newID, name)
		m.addSelfMemberForAdmission(newID, epoch)
		if admissionErr = m.persistMembersErr(); admissionErr != nil {
			m.removeMemberByUUID(m.identity.NodeUUID)
			m.setClusterIdentity("", "")
			return
		}
		if admissionErr = m.activateAdmission(newID, epoch); admissionErr != nil {
			m.removeMemberByUUID(m.identity.NodeUUID)
			rollbackErr := m.persistMembersErr()
			m.setClusterIdentity("", "")
			admissionErr = errors.Join(admissionErr, rollbackErr)
		}
	})
	if admissionErr != nil {
		return "", "", fmt.Errorf("commit cluster admission: %w", admissionErr)
	}
	m.emitNodesChanged()
	m.emitIdentityChanged()
	return newID, name, nil
}
