// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRetainedVLLMModel(t *testing.T, st *engineState, id string) string {
	t.Helper()
	dir, err := vllmModelPath(st, id)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		t.Fatal(err)
	}
	config := []byte(`{"architectures":["FixtureForCausalLM"],"model_type":"fixture"}`)
	configDigest := sha256.Sum256(config)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	files := []vllmModelFile{{Path: "config.json", Size: int64(len(config)), SHA256: hex.EncodeToString(configDigest[:])}}
	manifest, _ := json.Marshal(files)
	digest := sha256.Sum256(manifest)
	model := vllmModel{ID: id, Source: "huggingface", Revision: strings.Split(id, "@")[1], License: "fixture", Digest: hex.EncodeToString(digest[:]), Bytes: int64(len(config)), Files: files}
	if err := writeVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), model); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSelectVLLMModelBindsVerifiedReceiptAndSurvivesStatus(t *testing.T) {
	f := vllmResourceFixture(t)
	id := "owner/model@" + strings.Repeat("a", 40)
	dir := writeRetainedVLLMModel(t, f.st, id)
	result, err := f.e.SelectVLLMModel(context.Background(), id)
	if err != nil {
		if strings.Contains(err.Error(), "privilege") || strings.Contains(err.Error(), "symbolic link") {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		t.Fatal(err)
	}
	if !result.Selected || result.Model != id || result.Status.SelectedModel != id {
		t.Fatalf("selection result = %+v", result)
	}
	target, err := os.Readlink(filepath.Join(f.st.modelDir, "selected"))
	if err != nil || target != filepath.Base(dir) {
		t.Fatalf("selected target = %q err=%v", target, err)
	}
	if got := f.e.snapshot("vllm", f.st).SelectedModel; got != id {
		t.Fatalf("status selected_model = %q", got)
	}
}

func TestSelectVLLMModelRejectsChangedBytesAndHeldGroupBeforeEffects(t *testing.T) {
	f := vllmResourceFixture(t)
	id := "owner/model@" + strings.Repeat("b", 40)
	dir := writeRetainedVLLMModel(t, f.st, id)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.SelectVLLMModel(context.Background(), id); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed model admission error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.st.modelDir, "selected")); !os.IsNotExist(err) {
		t.Fatalf("failed verification changed selection: %v", err)
	}

	plan := vllmGroupTestPlan(2)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	run := vllmGroupRun{RunID: strings.Repeat("c", 32), Generation: 7, PlanDigest: digest, Plan: plan, State: "cleanup-required", Failure: "cleanup remains required"}
	for _, member := range plan.Members {
		run.Ranks = append(run.Ranks, vllmGroupRank{NodeID: member.NodeID, Attempted: true})
	}
	if err := os.MkdirAll(f.st.installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, vllmGroupJournalFile), run); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.SelectVLLMModel(context.Background(), id); err == nil || !strings.Contains(err.Error(), "retained serving group holds") {
		t.Fatalf("held group selection error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.st.modelDir, "selected")); !os.IsNotExist(err) {
		t.Fatalf("held group changed selection: %v", err)
	}
}

func TestSelectedVLLMModelRejectsCallerControlledRedirect(t *testing.T) {
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.modelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	escape := t.TempDir()
	if err := os.Symlink(escape, filepath.Join(f.st.modelDir, "selected")); err != nil {
		t.Skipf("symlink unavailable on this host: %v", err)
	}
	if _, err := selectedVLLMModel(f.st); err == nil {
		t.Fatal("absolute caller-controlled selection was accepted")
	}
}
