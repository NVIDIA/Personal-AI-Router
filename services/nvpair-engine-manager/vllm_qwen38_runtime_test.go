// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQwen38RuntimeRecipeBindsExactEscapedArtifactClosure(t *testing.T) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Runtime.ArtifactCount != vllmQwen38ArtifactCount || len(recipe.Runtime.Artifacts) != vllmQwen38ArtifactCount || recipe.Runtime.ArtifactBytes != vllmQwen38ArtifactBytes {
		t.Fatalf("wrong sealed closure: %+v", recipe.Runtime)
	}
	if recipe.Runtime.Wheel.URL != vllmQwen38WheelURL || !strings.Contains(recipe.Runtime.Wheel.URL, "%2B") || strings.Contains(recipe.Runtime.Wheel.URL, "+gd4d703caf") {
		t.Fatalf("root wheel URL lost escaped plus: %q", recipe.Runtime.Wheel.URL)
	}
	plan, err := qwen38OfflineInstallPlan(filepath.Join(t.TempDir(), "wheels"), filepath.Join(t.TempDir(), "environment"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes := strings.Count(string(plan.Requirements), "\n"); bytes != vllmQwen38ArtifactCount || plan.Receipt.Schema != 2 || plan.Receipt.LifecycleSchema != vllmRuntimeRecordSchema || plan.Receipt.StageOwner != "stageQwen38ManagedVLLM" || filepath.Base(plan.ReportPath) != "pair-qwen38-pip-report.json" {
		t.Fatalf("offline schema-2 plan changed: lines=%d receipt=%+v report=%s", bytes, plan.Receipt, plan.ReportPath)
	}
}

func TestQwen38BoundedWorkerAllowsOnlyVenvLib64Link(t *testing.T) {
	stage, err := os.ReadFile("vllm_qwen38_stage.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stage), `[]string{"-I", "-B", "-m", "venv", "--copies", filepath.Join(dir, "venv")}`) {
		t.Fatal("Qwen3.8 stage no longer creates the reviewed copied venv")
	}
	worker, err := os.ReadFile("vllm_qwen38_command_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(worker)
	const allowed = "if root!=stage or path!=os.path.join(stage,'venv','lib64') or os.readlink(path)!='lib': raise RuntimeError('owned stage changed')"
	if !strings.Contains(text, allowed) || strings.Contains(text, "os.path.join(stage,'lib64')") {
		t.Fatal("Qwen3.8 worker must allow only the copied venv's lib64 -> lib link, never bundle or top-level links")
	}
}

func TestQwen38QualificationBindsTorchWheelAndCUDABuildSeparately(t *testing.T) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		t.Fatal(err)
	}
	var torch vllmQwen38RecipeArtifact
	for _, artifact := range recipe.Runtime.Artifacts {
		if artifact.Name == "torch" {
			torch = artifact
		}
	}
	if torch.Version != "2.13.0" || torch.Filename != "torch-2.13.0-cp312-cp312-manylinux_2_28_aarch64.whl" || recipe.HostQualification.Capability.Torch != "2.13.0+cu130" {
		t.Fatalf("sealed torch metadata/build identity changed: %+v", torch)
	}
	if !strings.Contains(qwen38RuntimeQualification, "importlib.metadata.version('torch')=='"+torch.Version+"'") ||
		!strings.Contains(qwen38RuntimeQualification, "str(torch.__version__)=='"+recipe.HostQualification.Capability.Torch+"'") ||
		strings.Contains(qwen38RuntimeQualification, "importlib.metadata.version('torch')=='"+recipe.HostQualification.Capability.Torch+"'") {
		t.Fatal("Qwen3.8 qualification conflates wheel metadata with the CUDA-tagged torch build")
	}
	if got := qwen38CapabilityFailure(errors.New("python exit: PAIR_QWEN38_CAPABILITY:torch-metadata secret=do-not-log")); got.Error() != "Qwen3.8 runtime capability qualification failed: torch-metadata" {
		t.Fatalf("qualification leaked raw command output: %v", got)
	}
	if got := qwen38CapabilityFailure(errors.New("python exit: secret=do-not-log")); got.Error() != "Qwen3.8 runtime capability qualification failed" {
		t.Fatalf("unknown qualification failure leaked raw command output: %v", got)
	}
}

func TestQwen38RuntimePinsSubnetAwareNCCLOverride(t *testing.T) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		t.Fatal(err)
	}
	var nccl vllmQwen38RecipeArtifact
	for _, artifact := range recipe.Runtime.Artifacts {
		if artifact.Name == "nvidia-nccl-cu13" {
			nccl = artifact
		}
	}
	if nccl.Version != "2.30.7" || nccl.Filename != "nvidia_nccl_cu13-2.30.7-py3-none-manylinux_2_18_aarch64.whl" || nccl.SHA256 != "ca786ffa5a647c75d4d1f5cc72a6c4f537947e2ba8823d7c8aaf768e7a7b9f77" || recipe.HostQualification.Capability.NCCL != "2.30.7" {
		t.Fatalf("peer-aware NCCL artifact is not sealed: %+v", nccl)
	}
	if len(recipe.Runtime.Overrides) != 1 || recipe.Runtime.Overrides[0].Name != "nvidia-nccl-cu13" || recipe.Runtime.Overrides[0].FromVersion != "2.29.7" || recipe.Runtime.Overrides[0].ToVersion != "2.30.7" {
		t.Fatalf("NCCL override provenance is missing: %+v", recipe.Runtime.Overrides)
	}
	for _, evidence := range []string{"importlib.metadata.distribution('nvidia-nccl-cu13')", "nccl_dist.version=='2.30.7'", "nccl_version.value==23007"} {
		if !strings.Contains(qwen38RuntimeQualification, evidence) {
			t.Fatalf("runtime qualification does not prove %q", evidence)
		}
	}
	for _, code := range []string{"nccl-metadata", "nccl-library"} {
		if got := qwen38CapabilityFailure(errors.New("python exit: PAIR_QWEN38_CAPABILITY:" + code + " secret=do-not-log")); got.Error() != "Qwen3.8 runtime capability qualification failed: "+code {
			t.Fatalf("NCCL failure classification changed: %v", got)
		}
	}
}

func TestQwen38ProviderObservationReportsExactDrift(t *testing.T) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		t.Fatal(err)
	}
	packages := map[string]string{}
	files := map[string]struct {
		hash  string
		bytes int64
	}{}
	for _, provider := range recipe.HostQualification.Providers {
		files[provider.Path] = struct {
			hash  string
			bytes int64
		}{provider.SHA256, provider.Bytes}
		for _, pkg := range provider.Packages {
			packages[pkg.Name] = pkg.Identity
		}
	}
	providerIO := vllmQwen38ProviderIO{
		hashFile: func(_ context.Context, path string) (string, int64, error) {
			value, ok := files[path]
			if !ok {
				return "", 0, os.ErrNotExist
			}
			return value.hash, value.bytes, nil
		},
		packageIdentity: func(_ context.Context, name string) (string, error) { return packages[name], nil },
	}
	exact := observeQwen38Providers(context.Background(), recipe, providerIO)
	if !exact.Qualified || exact.ObservedClosureSHA256 != vllmQwen38ProviderClosureSHA256 || len(exact.Mismatches) != 0 {
		t.Fatalf("exact provider closure refused: %+v", exact)
	}
	changed := files[recipe.HostQualification.Providers[0].Path]
	changed.hash = strings.Repeat("0", 64)
	files[recipe.HostQualification.Providers[0].Path] = changed
	drift := observeQwen38Providers(context.Background(), recipe, providerIO)
	if drift.Qualified || drift.ObservedClosureSHA256 == vllmQwen38ProviderClosureSHA256 || len(drift.Mismatches) == 0 || drift.Mismatches[0].ObservedSHA256 == "" {
		t.Fatalf("provider drift was not explicit: %+v", drift)
	}
}

func TestQwen38CurrentBCProviderProfileHasExactClosedIdentity(t *testing.T) {
	profile := qwen38CurrentBCProviderProfile()
	files := map[string]struct {
		hash  string
		bytes int64
	}{}
	packages := map[string]string{}
	for _, provider := range profile.Providers {
		files[provider.Path] = struct {
			hash  string
			bytes int64
		}{provider.SHA256, provider.Bytes}
		for _, pkg := range provider.Packages {
			packages[pkg.Name] = pkg.Identity
		}
	}
	observed := observeQwen38ProviderProfile(context.Background(), profile, vllmQwen38ProviderIO{
		hashFile: func(_ context.Context, path string) (string, int64, error) {
			value, ok := files[path]
			if !ok {
				return "", 0, os.ErrNotExist
			}
			return value.hash, value.bytes, nil
		},
		packageIdentity: func(_ context.Context, name string) (string, error) { return packages[name], nil },
	})
	if !observed.Qualified || observed.ProfileID != vllmQwen38ProviderProfileBC || observed.ObservedClosureSHA256 != vllmQwen38ProviderClosureBC {
		t.Fatalf("current B/C profile changed: %+v", observed)
	}
}

func TestQwen38ArtifactDownloadResumesAndVerifiesBeforePublish(t *testing.T) {
	content := []byte("sealed-runtime-artifact")
	digest := sha256.Sum256(content)
	var seenRange string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRange = r.Header.Get("Range")
		if seenRange == "bytes=7-" {
			w.Header().Set("Content-Range", "bytes 7-22/23")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[7:])
			return
		}
		_, _ = w.Write(content)
	}))
	defer server.Close()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := vllmQwen38RecipeArtifact{Name: "fixture", Version: "1", Filename: "fixture.whl", SHA256: hex.EncodeToString(digest[:]), URL: server.URL + "/fixture.whl", Bytes: int64(len(content))}
	partial := filepath.Join(root, artifact.Filename+".part")
	if err := os.WriteFile(partial, content[:7], 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &Executor{client: server.Client()}
	resumed, err := executor.fetchQwen38Artifact(context.Background(), root, artifact, func(int64) {})
	if err != nil || !resumed || seenRange != "bytes=7-" {
		t.Fatalf("resume failed: resumed=%v range=%q err=%v", resumed, seenRange, err)
	}
	got, err := os.ReadFile(filepath.Join(root, artifact.Filename))
	if err != nil || string(got) != string(content) {
		t.Fatalf("published artifact changed: %q %v", got, err)
	}
	bad := artifact
	bad.Filename, bad.SHA256 = "bad.whl", strings.Repeat("f", 64)
	if _, err = executor.fetchQwen38Artifact(context.Background(), root, bad, func(int64) {}); err == nil || !os.IsNotExist(func() error { _, statErr := os.Stat(filepath.Join(root, bad.Filename+".part")); return statErr }()) {
		t.Fatal("checksum failure did not remove the untrusted partial")
	}
}

func TestQwen38PrepareCancellationIsExactOperationBound(t *testing.T) {
	st := &engineState{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	operation := strings.Repeat("a", 32)
	if err := registerQwen38Prepare(st, operation, cancel); err != nil {
		t.Fatal(err)
	}
	defer unregisterQwen38Prepare(st, operation)
	if err := cancelQwen38Prepare(st, strings.Repeat("b", 32)); err == nil {
		t.Fatal("a different operation canceled the owner")
	}
	if err := cancelQwen38Prepare(st, operation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("exact preparation did not cancel")
	}
}

func preparedQwen38FactsFixture(t *testing.T) (*engineState, string, vllmRuntimeReceipt, *vllmQwen38GroupFacts) {
	t.Helper()
	st := managedVLLMTestState(t)
	dir := filepath.Join(st.installDir, "environments", "v0-qwen-schema2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := qwen38OfflineInstallPlan(filepath.Join(st.installDir, "runtime-artifacts", vllmQwen38RecipeSHA256), dir)
	if err != nil {
		t.Fatal(err)
	}
	plan.Receipt.ProviderClosureSHA256 = vllmQwen38ProviderClosureSHA256
	if err := writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmQwen38RecipeReceiptFile), plan.Receipt); err != nil {
		t.Fatal(err)
	}
	compatibility := strings.Repeat("e", 64)
	launch := vllmQwen38LaunchReceipt{Schema: 1, RecipeID: vllmQwen38RecipeID, RecipeSHA256: vllmQwen38RecipeSHA256, RuntimeVersion: vllmQwen38Runtime, RuntimeCompatibilitySHA256: compatibility, ProviderClosureSHA256: vllmQwen38ProviderClosureSHA256, Python: "3.12.3", SOABI: "cpython-312-aarch64-linux-gnu", Glibc: "2.39", Torch: "2.13.0+cu130", CUDA: "13.0", NCCL: "2.30.7", Transformers: "5.17.0", CompressedTensors: "0.17.0", ModelOptClass: "ModelOptNvFp4Config", QwenClass: "Qwen4ExpForCausalLM", VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	launchPath := filepath.Join(dir, vllmQwen38LaunchReceiptFile)
	if err := writeManagedVLLMJSON(st.installDir, launchPath, launch); err != nil {
		t.Fatal(err)
	}
	launchHash, _ := managedVLLMFileHash(st.installDir, launchPath)
	recipe, _ := qwen38RuntimeRecipe()
	artifacts := make([]vllmQwen38PreparedArtifact, 0, len(recipe.Runtime.Artifacts))
	for _, artifact := range recipe.Runtime.Artifacts {
		artifacts = append(artifacts, vllmQwen38PreparedArtifact{Filename: artifact.Filename, SHA256: artifact.SHA256, Bytes: artifact.Bytes})
	}
	var allowed []string
	for _, profile := range qwen38ProviderProfiles(recipe) {
		allowed = append(allowed, profile.ClosureSHA256)
	}
	bundle := vllmQwen38PreparedBundleReceipt{Schema: 1, Phase: vllmQwen38PreparedPhase, RecipeID: vllmQwen38RecipeID, RecipeSHA256: vllmQwen38RecipeSHA256, ArtifactClosureSHA256: vllmQwen38ArtifactClosureSHA256, ArtifactCount: vllmQwen38ArtifactCount, ArtifactBytes: vllmQwen38ArtifactBytes, PreparedAt: time.Now().UTC().Format(time.RFC3339Nano), LaunchReceiptSHA256: launchHash, Provider: vllmQwen38ProviderObservation{Qualified: true, ProfileID: vllmQwen38ProviderProfileA, ExpectedClosureSHA256: vllmQwen38ProviderClosureSHA256, ObservedClosureSHA256: vllmQwen38ProviderClosureSHA256, AllowedClosureSHA256: allowed}, Artifacts: artifacts}
	bundlePath := filepath.Join(dir, vllmQwen38BundleReceiptFile)
	if err := writeManagedVLLMJSON(st.installDir, bundlePath, bundle); err != nil {
		t.Fatal(err)
	}
	recipeHash, _ := managedVLLMFileHash(st.installDir, filepath.Join(dir, vllmQwen38RecipeReceiptFile))
	bundleHash, _ := managedVLLMFileHash(st.installDir, bundlePath)
	receipt := vllmRuntimeReceipt{Schema: vllmEnvironmentReceiptSchema, RecipeID: vllmQwen38RecipeID, Engine: "vllm", OwnerRoot: st.installDir, Environment: dir, Version: vllmQwen38Runtime, Architecture: "arm64", PythonSHA256: strings.Repeat("1", 64), CLISHA256: strings.Repeat("2", 64), PipReportSHA256: strings.Repeat("3", 64), RecipeSHA256: vllmQwen38RecipeSHA256, RecipeReceiptSHA256: recipeHash, BundleReceiptSHA256: bundleHash, NodeID: "node-a", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	modelDigest := strings.Repeat("b", 64)
	facts, err := qwen38GroupFacts(st, dir, receipt, modelDigest, compatibility, 2)
	if err != nil {
		t.Fatal(err)
	}
	return st, dir, receipt, facts
}

func preparedQwen38V1OwnershipFixture(t *testing.T) (*engineState, string, vllmRuntimeReceipt) {
	t.Helper()
	st, dir, receipt, _ := preparedQwen38FactsFixture(t)
	var recipeReceipt vllmQwen38RecipeReceipt
	recipePath := filepath.Join(dir, vllmQwen38RecipeReceiptFile)
	if err := readVLLMJSON(st.installDir, recipePath, 1<<20, &recipeReceipt); err != nil {
		t.Fatal(err)
	}
	recipeReceipt.RecipeID, recipeReceipt.RecipeSHA256 = vllmQwen38RecipeV1ID, vllmQwen38RecipeV1SHA256
	recipeReceipt.ArtifactClosureSHA256, recipeReceipt.ArtifactBytes = vllmQwen38ArtifactV1ClosureSHA256, vllmQwen38ArtifactV1Bytes
	if err := writeManagedVLLMJSON(st.installDir, recipePath, recipeReceipt); err != nil {
		t.Fatal(err)
	}
	var launch vllmQwen38LaunchReceipt
	launchPath := filepath.Join(dir, vllmQwen38LaunchReceiptFile)
	if err := readVLLMJSON(st.installDir, launchPath, 32<<10, &launch); err != nil {
		t.Fatal(err)
	}
	launch.RecipeID, launch.RecipeSHA256, launch.NCCL = vllmQwen38RecipeV1ID, vllmQwen38RecipeV1SHA256, ""
	if err := writeManagedVLLMJSON(st.installDir, launchPath, launch); err != nil {
		t.Fatal(err)
	}
	launchHash, _ := managedVLLMFileHash(st.installDir, launchPath)
	var bundle vllmQwen38PreparedBundleReceipt
	bundlePath := filepath.Join(dir, vllmQwen38BundleReceiptFile)
	if err := readVLLMJSON(st.installDir, bundlePath, 1<<20, &bundle); err != nil {
		t.Fatal(err)
	}
	bundle.RecipeID, bundle.RecipeSHA256 = vllmQwen38RecipeV1ID, vllmQwen38RecipeV1SHA256
	bundle.ArtifactClosureSHA256, bundle.ArtifactBytes = vllmQwen38ArtifactV1ClosureSHA256, vllmQwen38ArtifactV1Bytes
	bundle.LaunchReceiptSHA256 = launchHash
	for i := range bundle.Artifacts {
		if strings.HasPrefix(bundle.Artifacts[i].Filename, "nvidia_nccl_cu13-") {
			bundle.Artifacts[i] = vllmQwen38PreparedArtifact{Filename: "nvidia_nccl_cu13-2.29.7-py3-none-manylinux_2_18_aarch64.whl", SHA256: "674a12383e3c38a1bcccae7d4f3633b37852230b6047883cb2f4c2d1b36d9bf5", Bytes: 206014712}
		}
	}
	if err := writeManagedVLLMJSON(st.installDir, bundlePath, bundle); err != nil {
		t.Fatal(err)
	}
	recipeHash, _ := managedVLLMFileHash(st.installDir, recipePath)
	bundleHash, _ := managedVLLMFileHash(st.installDir, bundlePath)
	receipt.RecipeID, receipt.RecipeSHA256 = vllmQwen38RecipeV1ID, vllmQwen38RecipeV1SHA256
	receipt.RecipeReceiptSHA256, receipt.BundleReceiptSHA256 = recipeHash, bundleHash
	binDir := filepath.Join(dir, "venv", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pythonPath, cliPath, reportPath := filepath.Join(binDir, "python"), filepath.Join(binDir, "vllm"), filepath.Join(dir, "pip-report.json")
	for path, content := range map[string]string{pythonPath: "v1-python", cliPath: "v1-cli", reportPath: `{"version":"1","pip_version":"fixture","install":[]}`} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	receipt.PythonSHA256, _ = managedVLLMFileHash(dir, pythonPath)
	receipt.CLISHA256, _ = managedVLLMFileHash(dir, cliPath)
	receipt.PipReportSHA256, _ = managedVLLMFileHash(dir, reportPath)
	receipt.Engine, receipt.OwnerRoot, receipt.Environment = "vllm", st.installDir, dir
	receipt.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmEnvironmentReceiptFile), receipt); err != nil {
		t.Fatal(err)
	}
	return st, dir, receipt
}

func TestQwen38V1Schema2IsExactOwnershipOnlyAndReplaceableByV2(t *testing.T) {
	st, dir, receipt := preparedQwen38V1OwnershipFixture(t)
	if !ownedRetainedQwen38Receipt(receipt) || !recognizedManagedVLLMReceipt(st, receipt) {
		t.Fatal("exact v1 schema-2 ownership was lost")
	}
	if admittedRetainedQwen38Receipt(receipt) || admittedManagedVLLMReceipt(receipt) || validateVLLMGroupRunnableReceipt(receipt, "") == nil {
		t.Fatal("v1 schema-2 receipt regained fresh Review or Start authority")
	}
	if currentPreparedQwen38Receipt(receipt) {
		t.Fatal("v1 preparation short-circuited instead of staging v2")
	}
	if err := validatePreparedQwen38ReceiptsForOwnership(st, dir, receipt); err != nil {
		t.Fatalf("exact v1 sidecars were not usable for upgrade/rollback/removal: %v", err)
	}
	if err := validatePreparedQwen38Receipts(st, dir, receipt); err == nil {
		t.Fatal("v1 sidecars passed the current v2 preparation gate")
	}
	marker := receipt
	marker.UVURL, marker.UVSHA256 = vllmQwen38LegacyMarkerUVURL, vllmQwen38LegacyMarkerUVSHA256
	if !legacyQwen38UVMarkerReceipt(marker) || ownedRetainedQwen38Receipt(marker) {
		t.Fatal("v1 legacy marker was not isolated as migration-only ownership")
	}
	if managedVLLMRecoveryStartAllowed(marker) {
		t.Fatal("rollback could standalone-start the v1 migration marker")
	}
	bundle, _, err := preparedQwen38ReceiptsForOwnership(st, dir, marker)
	if err != nil || validateQwen38LegacyUVMarkerMigration(marker, bundle, bundle.Provider) != nil {
		t.Fatalf("exact v1 legacy marker could not enter v2 replacement: %v", err)
	}
	if managedVLLMRecoveryStartAllowed(receipt) {
		t.Fatal("rollback could restart the v1 serving-group-only runtime")
	}
	_, _, v2, _ := preparedQwen38FactsFixture(t)
	if !currentPreparedQwen38Receipt(v2) {
		t.Fatal("current v2 preparation was not recognized")
	}
	if managedVLLMRecoveryStartAllowed(v2) {
		t.Fatal("rollback could restart the v2 serving-group-only runtime")
	}
	ordinary := receipt
	ordinary.RecipeID, ordinary.RecipeSHA256 = "ordinary-owned-recipe", strings.Repeat("f", 64)
	if !managedVLLMRecoveryStartAllowed(ordinary) {
		t.Fatal("ordinary schema-2 recovery behavior changed")
	}
}

func TestQwen38V1Schema2OwnsListenerRollbackAndUninstall(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("exact Qwen runtime ownership is an arm64 Linux contract")
	}
	t.Run("listener and rollback", func(t *testing.T) {
		st, dir, receipt := preparedQwen38V1OwnershipFixture(t)
		id := filepath.Base(dir)
		if _, got, err := validateVLLMEnvironment(st, id); err != nil || got.RecipeID != vllmQwen38RecipeV1ID {
			t.Fatalf("v1 environment ownership = %+v, err=%v", got, err)
		}
		images, err := managedVLLMImages(st, id)
		if err != nil || len(images) != 2 || !managedVLLMImageMatches(st, id, images[0]) {
			t.Fatalf("v1 listener image ownership = %v, err=%v", images, err)
		}
		newID := "v0-current"
		writeManagedVLLMTestEnvironment(t, st, newID, managedVLLMVersion)
		record := vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: newID, Previous: id, Activating: &vllmActivationIntent{Candidate: newID, PriorActive: id, WasRunning: true}}
		if err := writeVLLMRuntimeRecord(st, record); err != nil {
			t.Fatal(err)
		}
		restarted := 0
		control := vllmRuntimeControl{stop: func() error { return nil }, start: func(context.Context) error { return nil }, recoveryStart: func(context.Context) error { restarted++; return nil }, detect: func(bool) error { return nil }, write: func(value vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, value) }}
		if err := (&Executor{reporter: NewReporter(nil)}).reconcileManagedVLLMActivationWithControl(context.Background(), st, control); err != nil {
			t.Fatal(err)
		}
		restored, err := readVLLMRuntimeRecord(st)
		if err != nil || restored.Active != id || restarted != 0 || !ownedRetainedQwen38Receipt(receipt) {
			t.Fatalf("v1 rollback = %+v restarted=%d err=%v", restored, restarted, err)
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		st, dir, _ := preparedQwen38V1OwnershipFixture(t)
		id := filepath.Base(dir)
		if err := writeVLLMRuntimeRecord(st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: id}); err != nil {
			t.Fatal(err)
		}
		var removed []string
		ex := &Executor{reporter: NewReporter(nil), client: newEngineHTTPClient(time.Second)}
		err := ex.uninstallManagedVLLMWithIO(context.Background(), st, func(path string) error {
			removed = append(removed, path)
			return os.RemoveAll(path)
		}, func(value vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, value) })
		if err != nil || len(removed) != 1 || removed[0] != dir {
			t.Fatalf("v1 uninstall removed=%v err=%v", removed, err)
		}
	})
}

func TestPreparedSchema2QwenFactsBindModelProviderTopologyAndFabric(t *testing.T) {
	_, _, _, qwen := preparedQwen38FactsFixture(t)
	if !qwen.PreparedSchema2 || validateQwen38GroupFacts(qwen, 2) != nil || !validSHA256(qwen.ExpertParallelBindingSHA256) || qwen.EPLBBindingSHA256 != "" {
		t.Fatalf("prepared facts were not admitted: %+v", qwen)
	}
	for name, change := range map[string]func(*vllmQwen38GroupFacts){
		"provider":      func(f *vllmQwen38GroupFacts) { f.ProviderClosureSHA256 = strings.Repeat("0", 64) },
		"model":         func(f *vllmQwen38GroupFacts) { f.ModelDigest = strings.Repeat("0", 64) },
		"compatibility": func(f *vllmQwen38GroupFacts) { f.RuntimeCompatibilitySHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *qwen
			change(&changed)
			if validateQwen38GroupFacts(&changed, 2) == nil {
				t.Fatal("changed prepared facts were accepted")
			}
		})
	}
	if validateQwen38GroupFacts(qwen, 3) == nil {
		t.Fatal("two-node prepared bindings were reused for three nodes")
	}

	_, fabric := qualifiedDirectFabricFixture(t)
	selection, facts, pins := groupPlanFactsFixture(2)
	selection.Model = vllmQwen38ModelID
	_, config, _, _ := qwen38MetadataFixture(t)
	principals := make([]string, 2)
	for index := range facts {
		selection.NodeIDs[index] = fabric.Public.Targets[index].NodeID
		facts[index].NodeID = selection.NodeIDs[index]
		principals[index] = fabric.Public.Targets[index].Principal
		facts[index].Model = selection.Model
		facts[index].ModelDigest = qwen.ModelDigest
		facts[index].RuntimeVersion = vllmQwen38Runtime
		facts[index].RuntimeCompatibilitySHA256 = qwen.RuntimeCompatibilitySHA256
		facts[index].Config = append([]byte(nil), config...)
		copyFacts := *qwen
		facts[index].Qwen38 = &copyFacts
	}
	if err := bindQwen38FabricFacts(selection, facts, principals, fabric.Public.OperationID, fabric.Public.QualificationDigest, fabric.Public.CandidateIPs); err != nil {
		t.Fatal(err)
	}
	if _, err := assembleVLLMGroupPlan(selection, facts, pins); err != nil {
		t.Fatalf("prepared schema-2 facts plus current fabric were refused: %v", err)
	}
	heterogeneous := *facts[1].Qwen38
	heterogeneous.ProviderClosureSHA256 = vllmQwen38ProviderClosureBC
	layout, _ := qwen38Layout(2)
	heterogeneous.ExpertParallelBindingSHA256 = qwen38ReviewBinding("expert-parallel", 2, layout, heterogeneous.ModelDigest, heterogeneous.RuntimeCompatibilitySHA256, heterogeneous.ProviderClosureSHA256)
	facts[1].Qwen38 = &heterogeneous
	if _, err := assembleVLLMGroupPlan(selection, facts, pins); err == nil || !strings.Contains(err.Error(), "different qualified provider closures") {
		t.Fatalf("heterogeneous provider group was admitted: %v", err)
	}
	facts[1].Qwen38 = qwen
	facts[1].fabric = nil
	if _, err := assembleVLLMGroupPlan(selection, facts, pins); err == nil {
		t.Fatal("prepared runtime was admitted after current fabric evidence disappeared")
	}
}

func TestPreparedSchema2QwenFactsRequireFreshMatchingProvider(t *testing.T) {
	_, _, _, facts := preparedQwen38FactsFixture(t)
	qualified := vllmQwen38ProviderObservation{
		Qualified:             true,
		ProfileID:             vllmQwen38ProviderProfileA,
		ExpectedClosureSHA256: vllmQwen38ProviderClosureSHA256,
		ObservedClosureSHA256: vllmQwen38ProviderClosureSHA256,
		AllowedClosureSHA256:  []string{vllmQwen38ProviderClosureSHA256, vllmQwen38ProviderClosureBC},
	}
	if err := validateCurrentQwen38Provider(facts, qualified); err != nil {
		t.Fatalf("matching current provider was refused: %v", err)
	}
	for name, change := range map[string]func(*vllmQwen38ProviderObservation){
		"unqualified": func(value *vllmQwen38ProviderObservation) { value.Qualified = false },
		"drifted": func(value *vllmQwen38ProviderObservation) {
			value.ProfileID = vllmQwen38ProviderProfileBC
			value.ExpectedClosureSHA256 = vllmQwen38ProviderClosureBC
			value.ObservedClosureSHA256 = vllmQwen38ProviderClosureBC
		},
		"not allowed": func(value *vllmQwen38ProviderObservation) { value.AllowedClosureSHA256 = nil },
		"mismatch": func(value *vllmQwen38ProviderObservation) {
			value.Mismatches = []vllmQwen38ProviderMismatch{{Name: "libcuda.so.1", Reason: "changed"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			current := qualified
			current.AllowedClosureSHA256 = append([]string(nil), qualified.AllowedClosureSHA256...)
			change(&current)
			if validateCurrentQwen38Provider(facts, current) == nil {
				t.Fatal("stale or incomplete current provider was admitted")
			}
		})
	}
}

func TestPreparedSchema2QwenLegacyUVMarkerMigrationIsNarrow(t *testing.T) {
	st, dir, truthful, _ := preparedQwen38FactsFixture(t)
	var bundle vllmQwen38PreparedBundleReceipt
	if err := readVLLMJSON(st.installDir, filepath.Join(dir, vllmQwen38BundleReceiptFile), 1<<20, &bundle); err != nil {
		t.Fatal(err)
	}
	legacy := truthful
	legacy.UVURL, legacy.UVSHA256 = vllmQwen38LegacyMarkerUVURL, vllmQwen38LegacyMarkerUVSHA256
	if !legacyQwen38UVMarkerReceipt(legacy) || admittedRetainedQwen38Receipt(legacy) || !recognizedManagedVLLMReceipt(st, legacy) {
		t.Fatal("the exact old marker was not isolated as migration-only ownership")
	}
	if validatePreparedQwen38Receipts(st, dir, legacy) == nil {
		t.Fatal("the old marker remained runnable without migration")
	}
	if err := validatePreparedQwen38ReceiptsForOwnership(st, dir, legacy); err != nil {
		t.Fatalf("the exact old marker lost its complete cleanup/migration evidence: %v", err)
	}
	current := bundle.Provider
	if err := validateQwen38LegacyUVMarkerMigration(legacy, bundle, current); err != nil {
		t.Fatalf("validated old marker could not enter replacement migration: %v", err)
	}
	if err := validatePreparedQwen38Receipts(st, dir, truthful); err != nil {
		t.Fatalf("migrated receipt did not pass the normal prepared validators: %v", err)
	}

	t.Run("receipt tamper", func(t *testing.T) {
		changed := legacy
		changed.RecipeSHA256 = strings.Repeat("0", 64)
		if err := validateQwen38LegacyUVMarkerMigration(changed, bundle, current); err == nil {
			t.Fatal("changed old marker was migrated")
		}
	})
	t.Run("bundle tamper", func(t *testing.T) {
		changed := bundle
		changed.RecipeSHA256 = strings.Repeat("0", 64)
		if err := validateQwen38LegacyUVMarkerMigration(legacy, changed, current); err == nil {
			t.Fatal("changed prepared bundle was migrated")
		}
	})
	t.Run("provider drift", func(t *testing.T) {
		changed := current
		changed.ProfileID = vllmQwen38ProviderProfileBC
		changed.ExpectedClosureSHA256 = vllmQwen38ProviderClosureBC
		changed.ObservedClosureSHA256 = vllmQwen38ProviderClosureBC
		if err := validateQwen38LegacyUVMarkerMigration(legacy, bundle, changed); err == nil {
			t.Fatal("provider-drifted old marker was migrated")
		}
	})
}
