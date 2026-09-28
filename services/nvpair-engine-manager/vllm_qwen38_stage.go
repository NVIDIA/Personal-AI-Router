// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Qwen3.8 preparation is an explicit opt-in operation. Ordinary managed vLLM
// Install and Update continue to select the current 0.29 recipe.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

var vllmQwen38OperationID = regexp.MustCompile(`^[0-9a-f]{32}$`)

var qwen38PipConflict = regexp.MustCompile(`^\S+ \S+ has requirement (\S+?)==(\S+?)(?:; .+)?, but you have (\S+) (\S+)\.$`)

// pip check reports every declared recipe override as a broken pin (torch pins
// the NCCL release the override replaces), so only those exact lines may pass.
func qwen38ConflictsAreDeclaredOverrides(output []byte, recipe vllmQwen38Recipe) bool {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		match := qwen38PipConflict.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			return false
		}
		declared := false
		for _, override := range recipe.Runtime.Overrides {
			if match[1] == override.Name && match[2] == override.FromVersion && match[3] == override.Name && match[4] == override.ToVersion {
				declared = true
			}
		}
		if !declared {
			return false
		}
	}
	return true
}

type vllmQwen38PrepareRequest struct {
	OperationID string `json:"operationId"`
	Cancel      bool   `json:"cancel,omitempty"`
}

type vllmQwen38PrepareResult struct {
	OperationID     string                        `json:"operationId"`
	State           string                        `json:"state"`
	RecipeID        string                        `json:"recipeId"`
	RecipeSHA256    string                        `json:"recipeSha256"`
	RuntimeVersion  string                        `json:"runtimeVersion"`
	ArtifactCount   int                           `json:"artifactCount"`
	ArtifactBytes   int64                         `json:"artifactBytes"`
	Resumed         bool                          `json:"resumed,omitempty"`
	ActiveRuntime   string                        `json:"activeRuntime,omitempty"`
	PreviousRuntime string                        `json:"previousRuntime,omitempty"`
	Message         string                        `json:"message,omitempty"`
	Provider        vllmQwen38ProviderObservation `json:"provider"`
}

type vllmQwen38PrepareOwner struct {
	operationID string
	cancel      context.CancelFunc
}

var vllmQwen38PrepareOwners = struct {
	sync.Mutex
	values map[*engineState]vllmQwen38PrepareOwner
}{values: make(map[*engineState]vllmQwen38PrepareOwner)}

func registerQwen38Prepare(st *engineState, operationID string, cancel context.CancelFunc) error {
	vllmQwen38PrepareOwners.Lock()
	defer vllmQwen38PrepareOwners.Unlock()
	if _, exists := vllmQwen38PrepareOwners.values[st]; exists {
		return errors.New("Qwen3.8 runtime preparation is already active")
	}
	vllmQwen38PrepareOwners.values[st] = vllmQwen38PrepareOwner{operationID: operationID, cancel: cancel}
	return nil
}

func unregisterQwen38Prepare(st *engineState, operationID string) {
	vllmQwen38PrepareOwners.Lock()
	defer vllmQwen38PrepareOwners.Unlock()
	if owner, exists := vllmQwen38PrepareOwners.values[st]; exists && owner.operationID == operationID {
		delete(vllmQwen38PrepareOwners.values, st)
	}
}

func cancelQwen38Prepare(st *engineState, operationID string) error {
	vllmQwen38PrepareOwners.Lock()
	owner, exists := vllmQwen38PrepareOwners.values[st]
	vllmQwen38PrepareOwners.Unlock()
	if !exists || owner.operationID != operationID {
		return errors.New("no matching Qwen3.8 runtime preparation is active")
	}
	owner.cancel()
	return nil
}

func qwen38PrepareResult(operationID, state string) vllmQwen38PrepareResult {
	return vllmQwen38PrepareResult{OperationID: operationID, State: state, RecipeID: vllmQwen38RecipeID, RecipeSHA256: vllmQwen38RecipeSHA256, RuntimeVersion: vllmQwen38Runtime, ArtifactCount: vllmQwen38ArtifactCount, ArtifactBytes: vllmQwen38ArtifactBytes}
}

func qwen38InstallEnvironment(st *engineState, runtimeDir string, values map[string]string) ([]string, error) {
	environment := map[string]string{}
	for _, entry := range managedVLLMRuntimeEnv(st, runtimeDir) {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, errors.New("managed Qwen3.8 environment is invalid")
		}
		environment[key] = value
	}
	environment["HOME"] = filepath.Join(runtimeDir, "home")
	for key, value := range values {
		if key == "" || strings.ContainsAny(key+value, "\x00\r\n") {
			return nil, errors.New("sealed Qwen3.8 environment is invalid")
		}
		environment[key] = value
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+environment[key])
	}
	return result, nil
}

func validateQwen38PipReport(st *engineState, path string, recipe vllmQwen38Recipe) (vllmDependencyReport, error) {
	var report vllmDependencyReport
	if err := readVLLMJSON(st.installDir, path, 16<<20, &report); err != nil || report.Version != "1" || len(report.Install) != len(recipe.Runtime.Artifacts) {
		return report, errors.New("Qwen3.8 complete offline pip report is unavailable")
	}
	want := make(map[string]vllmQwen38RecipeArtifact, len(recipe.Runtime.Artifacts))
	for _, artifact := range recipe.Runtime.Artifacts {
		want[normalizedVLLMDependencyName(artifact.Name)] = artifact
	}
	for _, entry := range report.Install {
		name := normalizedVLLMDependencyName(entry.Metadata.Name)
		artifact, exists := want[name]
		if !exists || entry.Metadata.Version != artifact.Version || entry.Download.Archive.Hashes["sha256"] != artifact.SHA256 {
			return report, errors.New("Qwen3.8 pip report contains an unsealed dependency")
		}
		delete(want, name)
	}
	if len(want) != 0 {
		return report, errors.New("Qwen3.8 pip report omitted sealed dependencies")
	}
	return report, nil
}

const qwen38RuntimeQualification = `import ctypes,importlib.metadata,torch,vllm,transformers,compressed_tensors
from vllm.model_executor.layers.quantization.modelopt import ModelOptNvFp4Config
from transformers import Qwen4ExpForCausalLM
assert importlib.metadata.version('vllm')=='` + vllmQwen38Runtime + `', 'PAIR_QWEN38_CAPABILITY:vllm-version'
assert importlib.metadata.version('torch')=='2.13.0', 'PAIR_QWEN38_CAPABILITY:torch-metadata'
assert str(torch.__version__)=='2.13.0+cu130', 'PAIR_QWEN38_CAPABILITY:torch-build'
nccl_dist=importlib.metadata.distribution('nvidia-nccl-cu13')
assert nccl_dist.version=='2.30.7', 'PAIR_QWEN38_CAPABILITY:nccl-metadata'
nccl_files=[entry for entry in (nccl_dist.files or ()) if str(entry).endswith('libnccl.so.2')]
assert len(nccl_files)==1, 'PAIR_QWEN38_CAPABILITY:nccl-library'
nccl=ctypes.CDLL(str(nccl_dist.locate_file(nccl_files[0]))); nccl_version=ctypes.c_int()
assert nccl.ncclGetVersion(ctypes.byref(nccl_version))==0 and nccl_version.value==23007, 'PAIR_QWEN38_CAPABILITY:nccl-library'
assert importlib.metadata.version('transformers')=='5.17.0', 'PAIR_QWEN38_CAPABILITY:transformers-version'
assert importlib.metadata.version('compressed-tensors')=='0.17.0', 'PAIR_QWEN38_CAPABILITY:compressed-tensors-version'
assert torch.version.cuda=='13.0' and torch.cuda.is_available(), 'PAIR_QWEN38_CAPABILITY:cuda-available'
assert any(torch.cuda.get_device_capability(i)>=(12,0) for i in range(torch.cuda.device_count())), 'PAIR_QWEN38_CAPABILITY:gpu-capability'
assert ModelOptNvFp4Config.__name__=='ModelOptNvFp4Config', 'PAIR_QWEN38_CAPABILITY:modelopt-class'
assert Qwen4ExpForCausalLM.__name__=='Qwen4ExpForCausalLM', 'PAIR_QWEN38_CAPABILITY:qwen-class'
print(importlib.metadata.version('vllm'))`

func qwen38CapabilityFailure(err error) error {
	if err != nil {
		for _, code := range []string{"vllm-version", "torch-metadata", "torch-build", "nccl-metadata", "nccl-library", "transformers-version", "compressed-tensors-version", "cuda-available", "gpu-capability", "modelopt-class", "qwen-class"} {
			if strings.Contains(err.Error(), "PAIR_QWEN38_CAPABILITY:"+code) {
				return fmt.Errorf("Qwen3.8 runtime capability qualification failed: %s", code)
			}
		}
	}
	return errors.New("Qwen3.8 runtime capability qualification failed")
}

const vllmQwen38LaunchReceiptFile = "pair-qwen38-launch.json"

type vllmQwen38LaunchReceipt struct {
	Schema                     int    `json:"schema"`
	RecipeID                   string `json:"recipeId"`
	RecipeSHA256               string `json:"recipeSha256"`
	RuntimeVersion             string `json:"runtimeVersion"`
	RuntimeCompatibilitySHA256 string `json:"runtimeCompatibilitySha256"`
	ProviderClosureSHA256      string `json:"providerClosureSha256"`
	Python                     string `json:"python"`
	SOABI                      string `json:"soabi"`
	Glibc                      string `json:"glibc"`
	Torch                      string `json:"torch"`
	CUDA                       string `json:"cuda"`
	NCCL                       string `json:"nccl"`
	Transformers               string `json:"transformers"`
	CompressedTensors          string `json:"compressedTensors"`
	ModelOptClass              string `json:"modelOptClass"`
	QwenClass                  string `json:"qwenClass"`
	VerifiedAt                 string `json:"verifiedAt"`
}

func (e *Executor) stageQwen38ManagedVLLM(ctx context.Context, st *engineState, operationID, bundleRoot string, bundleReceipt vllmQwen38PreparedBundleReceipt) (id string, resultErr error) {
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return "", err
	}
	if record.Staged != "" {
		return "", errors.New("a prior managed vLLM stage must be reconciled first")
	}
	parent := filepath.Join(st.installDir, "environments")
	if err = os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(parent, "v0.28.1rc1-gd4d703caf-")
	if err != nil {
		return "", err
	}
	id = filepath.Base(dir)
	record.Staged = id
	if err = writeVLLMRuntimeRecord(st, record); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	defer cleanupFailedManagedVLLMStage(st, id, &resultErr)
	for _, path := range []string{filepath.Join(dir, "home"), filepath.Join(dir, "empty-cwd")} {
		if err = os.Mkdir(path, 0o700); err != nil {
			return "", err
		}
	}
	pythonSource := "/usr/bin/python3.12"
	if info, statErr := os.Stat(pythonSource); statErr != nil || !info.Mode().IsRegular() {
		return "", errors.New("Qwen3.8 preparation requires the qualified /usr/bin/python3.12")
	}
	e.emitInstallProgress("vllm", "creating-qwen38-environment", 60)
	bootstrapEnv := []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"), "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1"}
	if _, err = runManagedVLLMCommand(ctx, dir, pythonSource, []string{"-I", "-B", "-m", "venv", "--copies", filepath.Join(dir, "venv")}, bootstrapEnv); err != nil {
		return "", err
	}
	plan, err := qwen38OfflineInstallPlan(bundleRoot, dir)
	if err != nil {
		return "", err
	}
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		return "", err
	}
	if err = writeManagedVLLMFile(st.installDir, plan.RequirementsPath, plan.Requirements); err != nil {
		return "", err
	}
	plan.Receipt.ProviderClosureSHA256 = bundleReceipt.Provider.ObservedClosureSHA256
	if err = writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmQwen38RecipeReceiptFile), plan.Receipt); err != nil {
		return "", err
	}
	env, err := qwen38InstallEnvironment(st, dir, plan.EnvironmentVariables)
	if err != nil {
		return "", err
	}
	policy := vllmQwen38CommandPolicy{StageRoot: dir, BundleRoot: bundleRoot, Unit: "nvpair-vllm-qwen38-" + operationID + ".service", Timeout: time.Duration(plan.TimeoutSeconds) * time.Second, MemoryMaxBytes: uint64(plan.MemoryMaxBytes), StageMaxBytes: uint64(plan.StageMaxBytes), FreeReserveBytes: uint64(plan.FreeReserveBytes), TasksMax: plan.TasksMax}
	e.emitInstallProgress("vllm", "installing-qwen38-offline", 75)
	python := filepath.Join(dir, "venv", "bin", "python")
	if _, err = runQwen38BoundedCommand(ctx, python, plan.Arguments, env, plan.WorkingDirectory, policy); err != nil {
		return "", err
	}
	report, err := validateQwen38PipReport(st, plan.ReportPath, recipe)
	if err != nil {
		return "", err
	}
	cli := filepath.Join(dir, "venv", "bin", "vllm")
	if _, err = managedVLLMFileHash(dir, cli); err != nil {
		return "", errors.New("Qwen3.8 installation did not provide its exact vLLM CLI")
	}
	provisional := vllmRuntimeReceipt{Schema: vllmEnvironmentReceiptSchema, RecipeID: vllmQwen38RecipeID, Engine: "vllm", Version: vllmQwen38Runtime, Architecture: "arm64", RecipeSHA256: vllmQwen38RecipeSHA256}
	facts, err := e.probeVLLMPython(ctx, python)
	if err != nil {
		return "", err
	}
	compatibility, _, err := vllmCompatibilityFromLegacyReport(report, facts, provisional)
	if err != nil || e.verifyVLLMInstalledDependencySet(ctx, python, env, compatibility) != nil {
		return "", errors.New("Qwen3.8 installed dependency closure differs from the sealed recipe")
	}
	if out, checkErr := e.runVLLMCommand(ctx, python, []string{"-I", "-B", "-m", "pip", "check"}, env); checkErr != nil && !qwen38ConflictsAreDeclaredOverrides(out, recipe) {
		return "", errors.New("Qwen3.8 offline environment dependency consistency failed")
	}
	qualified, err := e.runVLLMCommand(ctx, python, []string{"-I", "-B", "-c", qwen38RuntimeQualification}, env)
	if err != nil || strings.TrimSpace(string(qualified)) != vllmQwen38Runtime {
		return "", qwen38CapabilityFailure(err)
	}
	managedReport, err := e.runVLLMCommand(ctx, python, []string{"-I", "-B", "-c", managedVLLMDependencyReport}, env)
	if err != nil || validateManagedVLLMDependencyReport(managedReport) != nil {
		return "", errors.New("Qwen3.8 managed dependency content report failed")
	}
	managedReportPath := filepath.Join(dir, "pip-report.json")
	if err = writeManagedVLLMDependencyReport(st.installDir, managedReportPath, managedReport); err != nil {
		return "", err
	}
	var managed vllmManagedDependencyReport
	if json.Unmarshal(managedReport, &managed) != nil {
		return "", errors.New("Qwen3.8 managed dependency content report is invalid")
	}
	managedCompatibility, compatibilitySHA256, err := vllmCompatibilityFromManagedReport(managed, facts, provisional)
	if err != nil || e.verifyVLLMInstalledDependencySet(ctx, python, env, managedCompatibility) != nil {
		return "", errors.New("Qwen3.8 managed runtime compatibility differs from the installed environment")
	}
	launch := vllmQwen38LaunchReceipt{Schema: 1, RecipeID: vllmQwen38RecipeID, RecipeSHA256: vllmQwen38RecipeSHA256, RuntimeVersion: vllmQwen38Runtime, RuntimeCompatibilitySHA256: compatibilitySHA256, ProviderClosureSHA256: bundleReceipt.Provider.ObservedClosureSHA256, Python: facts.Python, SOABI: facts.SOABI, Glibc: facts.Glibc, Torch: "2.13.0+cu130", CUDA: "13.0", NCCL: "2.30.7", Transformers: "5.17.0", CompressedTensors: "0.17.0", ModelOptClass: "ModelOptNvFp4Config", QwenClass: "Qwen4ExpForCausalLM", VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmQwen38LaunchReceiptFile), launch); err != nil {
		return "", err
	}
	bundleReceipt.LaunchReceiptSHA256, err = managedVLLMFileHash(dir, filepath.Join(dir, vllmQwen38LaunchReceiptFile))
	if err != nil {
		return "", err
	}
	if err = writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmQwen38BundleReceiptFile), bundleReceipt); err != nil {
		return "", err
	}
	pythonHash, err := managedVLLMFileHash(dir, python)
	if err != nil {
		return "", err
	}
	cliHash, err := managedVLLMFileHash(dir, cli)
	if err != nil {
		return "", err
	}
	reportHash, err := managedVLLMFileHash(dir, managedReportPath)
	if err != nil {
		return "", err
	}
	recipeReceiptHash, err := managedVLLMFileHash(dir, filepath.Join(dir, vllmQwen38RecipeReceiptFile))
	if err != nil {
		return "", err
	}
	bundleReceiptHash, err := managedVLLMFileHash(dir, filepath.Join(dir, vllmQwen38BundleReceiptFile))
	if err != nil {
		return "", err
	}
	rootAbs, _ := filepath.Abs(st.installDir)
	dirAbs, _ := filepath.Abs(dir)
	receipt := vllmRuntimeReceipt{Schema: vllmEnvironmentReceiptSchema, RecipeID: vllmQwen38RecipeID, Engine: "vllm", OwnerRoot: rootAbs, Environment: dirAbs, Version: vllmQwen38Runtime, Architecture: runtime.GOARCH, PythonSHA256: pythonHash, CLISHA256: cliHash, PipReportSHA256: reportHash, RecipeSHA256: vllmQwen38RecipeSHA256, RecipeReceiptSHA256: recipeReceiptHash, BundleReceiptSHA256: bundleReceiptHash, NodeID: e.vllmNodeID, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmEnvironmentReceiptFile), receipt); err != nil {
		return "", err
	}
	if _, _, err = validateVLLMEnvironment(st, id); err != nil {
		return "", err
	}
	return id, nil
}

// The early marker is never rewritten in place: replacement runs through the
// normal staged activation transaction so the linked compatibility/launch
// receipts remain crash-consistent. This gate permits that replacement only
// after the old owned environment and its current provider still match.
func validateQwen38LegacyUVMarkerMigration(receipt vllmRuntimeReceipt, bundle vllmQwen38PreparedBundleReceipt, current vllmQwen38ProviderObservation) error {
	if !legacyQwen38UVMarkerReceipt(receipt) || bundle.Schema != 1 || bundle.Phase != vllmQwen38PreparedPhase ||
		bundle.RecipeID != receipt.RecipeID || bundle.RecipeSHA256 != receipt.RecipeSHA256 ||
		bundle.Provider.ObservedClosureSHA256 == "" {
		return errors.New("Qwen3.8 legacy uv marker is not an exact prepared runtime")
	}
	facts := &vllmQwen38GroupFacts{ProviderClosureSHA256: bundle.Provider.ObservedClosureSHA256}
	if err := validateCurrentQwen38Provider(facts, current); err != nil {
		return err
	}
	return nil
}

func (e *Executor) PrepareQwen38Runtime(parent context.Context, request vllmQwen38PrepareRequest) (vllmQwen38PrepareResult, error) {
	result := qwen38PrepareResult(request.OperationID, "preparing")
	if !vllmQwen38OperationID.MatchString(request.OperationID) {
		return result, errors.New("Qwen3.8 preparation requires a 32-character lowercase hexadecimal operationId")
	}
	st, err := e.state("vllm")
	if err != nil {
		return result, err
	}
	if request.Cancel {
		if err := cancelQwen38Prepare(st, request.OperationID); err != nil {
			return result, err
		}
		result.State, result.Message = "cancel-requested", "PAIR requested cancellation for the exact preparation"
		return result, nil
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		return result, errors.New("Qwen3.8 retained runtime preparation requires qualified Linux arm64")
	}
	if !st.opMu.TryLock() {
		return result, errors.New("vLLM lifecycle is busy; retry Qwen3.8 preparation after it settles")
	}
	defer st.opMu.Unlock()
	operationCtx, cancel := context.WithCancel(parent)
	if err := registerQwen38Prepare(st, request.OperationID, cancel); err != nil {
		cancel()
		return result, err
	}
	defer func() {
		cancel()
		unregisterQwen38Prepare(st, request.OperationID)
	}()
	ctx, finish, err := e.beginVLLMMutation(operationCtx, st)
	if err != nil {
		return result, err
	}
	defer finish()
	ctx, timeoutCancel := context.WithTimeout(ctx, vllmQwen38PrepareTimeout)
	defer timeoutCancel()
	if err = e.reconcileManagedVLLMActivation(ctx, st); err != nil {
		return result, err
	}
	if err = e.rejectVLLMGroupMutation("vllm", "prepare Qwen3.8 runtime for"); err != nil {
		return result, err
	}
	if !cableIdentifier(e.vllmNodeID, 128) {
		return result, errors.New("Qwen3.8 preparation requires the local PAIR node identity")
	}
	if supported, reason := installSupport("vllm", st.plat); !supported {
		return result, fmt.Errorf("Qwen3.8 preparation is unavailable: %s", reason)
	}
	if err = validateManagedVLLMLexicalPath(st.installDir, st.installDir); err != nil {
		return result, err
	}
	for _, path := range []string{st.installDir, st.modelDir, filepath.Join(st.installDir, "runtime-home")} {
		if err = os.MkdirAll(path, 0o700); err != nil {
			return result, err
		}
	}
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		return result, err
	}
	result.Provider = e.inspectQwen38Providers(ctx, recipe)
	record, err := readVLLMRuntimeRecord(st)
	if err != nil {
		return result, err
	}
	if record.Removing {
		return result, errors.New("vLLM removal is incomplete; retry Uninstall before Qwen3.8 preparation")
	}
	if record.Staged != "" {
		record, err = e.reconcileManagedVLLMStage(st, record)
		if err != nil {
			return result, err
		}
	}
	replacingLegacyMarker := false
	if record.Active != "" {
		_, active, validateErr := validateVLLMEnvironment(st, record.Active)
		if validateErr != nil {
			return result, validateErr
		}
		if legacyQwen38UVMarkerReceipt(active) {
			bundle, _, receiptErr := preparedQwen38ReceiptsForOwnership(st, active.Environment, active)
			if receiptErr != nil {
				return result, receiptErr
			}
			if err = validateQwen38LegacyUVMarkerMigration(active, bundle, result.Provider); err != nil {
				return result, err
			}
			replacingLegacyMarker = true
		}
		if currentPreparedQwen38Receipt(active) {
			if !result.Provider.Qualified {
				result.State = "blocked-provider"
				result.Message = "prepared runtime remains stopped because this node no longer matches a qualified provider profile"
				return result, nil
			}
			bundle, _, receiptErr := preparedQwen38Receipts(st, active.Environment, active)
			if receiptErr != nil {
				return result, receiptErr
			}
			if validateCurrentQwen38Provider(&vllmQwen38GroupFacts{ProviderClosureSHA256: bundle.Provider.ObservedClosureSHA256}, result.Provider) != nil {
				result.State = "blocked-provider"
				result.Message = "prepared runtime remains stopped because this node's provider closure changed after preparation"
				return result, nil
			}
			result.State, result.ActiveRuntime, result.PreviousRuntime = "ready", record.Active, record.Previous
			result.Message = "exact Qwen3.8 runtime is already prepared"
			return result, nil
		}
	}
	st.mu.Lock()
	running, adopted := st.running, st.adopted
	st.mu.Unlock()
	if running || adopted {
		return result, errors.New("stop managed vLLM before preparing the Qwen3.8 group-only runtime")
	}
	if err = e.ensureManagedVLLMPortFree(ctx, st); err != nil {
		return result, err
	}
	if !result.Provider.Qualified {
		result.State = "blocked-provider"
		result.Message = "target provider closure is outside the closed qualified Spark profiles; update the node through DGX Dashboard before preparation"
		return result, nil
	}
	bundleRoot, bundleReceipt, resumed, err := e.ensureQwen38RuntimeBundle(ctx, st, recipe, result.Provider)
	result.Resumed = resumed
	if err != nil {
		return result, err
	}
	id, err := e.stageQwen38ManagedVLLM(ctx, st, request.OperationID, bundleRoot, bundleReceipt)
	if err != nil {
		return result, err
	}
	staged, err := readVLLMRuntimeRecord(st)
	if err != nil || staged.Staged != id {
		return result, errors.Join(errors.New("Qwen3.8 staged ownership was not durably recorded"), err)
	}
	if err = e.activateManagedVLLM(ctx, st, staged, id, false, e.managedVLLMRuntimeControl(st)); err != nil {
		return result, err
	}
	committed, err := readVLLMRuntimeRecord(st)
	if err != nil || committed.Active != id || committed.Staged != "" || committed.Activating != nil {
		return result, errors.Join(errors.New("Qwen3.8 activation did not commit under the schema-2 lifecycle"), err)
	}
	result.State, result.ActiveRuntime, result.PreviousRuntime = "ready", committed.Active, committed.Previous
	result.Message = "exact Qwen3.8 runtime is prepared and stopped; serving requires a reviewed group"
	if replacingLegacyMarker {
		result.Message = "exact Qwen3.8 runtime is prepared with truthful Python-venv/offline-pip provenance; the validated early uv-marker environment was retained only as rollback ownership"
	}
	e.emitInstallProgress("vllm", "qwen38-runtime-ready", 100)
	e.emitState("vllm")
	return result, nil
}

func currentPreparedQwen38Receipt(receipt vllmRuntimeReceipt) bool {
	return receipt.Schema == vllmEnvironmentReceiptSchema && admittedRetainedQwen38Receipt(receipt)
}

func marshalQwen38PrepareResult(result vllmQwen38PrepareResult) json.RawMessage {
	raw, _ := json.Marshal(result)
	return raw
}

func (m *Manager) runQwen38Prepare(ctx context.Context, msg *Message) {
	var request vllmQwen38PrepareRequest
	if !m.parse(msg, &request) {
		return
	}
	result, err := m.exec.PrepareQwen38Runtime(ctx, request)
	m.respondOrErr(msg, result, err)
}
