// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func fabricSetupControlRequest(m *Manager) fabricControlRequest {
	facts := fabricServiceInventory(m.cableLocal.nodeID)
	target := fabricTarget{NodeID: facts.NodeID, Principal: m.mesh.NodeUUID(), Interfaces: []fabricInterface{facts.Interfaces[0].fabricInterface, facts.Interfaces[1].fabricInterface}}
	return fabricControlRequest{Method: "reserve", OperationID: strings.Repeat("a", 32), Target: target}
}

func TestFabricReservationObservesEveryDiagnosticSetupHold(t *testing.T) {
	for _, test := range []struct {
		name string
		hold func(*diagnosticService)
	}{
		{"package-active", func(d *diagnosticService) { d.packageActive = "package-fixture" }},
		{"runtime-active", func(d *diagnosticService) { d.runtimeActive = "runtime-fixture" }},
		{"package-recovery", func(d *diagnosticService) { d.packageRecoveryFailed = true }},
		{"runtime-recovery", func(d *diagnosticService) { d.runtimeRecoveryFailed = true }},
		{"shutdown", func(d *diagnosticService) { d.packageAdmissionClosed = true }},
		{"cancelled-context", func(d *diagnosticService) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			d.ctx = ctx
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, _, _ := cableTestManager(t, nil)
			s := m.exec.fabric
			request := fabricSetupControlRequest(m)
			test.hold(m.exec.diagnostics)
			if !s.otherBusy() {
				t.Fatal("fabric did not observe the diagnostic setup hold")
			}
			// Both methods must reject before managed-state, workload-response,
			// inventory or native I/O. Their shared early busy error proves this.
			for _, method := range []string{"reserve", "reserve-rollback"} {
				request.Method = method
				result, err := s.localControl(context.Background(), m.mesh.NodeUUID(), request)
				if err == nil || err.Error() != "diagnostic or cable operation has active or unconfirmed resources" || result.Reserved {
					t.Fatalf("setup hold did not reject %s before I/O: %+v %v", method, result, err)
				}
			}
			if s.reservation != nil {
				t.Fatal("held setup acquired a fabric reservation")
			}
			if _, err := os.Stat(s.reservationFile()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("held setup persisted fabric ownership: %v", err)
			}
		})
	}
}

func TestFabricSetupHoldStillAllowsExactReservationRelease(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	s := m.exec.fabric
	request := fabricSetupControlRequest(m)
	request.Method = "release"
	owner := m.mesh.NodeUUID()
	s.reservation = &fabricReservation{OperationID: request.OperationID, OwnerPrincipal: owner, Target: request.Target}
	m.exec.diagnostics.packageAdmissionClosed = true
	m.exec.diagnostics.packageRecoveryFailed = true
	if !s.otherBusy() || !s.held() {
		t.Fatal("fixture does not hold both admission paths")
	}
	if _, err := s.localControl(context.Background(), owner, request); err != nil || s.reservation != nil {
		t.Fatalf("setup admission blocked exact fabric release: %v", err)
	}
}

func TestFabricIdleSetupDoesNotAcquireAnAdmissionHold(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	s := m.exec.fabric
	s.runs["fabric-fixture"] = &fabricRunRecord{Public: fabricOperation{State: "active"}}
	if s.otherBusy() || s.held() {
		t.Fatal("idle setup and stable applied fabric fenced workload admission")
	}
}
