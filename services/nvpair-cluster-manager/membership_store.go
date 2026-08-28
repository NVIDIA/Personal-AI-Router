// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
)

// membersFile is the durable, lean roster persisted under the cluster dir. Only
// confirmed members (state == member) are written; transient pending invites
// live in memory only and are resurrected by re-pairing.
const membersFile = "members.json"

func (m *Manager) membersPath() string {
	return filepath.Join(m.clusterDir, membersFile)
}

// persistMembers atomically writes the confirmed member roster.
func (m *Manager) persistMembers() {
	if err := m.persistMembersErr(); err != nil {
		log.Printf("persist members: %v", err)
	}
}

func (m *Manager) persistMembersErr() error {
	m.memMu.Lock()
	durable := make([]ClusterNode, 0, len(m.members))
	for _, n := range m.members {
		if n.State == stateMember {
			durable = append(durable, *cloneClusterNode(n))
		}
	}
	m.memMu.Unlock()
	sort.Slice(durable, func(i, j int) bool { return durable[i].ID < durable[j].ID })

	data, err := json.MarshalIndent(durable, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.membersPath(), data, 0o600)
}

// loadMembers restores the confirmed member roster from disk on startup. A
// missing file is the normal first-run / unclustered state.
func (m *Manager) loadMembers() error {
	data, err := os.ReadFile(m.membersPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var nodes []ClusterNode
	if err := json.Unmarshal(data, &nodes); err != nil {
		return err
	}
	m.memMu.Lock()
	defer m.memMu.Unlock()
	for i := range nodes {
		n := nodes[i]
		m.members[n.NodeUUID] = &n
	}
	return nil
}

// rollbackIncompleteAdmission removes a crash-left provisional transaction.
// New create/join commits persist a nonzero self member and any peer pin before
// activating admission.json. Legacy pre-v2 state has no such self epoch, so the
// marker is unambiguous and can be failed closed without discarding migratable
// legacy pins.
func (m *Manager) rollbackIncompleteAdmission() error {
	if cid, epoch := m.currentAdmission(); cid != "" || epoch != 0 {
		return nil
	}
	self, ok := m.memberByNodeID(m.identity.NodeUUID)
	if !ok || self.ClusterID == "" || self.AdmissionEpoch == 0 {
		return nil
	}
	var errs []error
	for _, pin := range m.trust.List() {
		if err := m.trust.Remove(pin.NodeUUID); err != nil {
			errs = append(errs, err)
			m.trust.Forget(pin.NodeUUID)
		}
	}
	m.resetMembership()
	if err := m.persistMembersErr(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// migrateLegacyAdmissions upgrades pre-v2 epoch-zero pins and members after
// this node has an active admission. Peer epoch 1 is authenticated by the
// corresponding durable pin; self uses its node-global active epoch.
func (m *Manager) migrateLegacyAdmissions(clusterID string, selfEpoch uint64) error {
	if clusterID == "" || selfEpoch == 0 {
		return nil
	}
	if err := m.trust.MigrateLegacyAdmissions(clusterID); err != nil {
		return err
	}
	peerEpochs := make(map[string]uint64)
	for _, pin := range m.trust.List() {
		if pin.ClusterID == clusterID {
			peerEpochs[pin.NodeUUID] = pin.AdmissionEpoch
			hasLocalV2 := false
			for _, end := range pin.Endorsements {
				if end.By == m.identity.NodeUUID && end.AdmissionEpoch == pin.AdmissionEpoch &&
					end.ByAdmissionEpoch == selfEpoch &&
					verifyEndorsement(m.selfPub(), pin.NodeUUID, pin.CertFingerprint, clusterID, end) {
					hasLocalV2 = true
					break
				}
			}
			if !hasLocalV2 {
				end := m.endorsePeer(pin.NodeUUID, pin.CertFingerprint, pin.AdmissionEpoch)
				if err := m.trust.AddEndorsements(pin.NodeUUID, []Endorsement{end}); err != nil {
					return fmt.Errorf("endorse migrated pin %s: %w", pin.NodeUUID, err)
				}
			}
		}
	}

	changed := false
	m.memMu.Lock()
	for _, n := range m.members {
		if n.ClusterID != "" && n.ClusterID != clusterID {
			continue
		}
		if n.NodeUUID == m.identity.NodeUUID {
			if n.ClusterID != clusterID || n.AdmissionEpoch != selfEpoch {
				n.ClusterID = clusterID
				n.AdmissionEpoch = selfEpoch
				changed = true
			}
			continue
		}
		if epoch := peerEpochs[n.NodeUUID]; epoch != 0 {
			if n.ClusterID != clusterID || n.AdmissionEpoch != epoch {
				n.ClusterID = clusterID
				n.AdmissionEpoch = epoch
				changed = true
			}
		}
	}
	m.memMu.Unlock()
	if changed {
		return m.persistMembersErr()
	}
	return nil
}

// pruneUnauthenticatedMembers removes crash leftovers that have no matching
// durable pin. Pairing persists the member before granting mTLS authority, so a
// crash between those writes is fail-closed and is repaired here.
func (m *Manager) pruneUnauthenticatedMembers(clusterID string, selfEpoch uint64) error {
	pins := make(map[string]*TrustedPin)
	for _, pin := range m.trust.List() {
		pins[pin.NodeUUID] = pin
	}
	changed := false
	m.memMu.Lock()
	for uuid, n := range m.members {
		if uuid == m.identity.NodeUUID {
			if n.ClusterID != clusterID || n.AdmissionEpoch != selfEpoch {
				delete(m.members, uuid)
				changed = true
			}
			continue
		}
		pin, ok := pins[uuid]
		if !ok || n.ClusterID != clusterID || pin.ClusterID != clusterID ||
			n.AdmissionEpoch == 0 || n.AdmissionEpoch != pin.AdmissionEpoch {
			delete(m.members, uuid)
			changed = true
		}
	}
	m.memMu.Unlock()
	for uuid, pin := range pins {
		member, ok := m.memberByNodeID(uuid)
		if ok && member.ClusterID == clusterID && member.AdmissionEpoch != 0 &&
			member.AdmissionEpoch == pin.AdmissionEpoch {
			continue
		}
		if err := m.trust.Remove(uuid); err != nil {
			return fmt.Errorf("prune unauthenticated pin %s: %w", uuid, err)
		}
	}
	if changed {
		m.clusterGen.Add(1)
		return m.persistMembersErr()
	}
	return nil
}
