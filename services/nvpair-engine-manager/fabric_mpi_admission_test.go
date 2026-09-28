// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// Exercise real admission with valid local trust bindings, stopping before any
// participant transport or native work. The older transport fixture deliberately
// has stale pins, which would otherwise reject before the fabric guard.
func fabricMPIAdmissionFixture(t *testing.T) (*diagnosticService, diagnosticProfile, json.RawMessage, diagnosticParticipantRequest) {
	t.Helper()
	d, p, raw, request := mpiTransportFixture(t)
	for i := range p.Members {
		pin, ok := d.m.mesh.PinSHA256(p.Members[i].Principal)
		if !ok {
			t.Fatal("fixture participant pin missing")
		}
		p.Members[i].ClusterPinSHA256 = pin
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	request.ProfileDigest = profileDigest(p)
	body["profileDigest"] = request.ProfileDigest
	delete(body, "planDigest")
	canonical, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	request.BootstrapPlanDigest = hex.EncodeToString(sum[:])
	body["planDigest"] = request.BootstrapPlanDigest
	raw, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateDiagnosticMPIBinding(p, raw, request, true); err != nil {
		t.Fatal(err)
	}
	d.m.exec.fabric = &fabricService{m: d.m, ctx: context.Background(), runs: map[string]*fabricRunRecord{}}
	d.bootstrapTestRun = func(context.Context, io.Reader) ([]byte, error) {
		t.Error("admission test reached native work")
		return nil, errors.New("unexpected native work")
	}
	return d, p, raw, request
}

func fabricMPIHoldCases() []struct {
	name string
	hold func(*fabricService)
} {
	result := []struct {
		name string
		hold func(*fabricService)
	}{
		{"reservation", func(s *fabricService) { s.reservation = &fabricReservation{} }},
		{"recovery-failed", func(s *fabricService) { s.recoveryFailed = true }},
	}
	for _, state := range []string{"applying", "rolling-back", "recovery-required"} {
		result = append(result, struct {
			name string
			hold func(*fabricService)
		}{state, func(s *fabricService) {
			s.runs["fabric-fixture"] = &fabricRunRecord{Public: fabricOperation{State: state}}
		}})
	}
	return result
}

func assertFabricMPIAdmissionUnchanged(t *testing.T, d *diagnosticService, request diagnosticParticipantRequest) {
	t.Helper()
	if len(d.operations) != 0 || len(d.cancels) != 0 || d.reservation != nil {
		t.Fatal("held fabric published MPI ownership")
	}
	for _, path := range []string{d.coordinatorBootstrapPath(request.OperationID), d.runDir(request.OperationID)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("admission created MPI state at %s: %v", path, err)
		}
	}
}

func TestFabricMutationFencesMPICoordinatorBeforePublication(t *testing.T) {
	for _, test := range fabricMPIHoldCases() {
		t.Run(test.name, func(t *testing.T) {
			d, p, raw, request := fabricMPIAdmissionFixture(t)
			test.hold(d.m.exec.fabric)
			key := []byte("volatile-admission-fixture")
			_, err := d.startBootstrap(p, raw, key)
			if err == nil || !strings.Contains(err.Error(), "fabric") {
				t.Fatalf("fabric hold was not the coordinator rejection: %v", err)
			}
			if !bytes.Equal(key, make([]byte, len(key))) {
				t.Fatal("rejected coordinator retained its volatile identity")
			}
			assertFabricMPIAdmissionUnchanged(t, d, request)
		})
	}
}

func TestFabricMutationFencesMPIParticipantBeforePreflight(t *testing.T) {
	for _, test := range fabricMPIHoldCases() {
		t.Run(test.name, func(t *testing.T) {
			d, p, raw, request := fabricMPIAdmissionFixture(t)
			test.hold(d.m.exec.fabric)
			d.preflight = func(context.Context, diagnosticProfile, diagnosticMember, string) error {
				t.Error("held fabric reached participant preflight")
				return errors.New("unexpected preflight")
			}
			result, err := d.prepareBootstrapParticipant(context.Background(), p, raw, request)
			if err == nil || !strings.Contains(err.Error(), "fabric") || result.Prepared {
				t.Fatalf("fabric hold was not the participant rejection: %+v %v", result, err)
			}
			assertFabricMPIAdmissionUnchanged(t, d, request)
		})
	}
}

func TestFabricStableActivePassesBothMPIAdmissionGuards(t *testing.T) {
	t.Run("coordinator", func(t *testing.T) {
		d, p, raw, request := fabricMPIAdmissionFixture(t)
		d.m.exec.fabric.runs["fabric-fixture"] = &fabricRunRecord{Public: fabricOperation{State: "active"}}
		// This existing fence follows fabric admission and prevents the test from
		// publishing a new operation or launching an asynchronous participant.
		d.operations[request.OperationID] = diagnosticOperation{OperationID: request.OperationID}
		key := []byte("volatile-admission-fixture")
		_, err := d.startBootstrap(p, raw, key)
		if err == nil || !strings.Contains(err.Error(), "operation was already consumed") {
			t.Fatalf("stable fabric did not reach the existing coordinator fence: %v", err)
		}
		if !bytes.Equal(key, make([]byte, len(key))) || len(d.cancels) != 0 || len(d.operations) != 1 {
			t.Fatal("coordinator boundary changed volatile identity or ownership")
		}
		delete(d.operations, request.OperationID)
		assertFabricMPIAdmissionUnchanged(t, d, request)
	})
	t.Run("participant", func(t *testing.T) {
		d, p, raw, request := fabricMPIAdmissionFixture(t)
		d.m.exec.fabric.runs["fabric-fixture"] = &fabricRunRecord{Public: fabricOperation{State: "active"}}
		stop := errors.New("admitted participant preflight boundary")
		d.preflight = func(context.Context, diagnosticProfile, diagnosticMember, string) error { return stop }
		result, err := d.prepareBootstrapParticipant(context.Background(), p, raw, request)
		if !errors.Is(err, stop) || result.Prepared {
			t.Fatalf("stable fabric did not reach participant preflight: %+v %v", result, err)
		}
		assertFabricMPIAdmissionUnchanged(t, d, request)
	})
}
