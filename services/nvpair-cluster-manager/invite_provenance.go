// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

const inviteCreatedClusterFile = "invite-created-cluster.json"

type inviteCreatedClusterMarker struct {
	ClusterID string `json:"clusterId"`
}

func (m *Manager) inviteCreatedClusterPath() string {
	return filepath.Join(m.clusterDir, inviteCreatedClusterFile)
}

// loadInviteCreatedCluster restores the durable provenance marker. The cluster
// identity itself is restored later by the Broker via cluster:set-identity; the
// marker id lets that path distinguish a matching invite-created cluster from
// an unrelated, intentionally restored identity.
func (m *Manager) loadInviteCreatedCluster() error {
	data, err := os.ReadFile(m.inviteCreatedClusterPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var marker inviteCreatedClusterMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return err
	}
	if marker.ClusterID == "" {
		return nil
	}
	m.clusterMu.Lock()
	m.inviteCreatedCluster = true
	m.inviteCreatedID = marker.ClusterID
	m.clusterMu.Unlock()
	return nil
}

func (m *Manager) isInviteCreatedCluster() bool {
	m.clusterMu.RLock()
	defer m.clusterMu.RUnlock()
	return m.inviteCreatedCluster && m.inviteCreatedID == m.clusterID && m.clusterID != ""
}

// setInviteCreatedCluster updates and durably persists whether the current
// cluster was auto-founded solely for invite pairing. Persistence closes the
// inviter-restart hole: after restart, restoring that same cluster id can clean
// up immediately because all in-memory pairing sessions are gone.
func (m *Manager) setInviteCreatedCluster(v bool) {
	if !v {
		if err := m.clearInviteCreatedCluster(); err != nil {
			log.Printf("clear invite-created cluster provenance: %v", err)
		}
		return
	}
	m.clusterMu.Lock()
	m.inviteCreatedCluster = true
	m.inviteCreatedID = m.clusterID
	id := m.inviteCreatedID
	m.clusterMu.Unlock()

	data, err := json.MarshalIndent(inviteCreatedClusterMarker{ClusterID: id}, "", "  ")
	if err != nil {
		log.Printf("persist invite-created cluster provenance: marshal: %v", err)
		return
	}
	if err := atomicWrite(m.inviteCreatedClusterPath(), data, 0o600); err != nil {
		log.Printf("persist invite-created cluster provenance: %v", err)
	}
}

func (m *Manager) clearInviteCreatedCluster() error {
	m.clusterMu.Lock()
	m.inviteCreatedCluster = false
	m.inviteCreatedID = ""
	m.clusterMu.Unlock()
	if err := os.Remove(m.inviteCreatedClusterPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// restoreInviteCreatedCluster reconciles a Broker-restored identity with the
// durable marker. A matching marker retains provenance; a different id is an
// intentional/restored cluster and clears stale provenance.
func (m *Manager) restoreInviteCreatedCluster(id string) bool {
	m.clusterMu.Lock()
	matches := id != "" && m.inviteCreatedCluster && m.inviteCreatedID == id
	m.clusterMu.Unlock()
	if !matches {
		m.setInviteCreatedCluster(false)
	}
	return matches
}
