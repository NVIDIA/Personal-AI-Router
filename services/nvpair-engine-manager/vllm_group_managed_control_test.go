// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVLLMManagedRankTypedPendingQualifiesOnlyOnce(t *testing.T) {
	h := readinessFixture(t, false)
	h.rank.st.vllmRank = h.rank
	for range 3 {
		result, err := h.rank.peerAction(context.Background(), h.binding, "ready")
		if err != nil || result.State != "started" || result.CleanupConfirmed || !result.EffectsApplied {
			t.Fatalf("pending was not a bound typed receipt: %+v %v", result, err)
		}
		if vllmGroupActionOutcome(vllmGroupRequest(h.binding, "ready"), result) == nil {
			t.Fatal("pending was accepted as ready")
		}
	}
	h.releaseOnce.Do(func() { close(h.release) })
	waitReadinessQualification(t, h)
	for range 3 {
		result, err := h.rank.peerAction(context.Background(), h.binding, "ready")
		if err != nil || vllmGroupActionOutcome(vllmGroupRequest(h.binding, "ready"), result) != nil {
			t.Fatalf("qualified receipt rejected: %+v %v", result, err)
		}
	}
	if h.posts.Load() != 1 {
		t.Fatal("status/ready polling repeated generation")
	}
	h.owns.Store(false)
	if _, err := h.rank.peerAction(context.Background(), h.binding, "ready"); err == nil {
		t.Fatal("lost owned model health became pending or success")
	}
}

func TestVLLMManagedRankRestoredReceiptHoldsOrdinaryMutations(t *testing.T) {
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.st.installDir, "serving-rank.json")
	receipt := vllmRankReceipt{RunID: strings.Repeat("a", 32), Generation: 2, PlanDigest: strings.Repeat("b", 64), Rank: 1, State: "started"}
	if err := writeVLLMJSON(f.st.installDir, path, receipt); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	f.st.vllmRank = f.e.restoreVLLMRankHold(f.st)
	if f.st.vllmRank == nil || !f.st.vllmRank.reserved() || rejectVLLMGroupOwnerMutation(f.st, "ordinary start") == nil {
		t.Fatal("retained unconfirmed rank allowed replacement")
	}
	if err := f.e.doStop(f.st, "vllm"); err == nil {
		t.Fatal("missing native recovery handle became confirmed cleanup")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("unconfirmed native receipt was rewritten")
	}
	receipt.State, receipt.CleanupConfirmed = "stopped", true
	if err := writeVLLMJSON(f.st.installDir, path, receipt); err != nil {
		t.Fatal(err)
	}
	if f.e.restoreVLLMRankHold(f.st) != nil {
		t.Fatal("confirmed completed receipt blocked standalone work")
	}
}

func TestVLLMManagedRankParticipantStatusCannotAdvertise(t *testing.T) {
	b, placement := managedRankFixture(t, 1)
	r := &vllmManagedRank{binding: b, placement: placement,
		receipt: vllmRankReceipt{RunID: b.RunID, Generation: b.Generation, PlanDigest: b.PlanDigest, Rank: b.Rank, State: "started"}}
	result := r.routeStatus(EngineStatus{Engine: "vllm", Running: true, Healthy: true})
	if result.Healthy || result.Starting || result.ServingGroup == nil || result.ServingGroup.Role != "participant" {
		t.Fatalf("headless rank projected as standalone healthy: %+v", result)
	}
	r.receipt.State, r.receipt.CleanupConfirmed = "stopped", true
	result = r.routeStatus(EngineStatus{Engine: "vllm", Running: true, Healthy: true})
	if !result.Healthy || result.ServingGroup != nil {
		t.Fatal("old completed rank masked later standalone work")
	}
}

func TestVLLMManagedRankRechecksReviewedResourcesAndPlacement(t *testing.T) {
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	b, placement := managedRankFixture(t, 1)
	memory := .05
	settings := vllmResourceSettings{GPUMemoryUtilization: &memory}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, vllmResourceSettingsFile), settings); err != nil {
		t.Fatal(err)
	}
	if code := rankFailureCode(validateVLLMRankReviewedSettings(f.st, b, placement)); code != "placement_rank_mismatch" {
		t.Fatalf("missing reviewed resources became native permission: %q", code)
	}
	b.Plan.Members[1].Resources = &settings
	b.Plan.Members[1].Placement = &vllmGroupPlacement{ModelPath: placement.ModelPath, Address: placement.LocalAddress, APIPort: placement.APIPort, MasterPort: placement.MasterPort}
	b.Plan.Members[0].Placement = &vllmGroupPlacement{ModelPath: placement.ModelPath, Address: placement.CoordinatorAddress, APIPort: placement.APIPort, MasterPort: placement.MasterPort}
	if err := validateVLLMRankReviewedSettings(f.st, b, placement); err != nil {
		t.Fatal(err)
	}
	changed := placement
	changed.LocalAddress = "10.0.0.99"
	if code := rankFailureCode(validateVLLMRankReviewedSettings(f.st, b, changed)); code != "placement_rank_mismatch" {
		t.Fatalf("changed native placement accepted: %q", code)
	}
	newMemory := .2
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, vllmResourceSettingsFile), vllmResourceSettings{GPUMemoryUtilization: &newMemory}); err != nil {
		t.Fatal(err)
	}
	if code := rankFailureCode(validateVLLMRankReviewedSettings(f.st, b, placement)); code != "saved_resource_settings_changed" {
		t.Fatalf("changed saved resources accepted: %q", code)
	}
}

func TestVLLMRankPrepareAdmitsClosedReceiptWithFullNativePlan(t *testing.T) {
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.st.installDir, "serving-rank.json")
	if err := admitRetainedVLLMRank(f.st, path); err != nil {
		t.Fatalf("absent receipt blocked preparation: %v", err)
	}
	receipt := vllmRankReceipt{SystemPlan: &vllmRankSystemPlan{Peers: []string{strings.Repeat("a", 5000)}},
		RunID: strings.Repeat("a", 32), Generation: 55, PlanDigest: strings.Repeat("b", 64), State: "stopped", CleanupConfirmed: true}
	if err := writeVLLMJSON(f.st.installDir, path, receipt); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= 4096 {
		t.Fatalf("fixture receipt does not exceed 4 KiB: %v", err)
	}
	if err := admitRetainedVLLMRank(f.st, path); err != nil {
		t.Fatalf("closed receipt with its full native plan blocked preparation: %v", err)
	}
	receipt.State, receipt.CleanupConfirmed = "started", false
	if err := writeVLLMJSON(f.st.installDir, path, receipt); err != nil {
		t.Fatal(err)
	}
	if code := rankFailureCode(admitRetainedVLLMRank(f.st, path)); code != "retained_rank_owner_changed" {
		t.Fatalf("unfinished receipt admitted replacement: %q", code)
	}
}

func rankFailureCode(err error) string {
	var failure *vllmRankStartError
	if !errors.As(err, &failure) {
		return ""
	}
	return failure.Failure.Code
}

func TestVLLMGroupModelConfigKeepsExactBoundedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte("{\"model_type\":\"qwen2\"}\n")
	sum := sha256.Sum256(data)
	model := vllmModel{Files: []vllmModelFile{{Path: "config.json", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}}}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readVLLMGroupModelConfig(model, dir)
	if err != nil || string(got) != string(data) {
		t.Fatalf("exact config bytes changed: %q %v", got, err)
	}
	for _, changed := range []string{"{}", strings.Repeat(" ", (1<<20)+1)} {
		if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readVLLMGroupModelConfig(model, dir); err == nil {
			t.Fatal("changed or oversized configuration accepted")
		}
	}
}
