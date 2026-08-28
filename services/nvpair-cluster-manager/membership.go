// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log"
	"sort"
	"time"
)

// MembershipState is a node's standing in the local view of the cluster (§6).
type MembershipState string

const (
	stateMember          MembershipState = "member"
	statePendingOutbound MembershipState = "pending-outbound"
	statePendingInbound  MembershipState = "pending-inbound"
)

// InviteState is a pairing session's lifecycle state (§6).
type InviteState string

const (
	inviteStatePending  InviteState = "pending"
	inviteStatePaired   InviteState = "paired"
	inviteStateDeclined InviteState = "declined"
	inviteStateCanceled InviteState = "canceled"
	inviteStateExpired  InviteState = "expired"
	inviteStateFailed   InviteState = "failed"
	// inviteStateRejected means the peer explicitly refused the pairing (e.g. it
	// is already clustered), as opposed to a transport/protocol failure. No PIN
	// is minted. Distinguished from inviteStateFailed so a UI can say "already
	// paired / node is in a cluster" rather than "invite failed".
	inviteStateRejected InviteState = "rejected"
)

// reasonAlreadyClustered is the rejection reason a joiner returns when it
// refuses an inbound pairing because it already belongs to a cluster. It rides
// the pairing envelope back to the inviter and surfaces on the invite-node
// result's "reason" field.
const reasonAlreadyClustered = "already-clustered"

// reasonIncorrectPIN is the failure reason stamped on both sides when a pairing
// fails because the joiner entered the wrong PIN (an EAP-NOOB MAC verification
// failure). It surfaces on the Invite's "reason" field so a UI can show a
// specific "Incorrect PIN" message rather than a generic "Pairing failed". Other
// failed causes (peer unreachable, malformed) leave the reason empty.
const reasonIncorrectPIN = "incorrect-pin"

// ClusterNode is a cluster member or pending invitee (§6). The logical id is the
// membership key for display; nodeUuid is the cryptographic principal.
type ClusterNode struct {
	ID             string          `json:"id"`
	NodeUUID       string          `json:"nodeUuid"`
	Name           string          `json:"name"`
	IPAddress      string          `json:"ipAddress"`
	Port           int             `json:"port"`
	ClusterID      string          `json:"clusterId"`
	AdmissionEpoch uint64          `json:"admissionEpoch,omitempty"`
	State          MembershipState `json:"state"`
	JoinedAt       *int64          `json:"joinedAt"`
	LastSeen       *int64          `json:"lastSeen"`
}

// Invite is the Broker-facing view of a pairing session (§6). Certificates are
// not carried here; they travel inside the EAP-NOOB transcript.
type Invite struct {
	InviteID            string      `json:"inviteId"`
	FromNodeID          string      `json:"fromNodeId"`
	FromNodeUUID        string      `json:"fromNodeUuid"`
	FromNodeName        string      `json:"fromNodeName"`
	ToNodeID            *string     `json:"toNodeId"`
	ClusterID           string      `json:"clusterId"`
	ClusterFriendlyName string      `json:"clusterFriendlyName"`
	Pin                 *string     `json:"pin"`
	State               InviteState `json:"state"`
	// Reason is an optional machine-readable qualifier for a terminal state
	// (e.g. "incorrect-pin" on a wrong-PIN failure). Empty when no specific
	// reason applies, so consumers fall back to generic copy.
	Reason      string `json:"reason,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	RespondedAt *int64 `json:"respondedAt"`
}

func cloneClusterNode(node *ClusterNode) *ClusterNode {
	if node == nil {
		return nil
	}
	cp := *node
	if node.JoinedAt != nil {
		v := *node.JoinedAt
		cp.JoinedAt = &v
	}
	if node.LastSeen != nil {
		v := *node.LastSeen
		cp.LastSeen = &v
	}
	return &cp
}

func cloneInvite(inv *Invite) *Invite {
	if inv == nil {
		return nil
	}
	cp := *inv
	if inv.ToNodeID != nil {
		v := *inv.ToNodeID
		cp.ToNodeID = &v
	}
	if inv.Pin != nil {
		v := *inv.Pin
		cp.Pin = &v
	}
	if inv.RespondedAt != nil {
		v := *inv.RespondedAt
		cp.RespondedAt = &v
	}
	return &cp
}

// upsertMember inserts or replaces a member, keyed by nodeUuid.
func (m *Manager) upsertMember(node *ClusterNode) {
	m.memMu.Lock()
	m.members[node.NodeUUID] = cloneClusterNode(node)
	m.memMu.Unlock()
	m.clusterGen.Add(1)
}

// removeMemberByUUID drops a member; returns whether one was present.
func (m *Manager) removeMemberByUUID(uuid string) bool {
	m.memMu.Lock()
	_, ok := m.members[uuid]
	delete(m.members, uuid)
	m.memMu.Unlock()
	if ok {
		m.clusterGen.Add(1)
	}
	return ok
}

// resetMembership clears the entire member set (used when leaving a cluster).
func (m *Manager) resetMembership() {
	m.memMu.Lock()
	m.members = make(map[string]*ClusterNode)
	m.memMu.Unlock()
	m.clusterGen.Add(1)
}

// resetInvites abandons every in-flight (pending) Broker-facing invite so a
// pairing that was still mid-flight cannot outlive the cluster it was scoped to
// (invites share memMu with the member set). Terminal records
// (declined/expired/failed/paired) are kept: they are observable history rather
// than live pairings, and the invite-created-cluster cleanup flips an invite to
// a terminal state and *then* dissolves the throwaway cluster through this same
// teardown, so that just-finished record must survive.
func (m *Manager) resetInvites() {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	for id, inv := range m.invites {
		if inv.State == inviteStatePending {
			delete(m.invites, id)
		}
	}
}

// memberByNodeID resolves a member by either its logical nodeId or its nodeUuid.
func (m *Manager) memberByNodeID(idOrUUID string) (*ClusterNode, bool) {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	if n, ok := m.members[idOrUUID]; ok {
		return cloneClusterNode(n), true
	}
	for _, n := range m.members {
		if n.ID == idOrUUID {
			return cloneClusterNode(n), true
		}
	}
	return nil, false
}

// setMemberAddr updates a member's reachable host:port under the membership
// lock, returning whether anything actually changed. A zero/invalid port keeps
// the member's existing listening port (falling back to the local default), so
// callers learning only a host (e.g. a TCP source IP, whose port is ephemeral)
// don't clobber the real listening port. Callers persist and emit
// nodes:changed when this returns true.
func (m *Manager) setMemberAddr(uuid, host string, port int) bool {
	if host == "" {
		return false
	}
	m.memMu.Lock()
	defer m.memMu.Unlock()
	n, ok := m.members[uuid]
	if !ok {
		return false
	}
	if port < 1 || port > 65535 {
		port = n.Port
	}
	if port < 1 || port > 65535 {
		port = m.port
	}
	if n.IPAddress == host && n.Port == port {
		return false
	}
	n.IPAddress = host
	n.Port = port
	return true
}

// noteObservedPeerHost records the source host an authenticated peer connected
// from and applies it as that member's address, returning whether anything
// changed.
//
// An address observed on a connection is the only claim about a peer's
// reachability this node has verified: the peer reached us from it, over a
// transport pinned to its certificate. A peer's own advertised address — or worse,
// a third peer's relayed copy of it — is that peer guessing which of its addresses
// works from here, which is exactly the guess that goes wrong on a multi-homed
// host. So the observation is remembered, and refreshMemberAddr will not overwrite
// it with a claim.
func (m *Manager) noteObservedPeerHost(uuid, host string) bool {
	if uuid == "" || host == "" {
		return false
	}
	m.observedMu.Lock()
	if m.observedPeerHosts == nil {
		m.observedPeerHosts = make(map[string]string)
	}
	m.observedPeerHosts[uuid] = host
	m.observedMu.Unlock()
	return m.setMemberAddr(uuid, host, 0)
}

// observedPeerHost returns the source host this peer was last seen connecting
// from, or "" if it never has been.
func (m *Manager) observedPeerHost(uuid string) string {
	m.observedMu.Lock()
	defer m.observedMu.Unlock()
	return m.observedPeerHosts[uuid]
}

// updateMemberIdentity refreshes a member's display fields (id/name) in place,
// keyed by its stable nodeUuid. Empty inputs are ignored so a partial update
// can't blank a known value. Returns whether anything changed. Display-only: the
// map key (nodeUuid) is untouched, so a peer's PC rename updates the shown name
// rather than creating a duplicate member after a host rename. Callers persist and emit
// nodes:changed when this returns true.
func (m *Manager) updateMemberIdentity(uuid, nodeID, name string) bool {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	n, ok := m.members[uuid]
	if !ok {
		return false
	}
	changed := false
	if nodeID != "" && n.ID != nodeID {
		n.ID = nodeID
		changed = true
	}
	if name != "" && n.Name != name {
		n.Name = name
		changed = true
	}
	return changed
}

// refreshSelfMemberIdentity updates this node's own member record (if it is
// clustered) to the current hostname-derived display fields after a restart, so
// a PC rename doesn't leave a stale self entry lingering in the roster under the
// old name — a ghost the local instance couldn't remove. Keyed by the
// stable nodeUuid; only the display id/name change. Persists when it changed.
func (m *Manager) refreshSelfMemberIdentity() {
	if m.updateMemberIdentity(m.identity.NodeUUID, m.identity.NodeID, m.identity.Name) {
		m.persistMembers()
	}
}

// snapshotNodes returns a stable, id-sorted copy of the member set.
func (m *Manager) snapshotNodes() []ClusterNode {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	out := make([]ClusterNode, 0, len(m.members))
	for _, n := range m.members {
		out = append(out, *cloneClusterNode(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// putInvite records (or replaces) a pairing session's Broker-facing view.
func (m *Manager) putInvite(inv *Invite) {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	m.invites[inv.InviteID] = cloneInvite(inv)
}

// getInvite returns a copy of the invite for an id.
func (m *Manager) getInvite(inviteID string) (*Invite, bool) {
	m.memMu.Lock()
	defer m.memMu.Unlock()
	inv, ok := m.invites[inviteID]
	if !ok {
		return nil, false
	}
	return cloneInvite(inv), true
}

// addSelfMember records this node as a member of its current cluster.
func (m *Manager) addSelfMember() {
	id, _ := m.clusterIdentity()
	admissionID, epoch := m.currentAdmission()
	if admissionID != id {
		epoch = 0
	}
	m.addSelfMemberForAdmission(id, epoch)
}

func (m *Manager) addSelfMemberForAdmission(clusterID string, epoch uint64) {
	now := time.Now().UnixMilli()
	m.upsertMember(&ClusterNode{
		ID:             m.identity.NodeID,
		NodeUUID:       m.identity.NodeUUID,
		Name:           m.identity.Name,
		IPAddress:      "127.0.0.1",
		Port:           m.port,
		ClusterID:      clusterID,
		AdmissionEpoch: epoch,
		State:          stateMember,
		JoinedAt:       &now,
	})
}

// handleGetInitial returns the current membership set (§7.0). An empty list is a
// normal early state, not an error.
func (m *Manager) handleGetInitial(msg *Message) {
	m.codec.Respond(msg.ID, map[string]any{"nodes": m.snapshotNodes()})
}

// emitNodesChanged persists the confirmed roster and pushes the full membership
// snapshot to the Broker (§7.0).
func (m *Manager) emitNodesChanged() {
	m.persistMembers()
	if err := m.codec.Notify("nodes:changed", map[string]any{"nodes": m.snapshotNodes()}); err != nil {
		log.Printf("emit nodes:changed: %v", err)
	}
}

// announceTrustChanged tells the rest of this node that the pin set moved. It is
// the TrustStore's change hook, so it necessarily runs after the pin is durable:
// a consumer that re-reads the cluster dir on receipt cannot observe a directory
// older than the event, which is the ordering guarantee that makes a
// notification usable here at all.
//
// The payload is deliberately empty. Recipients re-derive from disk rather than
// trusting a diff carried on the wire, so a message that arrives out of order,
// twice, or coalesced with another is harmless.
func (m *Manager) announceTrustChanged() {
	m.mesh.Refresh()
	m.clients.DropUnpinned()
	if err := m.codec.Notify("cluster:trust-changed", struct{}{}); err != nil {
		log.Printf("emit cluster:trust-changed: %v", err)
	}
}
