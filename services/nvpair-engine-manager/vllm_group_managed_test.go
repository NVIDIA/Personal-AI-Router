// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func managedRankFixture(t *testing.T, rank int) (vllmGroupBinding, vllmRankPlacement) {
	t.Helper()
	plan := vllmGroupTestPlan(2)
	digest, err := vllmGroupPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	return vllmGroupBinding{RunID: strings.Repeat("c", 32), Generation: 1, PlanDigest: digest, Plan: plan, Rank: rank}, vllmRankPlacement{LocalNode: plan.Members[rank].NodeID, LocalAddress: []string{"10.0.0.1", "10.0.0.2"}[rank], CoordinatorAddress: "10.0.0.1", ModelPath: filepath.Join(t.TempDir(), "model"), MasterPort: 29601, APIPort: 8181}
}
func TestVLLMManagedRankArguments(t *testing.T) {
	for rank := range 2 {
		b, p := managedRankFixture(t, rank)
		mem := .05
		length := int32(2048)
		args, err := vllmRankArgs("/managed/bin/vllm", b, p, vllmResourceSettings{GPUMemoryUtilization: &mem, MaxModelLen: &length})
		if err != nil {
			t.Fatal(err)
		}
		for flag, want := range map[string]string{"--nnodes": "2", "--tensor-parallel-size": "2", "--pipeline-parallel-size": "1", "--data-parallel-size": "1", "--distributed-executor-backend": "mp", "--data-parallel-backend": "mp", "--gpu-memory-utilization": "0.05", "--max-model-len": "2048"} {
			i := slices.Index(args, flag)
			if i < 0 || i+1 >= len(args) || args[i+1] != want {
				t.Fatalf("missing %s %s: %v", flag, want, args)
			}
		}
		if slices.Contains(args, "--headless") != (rank == 1) || slices.Contains(args, "--port") != (rank == 0) {
			t.Fatalf("participant accidentally exposes coordinator API: %v", args)
		}
		if args[2] != p.ModelPath || !slices.Contains(args, b.Plan.Model) {
			t.Fatal("model path/id lost")
		}
	}
}
func TestManagedVLLMProcessArgsUseSavedResourceSettings(t *testing.T) {
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.st.installDir, vllmResourceSettingsFile), []byte(`{"gpu_memory_utilization":0.05,"max_model_len":2048}`), 0o600); err != nil {
		t.Fatal(err)
	}
	model := "owner/model@" + strings.Repeat("a", 40)
	writeRetainedVLLMModel(t, f.st, model)
	if _, err := f.e.SelectVLLMModel(context.Background(), model); err != nil {
		if strings.Contains(err.Error(), "privilege") || strings.Contains(err.Error(), "symbolic link") {
			t.Skipf("symlink unavailable on this host: %v", err)
		}
		t.Fatal(err)
	}
	args, err := managedVLLMProcessArgs(f.st, []string{"serve", "/models/selected"})
	if err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{"--served-model-name": model, "--gpu-memory-utilization": "0.05", "--max-model-len": "2048"} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Fatalf("missing saved %s %s: %v", flag, want, args)
		}
	}
}
func TestManagedVLLMProcessArgsRejectInvalidSavedResourceSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, vllmResourceSettingsFile), []byte(`{"gpu_memory_utilization":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managedVLLMProcessArgs(&engineState{installDir: dir}, []string{"serve", "/models/selected"}); err == nil || !strings.Contains(err.Error(), "gpu_memory_utilization") {
		t.Fatalf("invalid saved resources did not fail closed: %v", err)
	}
}
func TestVLLMManagedRankRejectsChangedOwner(t *testing.T) {
	b, p := managedRankFixture(t, 1)
	for _, change := range []func(*vllmGroupBinding, *vllmRankPlacement){func(b *vllmGroupBinding, _ *vllmRankPlacement) { b.PlanDigest = strings.Repeat("d", 64) }, func(_ *vllmGroupBinding, p *vllmRankPlacement) { p.LocalNode = "unselected" }, func(_ *vllmGroupBinding, p *vllmRankPlacement) { p.CoordinatorAddress = "https://unrelated.example" }, func(_ *vllmGroupBinding, p *vllmRankPlacement) { p.MasterPort = p.APIPort }} {
		copyB, copyP := b, p
		change(&copyB, &copyP)
		if _, err := vllmRankArgs("/managed/bin/vllm", copyB, copyP, vllmResourceSettings{}); err == nil {
			t.Fatal("changed placement admitted")
		}
	}
}
func TestVLLMManagedRankNormalStopAndRetainedReceipt(t *testing.T) {
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	b, p := managedRankFixture(t, 1)
	done := make(chan struct{})
	close(done)
	r := &vllmManagedRank{e: f.e, st: f.st, binding: b, placement: p, path: filepath.Join(f.st.installDir, "serving-rank.json"), proc: &managedProc{done: done}, receipt: vllmRankReceipt{RunID: b.RunID, Generation: b.Generation, PlanDigest: b.PlanDigest, Rank: b.Rank, State: "started"}}
	f.st.vllmRank = r
	f.st.proc = r.proc
	f.st.running = true
	wrong := b
	wrong.Generation++
	if r.stop(wrong) == nil {
		t.Fatal("different generation stopped owner")
	}
	if err := r.ready(context.Background(), b); err == nil {
		t.Fatal("dead participant is ready")
	}
	if err := f.e.doStop(f.st, "vllm"); err != nil {
		t.Fatal(err)
	}
	if f.st.running || f.st.proc != nil || r.reserved() {
		t.Fatal("normal Stop did not reconcile owned state")
	}
	var got vllmRankReceipt
	if err := readVLLMJSON(f.st.installDir, r.path, 4096, &got); err != nil {
		t.Fatal(err)
	}
	if !got.CleanupConfirmed || got.State != "stopped" || got.PlanDigest != b.PlanDigest {
		t.Fatal("cleanup receipt does not bind operation")
	}
	if err := r.stop(b); err != nil {
		t.Fatal("same completed stop is not idempotent", err)
	}
}
func TestVLLMManagedRankRuntimeDigestExcludesPathsOnly(t *testing.T) {
	a := vllmRuntimeReceipt{Version: "0.28.0", Architecture: "aarch64", WheelSHA256: strings.Repeat("a", 64), PythonSHA256: strings.Repeat("b", 64), PipReportSHA256: strings.Repeat("c", 64), Environment: "/node-a/runtime"}
	b := a
	b.Environment = "/node-b/runtime"
	if vllmRankRuntimeDigest(a) != vllmRankRuntimeDigest(b) {
		t.Fatal("node-local location changes content identity")
	}
	b.PythonSHA256 = strings.Repeat("d", 64)
	if vllmRankRuntimeDigest(a) == vllmRankRuntimeDigest(b) {
		t.Fatal("changed executable accepted")
	}
}

func TestVLLMManagedRankSchema2UsesInterpreterAndBindsInstallerCLI(t *testing.T) {
	st := managedVLLMTestState(t)
	id := "v0-group"
	dir := writeManagedVLLMTestEnvironment(t, st, id, managedVLLMVersion)
	python, cli, receipt, err := validateVLLMRuntimeBinaries(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if python != filepath.Join(dir, "venv", "bin", "python") || cli != filepath.Join(dir, "venv", "bin", "vllm") || receipt.Environment != dir {
		t.Fatalf("schema2 runtime paths python=%q cli=%q environment=%q", python, cli, receipt.Environment)
	}
	want := vllmRankRuntimeDigest(receipt)
	for name, change := range map[string]func(*vllmRuntimeReceipt){
		"recipe":    func(r *vllmRuntimeReceipt) { r.RecipeID += "-changed" },
		"installer": func(r *vllmRuntimeReceipt) { r.UVSHA256 = strings.Repeat("1", 64) },
		"python":    func(r *vllmRuntimeReceipt) { r.PythonSHA256 = strings.Repeat("2", 64) },
		"cli":       func(r *vllmRuntimeReceipt) { r.CLISHA256 = strings.Repeat("3", 64) },
		"dependency report": func(r *vllmRuntimeReceipt) {
			r.PipReportSHA256 = strings.Repeat("4", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			change(&changed)
			if vllmRankRuntimeDigest(changed) == want {
				t.Fatal("schema2 runtime digest omitted changed content")
			}
		})
	}
	changedPath := receipt
	changedPath.Environment = "/another/node/environment"
	if vllmRankRuntimeDigest(changedPath) != want {
		t.Fatal("node-local schema2 path changed content identity")
	}
	if err := validateVLLMGroupRunnableReceipt(receipt, want); err != nil {
		t.Fatalf("current schema2 receipt was not admitted: %v", err)
	}
	legacy := vllmRuntimeReceipt{Schema: vllmLegacyReceiptSchema, Version: "0.28.0", Architecture: receipt.Architecture, WheelSHA256: strings.Repeat("4", 64), PythonSHA256: strings.Repeat("5", 64), PipReportSHA256: strings.Repeat("6", 64)}
	if err := validateVLLMGroupRunnableReceipt(legacy, vllmRankRuntimeDigest(legacy)); err == nil {
		t.Fatal("legacy ownership receipt retained new serving-group activation authority")
	}
}

func TestVLLMManagedRankAdmitsExactRetainedRuntimeForNewGroup(t *testing.T) {
	var recipe vllmLegacyRecipe
	for _, candidate := range managedVLLMLegacyOwnershipRecipes {
		if candidate.Architecture == runtime.GOARCH {
			recipe = candidate
			break
		}
	}
	if recipe.Version == "" {
		t.Skipf("no retained runtime recipe for %s", runtime.GOARCH)
	}
	receipt := vllmRuntimeReceipt{
		Schema: vllmLegacyReceiptSchema, Version: recipe.Version, Architecture: recipe.Architecture,
		WheelURL: recipe.WheelURL, WheelSHA256: recipe.WheelSHA256,
		PythonSHA256: strings.Repeat("5", 64), PipReportSHA256: strings.Repeat("6", 64),
	}
	if err := validateVLLMGroupRunnableReceipt(receipt, vllmRankRuntimeDigest(receipt)); err != nil {
		t.Fatalf("exact retained runtime was refused for a fresh group: %v", err)
	}
	receipt.WheelSHA256 = strings.Repeat("7", 64)
	if err := validateVLLMGroupRunnableReceipt(receipt, vllmRankRuntimeDigest(receipt)); err == nil {
		t.Fatal("changed retained runtime recipe was admitted")
	}
}

func TestVLLMManagedRankAdmitsExactRetainedQwenProfile(t *testing.T) {
	receipt := vllmRuntimeReceipt{
		Schema: vllmLegacyReceiptSchema, Version: "0.28.1rc1.dev361+gd4d703caf", Architecture: "arm64",
		WheelURL:     vllmQwen38WheelURL,
		WheelSHA256:  "f45d026fe1a9c532e89eeb71f888aabd1523a19a4052ff67e3810f02c9fdee67",
		PythonSHA256: strings.Repeat("1", 64), PipReportSHA256: strings.Repeat("2", 64),
		RecipeID:            vllmQwen38RecipeID,
		RecipeSHA256:        vllmQwen38RecipeSHA256,
		RecipeReceiptSHA256: strings.Repeat("3", 64), BundleReceiptSHA256: strings.Repeat("4", 64),
		NodeID: "node-a",
	}
	if err := validateVLLMGroupRunnableReceipt(receipt, vllmRankRuntimeDigest(receipt)); err != nil {
		t.Fatalf("exact retained Qwen runtime was refused: %v", err)
	}
	receipt.RecipeSHA256 = strings.Repeat("5", 64)
	if err := validateVLLMGroupRunnableReceipt(receipt, vllmRankRuntimeDigest(receipt)); err == nil {
		t.Fatal("changed Qwen runtime recipe was admitted")
	}
}

func TestManagedVLLMSchema2RuntimeBinaryLinkStaysInsideEnvironment(t *testing.T) {
	st := managedVLLMTestState(t)
	id := "v0-linked-python"
	dir := writeManagedVLLMTestEnvironment(t, st, id, managedVLLMVersion)
	python := filepath.Join(dir, "venv", "bin", "python")
	content, err := os.ReadFile(python)
	if err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(dir, "python", "cpython")
	if err = os.MkdirAll(filepath.Dir(owned), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(owned, content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(python); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join("..", "..", "python", "cpython"), python); err != nil {
		t.Skipf("runtime symlink unavailable on this host: %v", err)
	}
	if _, _, _, err = validateVLLMRuntimeBinaries(st, id); err != nil {
		t.Fatalf("in-environment final interpreter link rejected: %v", err)
	}
	escaped := filepath.Join(t.TempDir(), "python")
	if err = os.WriteFile(escaped, content, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(python); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(escaped, python); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = validateVLLMRuntimeBinaries(st, id); err == nil {
		t.Fatal("escaping final interpreter link was admitted")
	}
}

func TestVLLMManagedRankLostParticipantWithdrawsAndCleansGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stops := 0
		g := vllmGroupTestOwner(t, func(_ context.Context, b vllmGroupBinding, action string) error {
			if action == "status" && b.Rank == 1 {
				return errors.New("ordinary participant exit")
			}
			if action == "stop" {
				stops++
			}
			return nil
		})
		// The composed C monitor owns health cadence; A no longer duplicates it.
		g.live = func(ctx context.Context, run vllmGroupRun) error {
			for rank := range run.Ranks {
				if err := g.call(ctx, g.binding(run, rank), "status"); err != nil {
					return err
				}
			}
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := vllmGroupTestStart(t, g, ctx, 2)
		synctest.Wait()
		if !g.coordinatorEligible("node-a", run.RunID, run.Generation) {
			t.Fatal("fixture did not reach ready")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if stops != 2 || g.coordinatorEligible("node-a", run.RunID, run.Generation) || !g.status().CleanupConfirmed || g.status().State != "failed" {
			t.Fatal("lost participant retained eligibility or skipped cleanup")
		}
	})
}
