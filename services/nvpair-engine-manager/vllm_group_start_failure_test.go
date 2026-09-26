// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
)

func TestVLLMStartFailureRedactsUnknownOutput(t *testing.T) {
	err := classifyVLLMRankProcess(errors.New("password-secret-123"), 23, []byte("private stdout/path/password-secret-123"))
	var typed *vllmRankStartError
	if !errors.As(err, &typed) || !validVLLMRankStartFailure(&typed.Failure) {
		t.Fatal("safe typed failure missing")
	}
	data, _ := json.Marshal(typed.Failure)
	if strings.Contains(string(data), "secret") || strings.Contains(err.Error(), "secret") || typed.Failure.StderrCode != "unclassified" {
		t.Fatal("unknown process text escaped")
	}
	known := classifyVLLMRankProcess(errors.New("exit status 70"), 0, []byte("PAIR_RANK_FAILURE:model_content_changed\n"))
	if !errors.As(known, &typed) || typed.Failure.Code != "model_content_changed" || typed.Failure.StderrCode != "worker_rejected" {
		t.Fatal("known fixed worker code lost")
	}
	malformed := &vllmRankStartError{vllmRankStartFailure{Stage: "secret", Code: "secret"}}
	if strings.Contains(malformed.Error(), "secret") {
		t.Fatal("malformed typed error escaped")
	}
}
func TestVLLMStartFailureRetainsOnlyAllowlistedMissingProperties(t *testing.T) {
	err := classifyVLLMRankProcess(errors.New("exit status 70"), 0, []byte("PAIR_RANK_READBACK_MISSING:DeviceAllow,ProtectControlGroups\nPAIR_RANK_FAILURE:system_manager_readback_incomplete\n"))
	var typed *vllmRankStartError
	if !errors.As(err, &typed) || !reflect.DeepEqual(typed.Failure.MissingProperties, []string{"DeviceAllow", "ProtectControlGroups"}) || !validVLLMRankStartFailure(&typed.Failure) {
		t.Fatal("bounded readback diagnosis was not retained")
	}
	for _, detail := range []string{"password", "DeviceAllow,DeviceAllow", "ProtectControlGroups,DeviceAllow"} {
		err = classifyVLLMRankProcess(errors.New("exit status 70"), 0, []byte("PAIR_RANK_READBACK_MISSING:"+detail+"\nPAIR_RANK_FAILURE:system_manager_readback_incomplete\n"))
		if !errors.As(err, &typed) || len(typed.Failure.MissingProperties) != 0 || !validVLLMRankStartFailure(&typed.Failure) {
			t.Fatalf("unsafe readback diagnosis escaped: %q", detail)
		}
	}
	for _, failure := range []vllmRankStartFailure{
		{Stage: "helper", Code: "process_failed", Exit: 70, StderrCode: "worker_rejected", MissingProperties: []string{"DeviceAllow"}},
		{Stage: "helper", Code: "system_manager_readback_incomplete", Exit: 70, StderrCode: "worker_rejected", MissingProperties: []string{"password"}},
		{Stage: "helper", Code: "system_manager_readback_incomplete", Exit: 70, StderrCode: "worker_rejected", MissingProperties: []string{"DeviceAllow", "DeviceAllow"}},
	} {
		if validVLLMRankStartFailure(&failure) {
			t.Fatal("invalid readback metadata was accepted")
		}
	}
}
func TestVLLMStartFailurePeerRoundtripAndCleanupReceipt(t *testing.T) {
	p := vllmGroupTestPlan(2)
	digest, _ := vllmGroupPlanDigest(p)
	r := vllmGroupPeerRequest{Protocol: vllmGroupPeerProtocol, Action: "start", RunID: strings.Repeat("c", 32), Generation: 1, PlanDigest: digest, Plan: p, Rank: 1}
	original := &vllmRankStartError{vllmRankStartFailure{Stage: "helper", Code: "system_manager_readback_incomplete", Exit: 70, StdoutBytes: 0, StderrCode: "worker_rejected", MissingProperties: []string{"DeviceAllow"}}}
	result := failedVLLMGroupStart(r, original)
	raw, _ := json.Marshal(result)
	var got vllmGroupPeerResult
	if strictDiagnosticJSON(raw, &got) != nil || validateVLLMGroupPeerResult(r, got) != nil {
		t.Fatal("safe failure receipt rejected")
	}
	var typed *vllmRankStartError
	if !errors.As(vllmGroupActionOutcome(r, got), &typed) || !reflect.DeepEqual(typed.Failure, original.Failure) {
		t.Fatal("failure was erased across JSON")
	}
	r.Action = "stop"
	got.Action = "stop"
	got.State = "stopped"
	got.Code = "ok"
	got.CleanupConfirmed = true
	if err := vllmGroupActionOutcome(r, got); err != nil {
		t.Fatal("retained failure broke successful cleanup", err)
	}
	got.StartFailure.Code = "secret-payload"
	got.StartFailure.MissingProperties[0] = "ProtectControlGroups"
	if original.Failure.MissingProperties[0] != "DeviceAllow" {
		t.Fatal("peer JSON roundtrip aliased missing-property detail")
	}
	if validateVLLMGroupPeerResult(r, got) == nil {
		t.Fatal("unknown diagnostic code admitted")
	}
}
func TestVLLMStartFailureSurvivesGroupCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		expected := &vllmRankStartError{vllmRankStartFailure{Stage: "helper", Code: "system_manager_readback_incomplete", Exit: 70, StdoutBytes: 0, StderrCode: "worker_rejected", MissingProperties: []string{"DeviceAllow"}}}
		g := vllmGroupTestOwner(t, func(_ context.Context, b vllmGroupBinding, action string) error {
			if action == "start" && b.Rank == 1 {
				return expected
			}
			return nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		vllmGroupTestStart(t, g, ctx, 2)
		synctest.Wait()
		run := g.status()
		if run.State != "failed" || !run.CleanupConfirmed || run.Ranks[1].StartFailure == nil || !reflect.DeepEqual(*run.Ranks[1].StartFailure, expected.Failure) {
			t.Fatal("cleanup erased first start failure")
		}
		raw, err := json.Marshal(run)
		if err != nil || !strings.Contains(string(raw), `"startFailure"`) {
			t.Fatal("status omitted failure")
		}
		run.Ranks[1].StartFailure.Code = "mutation"
		run.Ranks[1].StartFailure.MissingProperties[0] = "ProtectControlGroups"
		retained := g.status().Ranks[1].StartFailure
		if retained.Code != "system_manager_readback_incomplete" || !reflect.DeepEqual(retained.MissingProperties, []string{"DeviceAllow"}) {
			t.Fatal("status aliases retained failure")
		}
	})
}

func TestVLLMStartFailurePreservesOnlyValidatedContextIdentity(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		failure := rankStartError("helper", "process_failed", cause, 0)
		if !errors.Is(failure, cause) || errors.Unwrap(failure) != nil {
			t.Fatal("redacted context identity lost or raw cause retained")
		}
		failure.Failure.Stage = "private-invalid-stage"
		if errors.Is(failure, cause) {
			t.Fatal("invalid diagnostic claimed lifecycle cancellation")
		}
	}
}

func TestVLLMStartFailureNormalStopRetainsCancelledDetailWithoutFalseFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := vllmGroupTestOwner(t, func(ctx context.Context, b vllmGroupBinding, action string) error {
			if action == "start" && b.Rank == 1 {
				<-ctx.Done()
				request := vllmGroupRequest(b, action)
				// This is the same redacted result consumed after paired transport.
				return vllmGroupActionOutcome(request, failedVLLMGroupStart(request, ctx.Err()))
			}
			return nil
		})
		run := vllmGroupTestStart(t, g, context.Background(), 2)
		synctest.Wait()
		if err := g.stop(context.Background(), run.RunID, run.Generation); err != nil {
			t.Fatal(err)
		}
		got := g.status()
		if got.RunID != run.RunID || got.Generation != run.Generation || got.State != "stopped" || !got.CleanupConfirmed || got.Failure != "" || g.reserved() {
			t.Fatalf("normal Stop became a failed or held run: %+v", got)
		}
		if got.Ranks[1].StartFailure == nil || got.Ranks[1].StartFailure.Code != "cancelled" {
			t.Fatal("normal Stop erased its redacted startup cancellation detail")
		}
	})
}
