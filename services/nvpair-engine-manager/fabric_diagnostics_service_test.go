// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFabricInspectDiagnosticSurvivesReservationCleanupAndReload(t *testing.T) {
	for _, mode := range []string{"clean-release", "release-failed", "wire-error", "unknown-error"} {
		t.Run(mode, func(t *testing.T) {
			s, r := fabricServiceFixture(t)
			var inspect, apply, rollback int
			var controls []string
			s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
				controls = append(controls, request.Method+":"+target.NodeID)
				if mode == "release-failed" && request.Method == "release" && target.NodeID == r.Public.Targets[1].NodeID {
					return fabricControlResult{}, errors.New("synthetic-private-release-error")
				}
				return fabricControlResult{}, nil
			}
			s.worker = func(_ context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
				if request.Protocol != fabricWorkerProtocol {
					t.Fatal("missing explicit worker protocol")
				}
				if request.Method == "apply" {
					apply++
					return fabricWorkerResult{}, nil
				}
				if request.Method == "rollback" {
					rollback++
					return fabricWorkerResult{}, nil
				}
				inspect++
				if plan.NodeID == r.Public.Targets[0].NodeID {
					return fabricWorkerResult{}, nil
				}
				if mode == "unknown-error" {
					return fabricWorkerResult{}, errors.New("synthetic-private-query-error")
				}
				result := fabricWorkerResult{Protocol: fabricWorkerProtocol, OperationID: request.OperationID, NodeID: plan.NodeID, Principal: plan.Principal, Method: "inspect", FailureCode: "network-manager-query-failed"}
				if mode == "wire-error" {
					return result, nil
				}
				return result, fabricInspectionError("network-manager-query-failed", errors.New("synthetic-private-query-error"))
			}
			s.execute(context.Background(), r)
			wantCode := "network-manager-query-failed"
			if mode == "unknown-error" {
				wantCode = "native-inspect-failed"
			}
			if inspect != 2 || apply != 0 || rollback != 0 || r.Attempted[0] || r.Attempted[1] || r.Public.EffectsApplied || r.Public.EffectsUnconfirmed {
				t.Fatal("diagnostic changed pre-apply effects/accounting")
			}
			if r.Public.Failure == nil || *r.Public.Failure != (fabricFailure{NodeID: r.Public.Targets[1].NodeID, Phase: "inspect", Code: wantCode}) {
				t.Fatalf("first fixed cause lost: %+v", r.Public.Failure)
			}
			if r.Public.CleanupConfirmed != (mode != "release-failed") {
				t.Fatal("diagnostic promoted cleanup or lost independent release outcome")
			}
			wantState := "failed"
			if mode == "release-failed" {
				wantState = "recovery-required"
			}
			if r.Public.State != wantState {
				t.Fatal("terminal state changed")
			}
			if !reflect.DeepEqual(controls, []string{"reserve:node-a", "reserve:node-b", "release:node-b", "release:node-a"}) {
				t.Fatalf("reservation sequence changed: %v", controls)
			}
			data, err := os.ReadFile(s.file(r.Public.OperationID))
			if err != nil || strings.Contains(string(data), "synthetic-private") {
				t.Fatal("retention failed or leaked raw error")
			}
			var saved fabricRunRecord
			if json.Unmarshal(data, &saved) != nil || !validFabricRecord(saved) || !reflect.DeepEqual(saved.Public.Failure, r.Public.Failure) {
				t.Fatal("typed diagnostic did not survive original journal")
			}
			loaded := newFabricService(s.m)
			if loaded.recoveryFailed || loaded.runs[r.Public.OperationID] == nil || !reflect.DeepEqual(loaded.runs[r.Public.OperationID].Public.Failure, r.Public.Failure) {
				t.Fatal("typed diagnostic did not reload")
			}
			after, err := os.ReadFile(s.file(r.Public.OperationID))
			if err != nil || string(after) != string(data) {
				t.Fatal("reload rewrote historical failure")
			}
		})
	}
}

func TestFabricInspectDiagnosticRecordGuardAndLegacyCompatibility(t *testing.T) {
	_, r := fabricServiceFixture(t)
	r.Public.Failure = &fabricFailure{NodeID: r.Public.Targets[0].NodeID, Phase: "inspect", Code: "route-query-failed"}
	if !validFabricRecord(*r) {
		t.Fatal("valid pre-apply failure rejected")
	}
	for _, test := range []struct {
		name   string
		change func(*fabricRunRecord)
		valid  bool
	}{
		{"legacy", func(r *fabricRunRecord) { r.Public.Failure = nil }, true},
		{"foreign-node", func(r *fabricRunRecord) { r.Public.Failure.NodeID = "foreign" }, false},
		{"unknown-code", func(r *fabricRunRecord) { r.Public.Failure.Code = "synthetic-private" }, false},
		{"wrong-phase", func(r *fabricRunRecord) { r.Public.Failure.Phase = "apply" }, false},
		{"apply-attempted", func(r *fabricRunRecord) { r.Attempted[0] = true }, false},
		{"effects-applied", func(r *fabricRunRecord) { r.Public.EffectsApplied = true }, false},
		{"effects-unconfirmed", func(r *fabricRunRecord) { r.Public.EffectsUnconfirmed = true }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(r)
			var copy fabricRunRecord
			_ = json.Unmarshal(raw, &copy)
			test.change(&copy)
			if validFabricRecord(copy) != test.valid {
				t.Fatal("diagnostic guard or legacy semantics changed")
			}
		})
	}
}

func TestFabricInspectErrorTakesPrecedenceOverReturnedBlockerText(t *testing.T) {
	s, r := fabricServiceFixture(t)
	s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
		return fabricControlResult{}, nil
	}
	s.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
		return fabricWorkerResult{Facts: fabricNativeFacts{Blockers: []string{"synthetic-private-blocker"}}}, fabricInspectionError("dns-unavailable", errors.New("synthetic-private-error"))
	}
	s.execute(context.Background(), r)
	if r.Public.Failure == nil || r.Public.Failure.Code != "dns-unavailable" || strings.Contains(r.Public.Message, "synthetic-private") || strings.Contains(r.Failure, "synthetic-private") {
		t.Fatal("blocker text replaced or leaked over the first typed error")
	}
}
