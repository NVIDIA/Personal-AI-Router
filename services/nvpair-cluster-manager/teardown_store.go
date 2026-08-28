// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const teardownMarkerFile = "teardown.pending"

type teardownMarker struct {
	ClusterID      string `json:"clusterId"`
	AdmissionEpoch uint64 `json:"admissionEpoch"`
	StartedAt      int64  `json:"startedAt"`
}

func (m *Manager) teardownMarkerPath() string {
	return filepath.Join(m.clusterDir, teardownMarkerFile)
}

func (m *Manager) beginDurableTeardown() error {
	cid, epoch := m.currentAdmission()
	data, err := json.Marshal(teardownMarker{
		ClusterID: cid, AdmissionEpoch: epoch, StartedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		return err
	}
	m.teardownPending.Store(true)
	if err := atomicWrite(m.teardownMarkerPath(), data, 0o600); err != nil {
		m.teardownPending.Store(false)
		return err
	}
	return nil
}

func (m *Manager) pendingTeardown() (bool, error) {
	_, err := os.Stat(m.teardownMarkerPath())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// finishDurableTeardown is idempotent. It clears every durable authorization
// surface before removing the journal marker. Any failure leaves the marker in
// place so startup retries instead of serving resurrected cluster state.
func (m *Manager) finishDurableTeardown() error {
	m.teardownPending.Store(true)
	var errs []error
	if err := m.clearAdmission(); err != nil {
		errs = append(errs, fmt.Errorf("clear admission: %w", err))
	}
	for _, pin := range m.trust.List() {
		if err := m.trust.Remove(pin.NodeUUID); err != nil {
			errs = append(errs, err)
			m.trust.Forget(pin.NodeUUID)
		}
	}
	m.resetMembership()
	if err := m.persistMembersErr(); err != nil {
		errs = append(errs, fmt.Errorf("clear members: %w", err))
	}
	if err := m.clearAllTombstones(); err != nil {
		errs = append(errs, fmt.Errorf("clear tombstones: %w", err))
	}
	if err := m.clearAllRemovalProofs(); err != nil {
		errs = append(errs, fmt.Errorf("clear removal proofs: %w", err))
	}
	if err := m.clearInviteCreatedCluster(); err != nil {
		errs = append(errs, fmt.Errorf("clear invite provenance: %w", err))
	}

	// In-memory teardown always completes, even when a durable operation needs a
	// restart retry. The marker prevents this process or the next one from
	// accepting stale settings as a new cluster admission.
	m.setClusterIdentity("", "")
	m.resetSessions()
	m.resetInvites()
	m.clusterTornDown = true
	m.mesh.Refresh()
	m.clients.CloseIdle()

	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := os.Remove(m.teardownMarkerPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear teardown marker: %w", err)
	}
	m.teardownPending.Store(false)
	return nil
}

func (m *Manager) recoverPendingTeardown() error {
	pending, err := m.pendingTeardown()
	if err != nil || !pending {
		return err
	}
	m.teardownPending.Store(true)
	return m.finishDurableTeardown()
}
