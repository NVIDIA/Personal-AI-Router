// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// admissionFile stores this node's monotonic cluster-admission counter and its
// currently-active admission. The counter never moves backwards or resets on
// teardown, so a later admission to the same cluster is distinguishable from an
// earlier incarnation across process restarts.
const admissionFile = "admission.json"

type admissionState struct {
	Counter   uint64 `json:"counter"`
	Activated uint64 `json:"activated,omitempty"`
	Retired   bool   `json:"retired,omitempty"`
	ClusterID string `json:"clusterId,omitempty"`
	Epoch     uint64 `json:"epoch,omitempty"`
}

func (m *Manager) admissionPath() string {
	return filepath.Join(m.clusterDir, admissionFile)
}

func (m *Manager) loadAdmission() error {
	data, err := os.ReadFile(m.admissionPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state admissionState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Activated == 0 && state.Epoch != 0 {
		state.Activated = state.Epoch
	}
	if state.Epoch > state.Counter {
		return fmt.Errorf("active admission epoch %d exceeds counter %d", state.Epoch, state.Counter)
	}
	if state.Activated > state.Counter || state.Epoch > state.Activated {
		return fmt.Errorf("admission high-water marks are inconsistent")
	}
	if (state.ClusterID == "") != (state.Epoch == 0) {
		return fmt.Errorf("active admission must carry both clusterId and epoch")
	}
	m.admissionMu.Lock()
	m.admissionCounter = state.Counter
	m.admissionActivated = state.Activated
	m.admissionRetired = state.Retired
	m.admissionClusterID = state.ClusterID
	m.admissionEpoch = state.Epoch
	m.admissionMu.Unlock()
	return nil
}

// persistAdmissionLocked atomically writes the admission state. Caller holds
// admissionMu.
func (m *Manager) persistAdmissionLocked() error {
	state := admissionState{
		Counter:   m.admissionCounter,
		Activated: m.admissionActivated,
		Retired:   m.admissionRetired,
		ClusterID: m.admissionClusterID,
		Epoch:     m.admissionEpoch,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.admissionPath(), data, 0o600)
}

// reserveAdmissionEpoch durably consumes and returns the next node-global
// admission epoch. Failed/declined pairings may leave gaps; monotonicity, not
// density, is the security property.
func (m *Manager) reserveAdmissionEpoch() (uint64, error) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	old := m.admissionCounter
	m.admissionCounter++
	if err := m.persistAdmissionLocked(); err != nil {
		m.admissionCounter = old
		return 0, err
	}
	return m.admissionCounter, nil
}

// activateAdmission durably marks epoch as this node's current admission to
// clusterID. epoch must already have been reserved.
func (m *Manager) activateAdmission(clusterID string, epoch uint64) error {
	if clusterID == "" || epoch == 0 {
		return fmt.Errorf("activate admission requires nonempty clusterId and epoch")
	}
	if m.teardownPending.Load() {
		return fmt.Errorf("cluster teardown is pending")
	}
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	if epoch > m.admissionCounter {
		return fmt.Errorf("admission epoch %d was not reserved (counter %d)", epoch, m.admissionCounter)
	}
	if epoch <= m.admissionActivated {
		return fmt.Errorf("admission epoch %d is stale (last activated %d)", epoch, m.admissionActivated)
	}
	oldActivated := m.admissionActivated
	oldRetired := m.admissionRetired
	oldCluster, oldEpoch := m.admissionClusterID, m.admissionEpoch
	m.admissionActivated = epoch
	m.admissionRetired = false
	m.admissionClusterID, m.admissionEpoch = clusterID, epoch
	if err := m.persistAdmissionLocked(); err != nil {
		m.admissionActivated = oldActivated
		m.admissionRetired = oldRetired
		m.admissionClusterID, m.admissionEpoch = oldCluster, oldEpoch
		return err
	}
	return nil
}

// ensureAdmission returns the durable current admission for clusterID, minting
// one only for a first upgrade/startup restore that has no admission record yet.
// It never increments a matching admission on restart.
func (m *Manager) ensureAdmission(clusterID string) (uint64, error) {
	m.admissionMu.Lock()
	if m.admissionClusterID == clusterID && m.admissionEpoch != 0 {
		epoch := m.admissionEpoch
		m.admissionMu.Unlock()
		return epoch, nil
	}
	if m.admissionClusterID != "" || m.admissionEpoch != 0 {
		activeCluster := m.admissionClusterID
		m.admissionMu.Unlock()
		return 0, fmt.Errorf("active admission belongs to cluster %q", activeCluster)
	}
	if m.admissionRetired {
		m.admissionMu.Unlock()
		return 0, fmt.Errorf("cluster identity was durably retired; rejoin or create a new cluster")
	}
	m.admissionMu.Unlock()

	epoch, err := m.reserveAdmissionEpoch()
	if err != nil {
		return 0, err
	}
	if err := m.activateAdmission(clusterID, epoch); err != nil {
		return 0, err
	}
	return epoch, nil
}

func (m *Manager) currentAdmission() (clusterID string, epoch uint64) {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	return m.admissionClusterID, m.admissionEpoch
}

func (m *Manager) admissionWasRetired() bool {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	return m.admissionRetired
}

// clearAdmission durably ends the current admission without resetting the
// monotonic counter.
func (m *Manager) clearAdmission() error {
	m.admissionMu.Lock()
	defer m.admissionMu.Unlock()
	oldRetired := m.admissionRetired
	oldCluster, oldEpoch := m.admissionClusterID, m.admissionEpoch
	m.admissionRetired = true
	m.admissionClusterID, m.admissionEpoch = "", 0
	if err := m.persistAdmissionLocked(); err != nil {
		m.admissionRetired = oldRetired
		m.admissionClusterID, m.admissionEpoch = oldCluster, oldEpoch
		return err
	}
	return nil
}
