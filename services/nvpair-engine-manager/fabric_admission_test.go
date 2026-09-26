// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFabricReservationUsesCanonicalDiagnosticAdmission(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	facts := fabricServiceInventory(m.cableLocal.nodeID)
	facts.Principal = m.mesh.NodeUUID()
	target := fabricTarget{NodeID: facts.NodeID, Principal: facts.Principal, Interfaces: []fabricInterface{facts.Interfaces[0].fabricInterface, facts.Interfaces[1].fabricInterface}}
	request := fabricControlRequest{Method: "reserve", OperationID: strings.Repeat("a", 32), Target: target}
	m.exec.diagnosticMu.Lock()
	finished := make(chan error, 1)
	go func() {
		_, err := m.exec.fabric.localControl(context.Background(), m.mesh.NodeUUID(), request)
		finished <- err
	}()
	select {
	case <-finished:
		t.Fatal("fabric admission bypassed canonical diagnostic lock")
	case <-time.After(20 * time.Millisecond):
	}
	// Publish the competing diagnostic while its canonical guard is held. The
	// waiting fabric operation must observe it before native inventory or effects.
	m.exec.diagnostics.mu.Lock()
	m.exec.diagnostics.cancels["diagnostic-in-flight"] = func() {}
	m.exec.diagnostics.mu.Unlock()
	m.exec.diagnosticMu.Unlock()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("fabric reserve ignored competing diagnostic")
		}
	case <-time.After(time.Second):
		t.Fatal("fabric admission did not settle")
	}
	if m.exec.fabric.reservation != nil {
		t.Fatal("busy diagnostic acquired a fabric reservation")
	}
}

func TestFabricMutationFencesDiagnosticButStableConfigurationDoesNot(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	id := strings.Repeat("a", 32)
	m.exec.fabric.runs[id] = &fabricRunRecord{Public: fabricOperation{State: "applying"}}
	if _, err := m.exec.diagnostics.start(diagnosticProfile{}); err == nil {
		t.Fatal("diagnostic start bypassed fabric mutation hold")
	}
	if !m.exec.diagnostics.reserved() {
		t.Fatal("managed lifecycle did not see fabric mutation hold")
	}
	m.exec.fabric.runs[id].Public.State = "active"
	if m.exec.diagnostics.reserved() {
		t.Fatal("stable applied network fenced managed workloads")
	}
	m.exec.fabric.runs[id].Public.State = "recovery-required"
	if !m.exec.diagnostics.reserved() {
		t.Fatal("unknown cleanup released shared admission")
	}
}

func fabricAdmissionTestTarget(m *Manager) fabricTarget {
	facts := fabricServiceInventory(m.cableLocal.nodeID)
	facts.Principal = m.mesh.NodeUUID()
	return fabricTarget{NodeID: facts.NodeID, Principal: facts.Principal, Interfaces: []fabricInterface{facts.Interfaces[0].fabricInterface, facts.Interfaces[1].fabricInterface}}
}

// An engine action holds admission for its whole run; a model pull can hold it
// for the full action timeout. Re-proof and release must not wait behind it.
func TestFabricReadOnlyControlDoesNotWaitForEngineAction(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	target := fabricAdmissionTestTarget(m)
	m.exec.diagnosticMu.RLock()
	defer m.exec.diagnosticMu.RUnlock()
	for _, method := range []string{"requalify", "release"} {
		finished := make(chan error, 1)
		go func() {
			_, err := m.exec.fabric.localControl(context.Background(), m.mesh.NodeUUID(), fabricControlRequest{Method: method, OperationID: strings.Repeat("a", 32), Target: target})
			finished <- err
		}()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatalf("%s waited for an in-flight engine action", method)
		}
	}
}

// A reservation that cannot be admitted yet must not join the writer queue:
// that would stall every later engine start behind the in-flight action and
// still publish the hold after its caller gave up.
func TestFabricReservationDoesNotQueueBehindEngineAction(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	target := fabricAdmissionTestTarget(m)
	m.exec.diagnosticMu.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := m.exec.fabric.localControl(ctx, m.mesh.NodeUUID(), fabricControlRequest{Method: "reserve", OperationID: strings.Repeat("a", 32), Target: target})
		finished <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if !m.exec.diagnosticMu.TryRLock() {
		m.exec.diagnosticMu.RUnlock()
		t.Fatal("a waiting fabric reservation blocked later engine actions")
	}
	m.exec.diagnosticMu.RUnlock()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("reservation was admitted during an in-flight engine action")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reservation outlived its caller")
	}
	m.exec.diagnosticMu.RUnlock()
	time.Sleep(50 * time.Millisecond)
	if m.exec.fabric.reservation != nil {
		t.Fatal("an abandoned reservation published its hold")
	}
}

func TestFabricAdmissionRefusesBusyParticipantWithinItsBound(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	m.exec.diagnosticMu.RLock()
	defer m.exec.diagnosticMu.RUnlock()
	started := time.Now()
	if err := m.exec.fabric.lockAdmission(context.Background(), 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy admission was not refused: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("bounded admission wait overran")
	}
}
