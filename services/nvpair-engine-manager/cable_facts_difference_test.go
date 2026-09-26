// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrusttest"
)

func TestCableFactsDifferencePrearmReportsField(t *testing.T) {
	for _, mode := range []string{"alias-mac", "remote-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			request := onceTestRequest(t)
			if mode == "alias-mac" {
				request.Review.Targets[0].Ports[0].Interfaces[0].MAC = "02:00:00:00:00:fe"
			}
			input, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			fake := newProbeTestIO()
			var output bytes.Buffer
			err = runCableProbeWorker(context.Background(), strings.NewReader(string(input)+"\n"), &output, func(ctx context.Context, request cableProbeOnceRequest) (*cableProbeSession, error) {
				// Retain the historical failure-frame contract independently of the
				// removed worker matrix read. New workers use native local checks.
				index, field, status := 0, "interface-mac", ""
				if mode == "remote-unavailable" {
					index, field, status = 1, "read", "unavailable"
				}
				failure := newCablePreparationError("prepare-facts-mismatch", "not-attempted", errors.New("synthetic-private-read-detail"))
				failure.FactsDifference = &cableprobe.FactsDifference{TargetIndex: &index, Field: field, ReadStatus: status}
				return nil, boundCablePreparationError(failure)
			})
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if json.Unmarshal(output.Bytes(), &wire) != nil || wire["factsDifference"] == nil {
				t.Fatalf("preparation mismatch omitted its bounded facts difference: %s", output.Bytes())
			}
			var difference struct {
				TargetIndex int    `json:"targetIndex"`
				Field       string `json:"field"`
				ReadStatus  string `json:"readStatus"`
			}
			if json.Unmarshal(wire["factsDifference"], &difference) != nil {
				t.Fatal("invalid facts difference")
			}
			if mode == "alias-mac" && (difference.TargetIndex != 0 || difference.Field != "interface-mac" || difference.ReadStatus != "") {
				t.Fatalf("wrong L2 field: %+v", difference)
			}
			if mode == "remote-unavailable" && (difference.TargetIndex != 1 || difference.Field != "read" || difference.ReadStatus != "unavailable") {
				t.Fatalf("read failure was claimed as identity drift: %+v", difference)
			}
			if fake.lockCalls != 0 || fake.readCalls != 0 || fake.writeCalls != 0 || bytes.Contains(output.Bytes(), []byte("synthetic-private")) {
				t.Fatal("difference evidence acquired resources or exposed raw detail")
			}
		})
	}
}

func TestCableFactsDifferenceFirstOrderedField(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		change      func(*cableprobe.Target)
	}{
		{"node", "target-node-id", func(v *cableprobe.Target) { v.NodeID = "other" }},
		{"principal", "principal", func(v *cableprobe.Target) { v.Principal = "other" }},
		{"ports", "ports", func(v *cableprobe.Target) { v.Ports = nil }},
		{"switch", "switch-id", func(v *cableprobe.Target) { v.Ports[0].SwitchID = "other" }},
		{"port-name", "port-name", func(v *cableprobe.Target) { v.Ports[0].PortName = "other" }},
		{"interfaces", "interfaces", func(v *cableprobe.Target) { v.Ports[0].Interfaces = nil }},
		{"alias-name", "interface-name", func(v *cableprobe.Target) { v.Ports[0].Interfaces[0].Name = "other" }},
		{"alias-index", "interface-index", func(v *cableprobe.Target) { v.Ports[0].Interfaces[0].Index++ }},
		{"alias-mac", "interface-mac", func(v *cableprobe.Target) { v.Ports[0].Interfaces[0].MAC = "02:00:00:00:00:fe" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approved := onceTestRequest(t).Review.Targets
			actual := cloneCableRun(cableprobe.Run{Targets: approved}).Targets
			tc.change(&actual[1])
			d := firstCableFactsDifference(approved, actual, nil)
			if d == nil || d.TargetIndex == nil || *d.TargetIndex != 1 || d.Field != tc.field || d.ReadStatus != "" || !validCableFactsDifference(d, approved) {
				t.Fatalf("wrong first ordered field: %+v", d)
			}
			actual[0].Ports[0].Interfaces[0].MAC = "02:00:00:00:00:fd"
			d = firstCableFactsDifference(approved, actual, nil)
			if *d.TargetIndex != 0 || d.Field != "interface-mac" {
				t.Fatal("later target displaced the first approved target")
			}
		})
	}
	approved := onceTestRequest(t).Review.Targets
	actual := cloneCableRun(cableprobe.Run{Targets: approved}).Targets
	actual[0].Reason, actual[0].RawPrivilege = "volatile-review-reason", "unavailable"
	if !reflect.DeepEqual(cableProbeTargetFacts(approved), cableProbeTargetFacts(actual)) || firstCableFactsDifference(approved, actual, nil) != nil {
		t.Fatal("non-L2 review fields became comparison inputs")
	}
	for _, field := range []string{"ports", "interfaces"} {
		want, got := cloneCableRun(cableprobe.Run{Targets: approved}).Targets, cloneCableRun(cableprobe.Run{Targets: approved}).Targets
		if field == "ports" {
			want[0].Ports = nil
			got[0].Ports = []cableprobe.Port{}
		} else {
			want[0].Ports[0].Interfaces = nil
			got[0].Ports[0].Interfaces = []cableprobe.Interface{}
		}
		if d := firstCableFactsDifference(want, got, nil); d == nil || d.Field != field {
			t.Fatal("nil and empty slices were normalized")
		}
	}
	actual[0], actual[1] = actual[1], actual[0]
	if d := firstCableFactsDifference(approved, actual, nil); d == nil || *d.TargetIndex != 0 || d.Field != "target-node-id" {
		t.Fatal("target order was normalized")
	}
	for _, kind := range []string{"ports", "aliases"} {
		want := cloneCableRun(cableprobe.Run{Targets: approved}).Targets
		if kind == "ports" {
			want[0].Ports = append(want[0].Ports, want[1].Ports[0])
		} else {
			want[0].Ports[0].Interfaces = append(want[0].Ports[0].Interfaces, cableprobe.Interface{Name: "eth2", Index: 2, MAC: "02:00:00:00:00:02"})
		}
		got := cloneCableRun(cableprobe.Run{Targets: want}).Targets
		field := "switch-id"
		if kind == "ports" {
			got[0].Ports[0], got[0].Ports[1] = got[0].Ports[1], got[0].Ports[0]
		} else {
			aliases := got[0].Ports[0].Interfaces
			aliases[0], aliases[1] = aliases[1], aliases[0]
			field = "interface-name"
		}
		if d := firstCableFactsDifference(want, got, nil); d == nil || d.Field != field {
			t.Fatal("ordered port or alias identity was normalized")
		}
	}
}

func TestCableFactsDifferenceReadClassification(t *testing.T) {
	for _, mode := range []string{"unavailable", "invalid", "stale", "expired", "binding-changed", "port-unavailable", "identity-invalid", "transit-stale", "local-stale"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				localReads := 0
				m, _, dir := cableTestManager(t, func(*http.Request) (*http.Response, error) {
					localReads++
					observed := time.Now()
					if mode == "expired" {
						observed = observed.Add(-time.Second)
					}
					if mode == "local-stale" {
						observed = observed.Add(-2 * time.Second)
					}
					return cableTestResponse(t, cableTestInfo(t, observed, 1)), nil
				})
				cableTestRemoteRead(t, m, func(*http.Request) (*http.Response, error) {
					if mode == "unavailable" {
						return nil, errors.New("synthetic-private-error")
					}
					if mode == "invalid" {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{invalid"))}, nil
					}
					if mode == "expired" {
						time.Sleep(1500 * time.Millisecond)
					}
					if mode == "binding-changed" {
						clustertrusttest.WritePeerPin(t, dir, "principal-peer")
					}
					facts := cableTestPeerSnapshot(t)
					switch mode {
					case "stale":
						facts.AgeMs = 2000
					case "identity-invalid":
						facts.Principal = "foreign"
					case "port-unavailable":
						facts.Ports[0].PortName = "other"
					case "transit-stale":
						facts.AgeMs = 1999
						time.Sleep(time.Millisecond)
					}
					return cableTestResponse(t, facts), nil
				})
				approved := onceTestRequest(t).Review.Targets
				review, statuses, err := m.reviewCablesDetailed(context.Background(), cableTestSelection())
				if err != nil {
					t.Fatal(err)
				}
				want, index := mode, 1
				switch mode {
				case "identity-invalid":
					want = "invalid"
				case "transit-stale":
					want = "stale"
				case "local-stale":
					want = "stale"
					index = 0
				case "expired":
					index = 0
				}
				d := firstCableFactsDifference(approved, review.Targets, statuses)
				if d == nil || *d.TargetIndex != index || d.Field != "read" || d.ReadStatus != want {
					t.Fatalf("wrong structured reread classification: %+v, statuses=%v", d, statuses)
				}
				if mode == "local-stale" && localReads != 2 {
					t.Fatal("local stale-only retry bound changed")
				}
				for _, target := range review.Targets {
					if strings.Contains(target.Reason, "synthetic-private") {
						t.Fatal("private failure detail escaped")
					}
				}
			})
		})
	}
}

func factsDifferenceTestLine(t *testing.T, request cableProbeOnceRequest, difference string) []byte {
	t.Helper()
	terminal := cablePrearmMessage{State: "prearm-failed", RunID: request.RunID, ReviewID: request.Review.ReviewID, Code: "prepare-facts-mismatch", ResourceOutcome: "not-attempted"}
	encoded, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if difference != "" {
		encoded = append(append(encoded[:len(encoded)-1], []byte(`,"factsDifference":`)...), []byte(difference+"}")...)
	}
	return encoded
}

func TestCableFactsDifferenceStrictWire(t *testing.T) {
	request := onceTestRequest(t)
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"legacy-absence", "", true},
		{"field", `{"targetIndex":1,"field":"interface-mac"}`, true},
		{"read", `{"targetIndex":0,"field":"read","readStatus":"expired"}`, true},
		{"unknown-key", `{"targetIndex":0,"field":"ports","raw":"synthetic-private"}`, false},
		{"unknown-field", `{"targetIndex":0,"field":"synthetic-private"}`, false},
		{"missing-index", `{"field":"ports"}`, false},
		{"null-index", `{"targetIndex":null,"field":"ports"}`, false},
		{"negative-index", `{"targetIndex":-1,"field":"ports"}`, false},
		{"unapproved-index", `{"targetIndex":2,"field":"ports"}`, false},
		{"fraction-index", `{"targetIndex":0.5,"field":"ports"}`, false},
		{"null", `null`, false},
		{"duplicate-index", `{"targetIndex":1,"targetIndex":0,"field":"ports"}`, false},
		{"read-missing-status", `{"targetIndex":0,"field":"read"}`, false},
		{"read-unknown-status", `{"targetIndex":0,"field":"read","readStatus":"synthetic-private"}`, false},
		{"field-with-status", `{"targetIndex":0,"field":"ports","readStatus":"stale"}`, false},
		{"field-with-empty-status", `{"targetIndex":0,"field":"ports","readStatus":""}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeCablePrearmLine(factsDifferenceTestLine(t, request, tc.data), request.RunID, request.Review.ReviewID, request.Review.Targets)
			if (err == nil) != tc.valid {
				t.Fatalf("wire admission mismatch: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("raw metadata escaped protocol error")
			}
		})
	}
	valid := factsDifferenceTestLine(t, request, `{"targetIndex":0,"field":"ports"}`)
	if _, err := decodeCablePrearmLine(valid, request.RunID, request.Review.ReviewID); err == nil {
		t.Fatal("metadata was accepted without approved target context")
	}
	for _, replacements := range [][2]string{{`"prepare-facts-mismatch"`, `"prepare-configure-failed"`}, {`"not-attempted"`, `"rollback-confirmed"`}, {`"sent":0`, `"sent":1`}, {`"cleanupConfirmed":false`, `"cleanupConfirmed":true`}} {
		line := strings.Replace(string(valid), replacements[0], replacements[1], 1)
		if _, err := decodeCablePrearmLine([]byte(line), request.RunID, request.Review.ReviewID, request.Review.Targets); err == nil {
			t.Fatal("metadata changed resource or failure context")
		}
	}
}

func TestCableFactsDifferenceProviderAndRetainedProjection(t *testing.T) {
	f := newCableProductFixture(t)
	launch := f.s.launch
	f.s.launch = func(ctx context.Context, client *onboardingSSH, plan cableLaunchPlan, request cableProbeOnceRequest, admin string) (*cableWorker, error) {
		if plan.NodeID != "host-peer" {
			return launch(ctx, client, plan, request, admin)
		}
		// The reporting peer points at the owner's approved target. Whole-run
		// order must survive the real provider, coordinator and retained journal.
		line := factsDifferenceTestLine(t, request, `{"targetIndex":0,"field":"read","readStatus":"expired"}`)
		session := &launchTestSession{output: strings.NewReader(string(line) + "\n"), done: make(chan struct{})}
		worker, err := openCableWorker(ctx, &onboardingSSH{testCableSession: func() (cableSSHSession, error) { return session, nil }}, plan, request, admin)
		if err == nil {
			session.finish()
		}
		return worker, err
	}
	run := f.done(f.start(f.ready()))
	if run.State != "failed" || run.CleanupConfirmed || f.count("start-attempt:") != 0 || !validCableDiagnostics(run) {
		t.Fatal("prearm evidence changed launch or cleanup admission")
	}
	row := run.Diagnostics.Participants[1]
	if row.NodeID != "host-peer" || row.Code != "prepare-facts-mismatch" || row.FactsDifference == nil || *row.FactsDifference.TargetIndex != 0 || row.FactsDifference.ReadStatus != "expired" {
		t.Fatalf("affected target was replaced by reporting target: %+v", row)
	}
	before, err := os.ReadFile(f.s.file(run.RunID))
	if err != nil {
		t.Fatal(err)
	}
	loaded := newCableProductService(f.s.m)
	defer loaded.shutdown()
	opened, err := loaded.status(cableprobe.StatusRequest{RunID: run.RunID})
	if err != nil || !reflect.DeepEqual(opened, run) || !loaded.held() {
		t.Fatal("difference did not survive unchanged retained loading")
	}
	after, err := os.ReadFile(f.s.file(run.RunID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read rewrote original history")
	}
	for _, mode := range []string{"legacy", "wrong-code", "wrong-outcome", "bad-index", "missing-index", "final-code"} {
		t.Run(mode, func(t *testing.T) {
			candidate := cloneCableRun(run)
			row := &candidate.Diagnostics.Participants[1]
			switch mode {
			case "legacy":
				row.FactsDifference = nil
			case "wrong-code":
				row.Code = "prepare-open-failed"
			case "wrong-outcome":
				row.PreparationResourceOutcome = "rollback-confirmed"
			case "bad-index":
				index := 2
				row.FactsDifference.TargetIndex = &index
			case "missing-index":
				row.FactsDifference.TargetIndex = nil
			case "final-code":
				row.FinalValidationCode = "facts-stale"
			}
			if validCableDiagnostics(candidate) != (mode == "legacy") {
				t.Fatal("retained metadata accepted in contradictory context")
			}
		})
	}
}
