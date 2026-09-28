// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

const (
	vllmQwen38RecipeID                    = "qwen38-flash-next-nvfp4-d4d703c-spark-a-v4"
	vllmQwen38RecipeSHA256                = "5870b327fa252f3ddf27730738449e0a7615256cb9bce5ee29ae816a3b37752e"
	vllmQwen38Runtime                     = "0.28.1rc1.dev361+gd4d703caf"
	vllmQwen38WheelURL                    = "https://wheels.vllm.ai/d4d703caf908786416585ceb1f369e2e0363358b/vllm-0.28.1rc1.dev361%2Bgd4d703caf-cp38-abi3-manylinux_2_28_aarch64.whl"
	vllmQwen38WheelSHA256                 = "f45d026fe1a9c532e89eeb71f888aabd1523a19a4052ff67e3810f02c9fdee67"
	vllmQwen38ArtifactClosureSHA256       = "47d74f2078e4f3fb393e4f4048bded2a0c0b39e76bb5b6eaeceea8b8a003b8d5"
	vllmQwen38ProviderClosureSHA256       = "8396cb056ada51dee9f92eeb11469c4573d967b2861b5c3c1ef5f7b4c1ec3c61"
	vllmQwen38ArtifactCount               = 196
	vllmQwen38ArtifactBytes         int64 = 3940635938
	// Recipe v1 remains ownership-only so an installed environment can be
	// inspected, upgraded, rolled back to a stopped pointer, or removed. It is
	// never part of fresh group/runtime admission.
	vllmQwen38RecipeV1ID                    = "qwen38-flash-next-nvfp4-d4d703c-spark-a-v1"
	vllmQwen38RecipeV1SHA256                = "28aa064d0aaa85b306caf0f59abefd4596f299316ea4031e67c5dd9688059bf9"
	vllmQwen38ArtifactV1ClosureSHA256       = "226ca38cb7eb19dd1210eac1fe1e41a62394e673c1be1cdb9471c49f95de0645"
	vllmQwen38ArtifactV1Bytes         int64 = 3930666769
	// Recipe v2 installed these exact runtime bytes but sealed a three-node
	// DP3/EP3/EPLB layout that cannot fit a Spark's memory. It is ownership-only
	// like v1.
	vllmQwen38RecipeV2ID     = "qwen38-flash-next-nvfp4-d4d703c-spark-a-v2"
	vllmQwen38RecipeV2SHA256 = "36bf9896c12e4cd0f7fb773f0ff4f6087396d30f159b0c4935846adc033a4127"
	// Recipe v3 installed the same bytes but sealed a three-node pipeline the
	// runtime refuses for this model. It is ownership-only like v1 and v2; v4
	// seals the same bytes for two Sparks only.
	vllmQwen38RecipeV3ID     = "qwen38-flash-next-nvfp4-d4d703c-spark-a-v3"
	vllmQwen38RecipeV3SHA256 = "0912e40de2779f4da9f15cbb59a2d819759710bc16b56a614b2e30982e701051"
	// Early schema-2 adaptation receipts used the ordinary installer's uv
	// identity as a catalog marker even though this recipe uses system Python,
	// venv and offline pip. It is recognized only for validated migration.
	vllmQwen38LegacyMarkerUVURL    = "https://github.com/astral-sh/uv/releases/download/0.12.17/uv-aarch64-unknown-linux-gnu.tar.gz"
	vllmQwen38LegacyMarkerUVSHA256 = "d636d1b678e9e7f367ecb22b46bd1cabbed234d6bc3b4d96365d2b507f72f86c"
)

//go:embed testdata/qwen38-runtime-recipe.json
var vllmQwen38RecipeJSON []byte

type vllmQwen38RecipeArtifact struct {
	Name, Version, Filename, SHA256, URL string
	Bytes                                int64 `json:"bytes"`
}

type vllmQwen38Recipe struct {
	Comment        string `json:"$comment"`
	Schema         int    `json:"schema"`
	ID             string `json:"id"`
	PreferredNodes int
	Model          struct {
		ID, Architecture, Quantization            string
		ContextLength, MinSequences, MaxSequences int
		MTP, DFlash                               bool
	}
	Runtime struct {
		Version, Commit, PlanSHA256, GraphSHA256 string
		ArtifactCount                            int
		ArtifactBytes                            int64
		Wheel                                    struct {
			Filename, SHA256, URL string
			Bytes                 int64
		}
		Artifacts []vllmQwen38RecipeArtifact
		Overrides []struct{ Name, FromVersion, ToVersion, Reason string }
	}
	Install struct {
		FreshVenv, EmptyCwd, NetworkFetch bool
		Flags                             []string
		TelemetryEnvironment              map[string]string
		Limits                            struct {
			TimeoutSeconds                                  int
			MemoryMaxBytes, StageMaxBytes, FreeReserveBytes int64
			TasksMax                                        int
			WholeCgroupCleanup                              bool
		}
		Owners   struct{ Stage, Activate, Rollback, Cleanup, ActivePointer string }
		Receipts []string
	}
	HostQualification struct {
		EvidenceSealSHA256, ReferenceHostUUID, Architecture, Python, SOABI, Glibc, SeedPip, Driver string
		DriverConsumers, MandatoryLoaderTargets, AbsoluteRunpathElfs, TrailingEmptyRunpathElfs     int
		Providers                                                                                  []struct {
			Name, Path, SHA256 string
			Bytes              int64
			Packages           []struct{ Name, Identity string }
		}
		Capability struct{ Torch, CUDA, NCCL, VLLM, Transformers, CompressedTensors, ModelOptClass, ModelOptQuantMethod, Qwen4ExpClass string }
	}
	Layouts       []vllmQwen38Layout
	RequiredGates []string
}

type vllmQwen38InstallPlan struct {
	RecipeID, RecipeSHA256, RootWheel, WheelsDirectory, Environment, WorkingDirectory, RequirementsPath, ReportPath string
	Arguments                                                                                                       []string
	EnvironmentVariables                                                                                            map[string]string
	TimeoutSeconds, TasksMax                                                                                        int
	MemoryMaxBytes, StageMaxBytes, FreeReserveBytes                                                                 int64
	WholeCgroupCleanup                                                                                              bool
	Owners                                                                                                          map[string]string
	Receipts                                                                                                        []string
	Requirements                                                                                                    []byte
	Receipt                                                                                                         vllmQwen38RecipeReceipt
}

type vllmQwen38RecipeReceipt struct {
	Schema, LifecycleSchema, ArtifactCount, DriverConsumers                   int
	RecipeID, RecipeSHA256                                                    string
	RuntimeVersion, RuntimeCommit                                             string
	WheelSHA256, ArtifactClosureSHA256                                        string
	ProviderClosureSHA256, ReferenceProviderClosureSHA256, EvidenceSealSHA256 string
	ArtifactBytes                                                             int64
	TimeoutSeconds, TasksMax                                                  int
	MemoryMaxBytes, StageMaxBytes, FreeReserveBytes                           int64
	WholeCgroupCleanup                                                        bool
	Offline, EmptyCwd, TelemetryDisabled                                      bool
	StageOwner, ActivateOwner                                                 string
	RollbackOwner, CleanupOwner, ActivePointer                                string
}

type vllmQwen38Admission struct {
	RecipeSHA256                           string
	Nodes                                  int
	Layout                                 vllmQwen38Layout
	ArtifactBundleReady, SnapshotReady     bool
	StorageReady                           bool
	ProviderLoaderReady, ExpertOwnerReady  bool
	EPLBOwnerReady, RDMAReady, LaunchReady bool
	MTP, DFlash                            bool
}

func decodeQwen38RuntimeRecipe(raw []byte) (vllmQwen38Recipe, error) {
	var recipe vllmQwen38Recipe
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recipe); err != nil {
		return recipe, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return recipe, errors.New("Qwen3.8 runtime recipe has trailing content")
	}
	return recipe, nil
}

func qwen38RuntimeRecipe() (vllmQwen38Recipe, error) {
	return validateQwen38RuntimeRecipeBytes(vllmQwen38RecipeJSON)
}

func validateQwen38RuntimeRecipeBytes(raw []byte) (vllmQwen38Recipe, error) {
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != vllmQwen38RecipeSHA256 {
		return vllmQwen38Recipe{}, errors.New("Qwen3.8 runtime recipe bytes changed")
	}
	recipe, err := decodeQwen38RuntimeRecipe(raw)
	if err != nil {
		return recipe, err
	}
	return recipe, validateQwen38RuntimeRecipe(recipe)
}

func validateQwen38RuntimeRecipe(recipe vllmQwen38Recipe) error {
	profile := qwen38Profile()
	if recipe.Schema != 1 || recipe.ID != vllmQwen38RecipeID || recipe.PreferredNodes != 2 || recipe.Model.ID != profile.ModelID || recipe.Model.Architecture != profile.Architecture || recipe.Model.Quantization != profile.Quantization || recipe.Model.ContextLength != profile.ContextLength || recipe.Model.MinSequences != profile.MinSequences || recipe.Model.MaxSequences != profile.MaxSequences || recipe.Model.MTP || recipe.Model.DFlash {
		return errors.New("Qwen3.8 runtime recipe identity or model profile changed")
	}
	if recipe.Runtime.Version != vllmQwen38Runtime || recipe.Runtime.Commit != vllmQwen38MinimumCommit || recipe.Runtime.Wheel.SHA256 != vllmQwen38WheelSHA256 || recipe.Runtime.Wheel.URL != vllmQwen38WheelURL || recipe.Runtime.Wheel.Bytes != 310929989 || recipe.Runtime.ArtifactCount != vllmQwen38ArtifactCount || recipe.Runtime.ArtifactBytes != vllmQwen38ArtifactBytes || recipe.Runtime.PlanSHA256 != "43bf769f7b7b1c43919db3ec3452451bf4271eb23719b57583117a8451f668a0" || recipe.Runtime.GraphSHA256 != "022731c43ac9e46e301d19d4fa1e5bca22bef30e62b1b771b0878d2e6f77e52b" {
		return errors.New("Qwen3.8 runtime or acquisition provenance changed")
	}
	rootURL, err := url.Parse(recipe.Runtime.Wheel.URL)
	if err != nil || rootURL.Scheme != "https" || rootURL.Host != "wheels.vllm.ai" || recipe.Runtime.Wheel.Filename != "vllm-0.28.1rc1.dev361+gd4d703caf-cp38-abi3-manylinux_2_28_aarch64.whl" {
		return errors.New("Qwen3.8 root wheel identity changed")
	}
	if len(recipe.Runtime.Artifacts) != recipe.Runtime.ArtifactCount {
		return errors.New("Qwen3.8 artifact closure is incomplete")
	}
	// The upstream vLLM resolution is retained as provenance, while PAIR owns
	// this one explicit binary-compatible override. NCCL 2.29 cannot select a
	// peer-reachable HCA in the switchless three-Spark /31 ring; NVIDIA's 2.30
	// subnet-aware routing does.
	if len(recipe.Runtime.Overrides) != 1 || recipe.Runtime.Overrides[0].Name != "nvidia-nccl-cu13" || recipe.Runtime.Overrides[0].FromVersion != "2.29.7" || recipe.Runtime.Overrides[0].ToVersion != "2.30.7" || recipe.Runtime.Overrides[0].Reason != "nvidia-dgx-spark-switchless-ring-subnet-routing" {
		return errors.New("Qwen3.8 runtime override provenance changed")
	}
	seen, filenames, foundRoot, total, previous := map[string]bool{}, map[string]bool{}, false, int64(0), ""
	artifactClosure := sha256.New()
	for _, artifact := range recipe.Runtime.Artifacts {
		name := normalizedVLLMDependencyName(artifact.Name)
		u, parseErr := url.Parse(artifact.URL)
		if !vllmDependencyName.MatchString(artifact.Name) || !vllmDependencyVersion.MatchString(artifact.Version) || !onboardingSHA.MatchString(artifact.SHA256) || artifact.Bytes <= 0 || filepath.Base(artifact.Filename) != artifact.Filename || parseErr != nil || u.Scheme != "https" || u.User != nil || seen[name] || filenames[artifact.Filename] || previous != "" && name <= previous {
			return errors.New("Qwen3.8 artifact has invalid exact provenance")
		}
		seen[name], filenames[artifact.Filename], total, previous = true, true, total+artifact.Bytes, name
		fmt.Fprintf(artifactClosure, "%s\x00%s\x00%s\x00%d\x00%s\x00%s\n", name, artifact.Version, artifact.Filename, artifact.Bytes, artifact.SHA256, artifact.URL)
		if name == "vllm" {
			foundRoot = artifact.Version == recipe.Runtime.Version && artifact.Filename == recipe.Runtime.Wheel.Filename && artifact.SHA256 == recipe.Runtime.Wheel.SHA256 && artifact.URL == recipe.Runtime.Wheel.URL && artifact.Bytes == recipe.Runtime.Wheel.Bytes
		}
	}
	if !foundRoot || total != recipe.Runtime.ArtifactBytes || hex.EncodeToString(artifactClosure.Sum(nil)) != vllmQwen38ArtifactClosureSHA256 {
		return errors.New("Qwen3.8 root wheel or complete artifact byte closure changed")
	}
	wantFlags := []string{"--no-index", "--no-deps", "--require-hashes", "--only-binary=:all:", "--no-cache-dir", "--disable-pip-version-check"}
	wantOwners := []string{"stageVLLMEnvironment", "activateVLLMEnvironment", "restoreVLLMRuntime", "uninstallVLLMWithIO", vllmRuntimeRecordFile}
	wantReceipts := []string{"pip-report.json", vllmEnvironmentReceiptFile, "pair-runtime-compatibility.json", "pair-qwen38-recipe.json", vllmRuntimeRecordFile}
	owners := []string{recipe.Install.Owners.Stage, recipe.Install.Owners.Activate, recipe.Install.Owners.Rollback, recipe.Install.Owners.Cleanup, recipe.Install.Owners.ActivePointer}
	if !recipe.Install.FreshVenv || !recipe.Install.EmptyCwd || recipe.Install.NetworkFetch || !slices.Equal(recipe.Install.Flags, wantFlags) || !slices.Equal(owners, wantOwners) || !slices.Equal(recipe.Install.Receipts, wantReceipts) || recipe.Install.Limits.TimeoutSeconds != 1800 || recipe.Install.Limits.MemoryMaxBytes != 24<<30 || recipe.Install.Limits.StageMaxBytes != 24<<30 || recipe.Install.Limits.FreeReserveBytes != 40<<30 || recipe.Install.Limits.TasksMax != 512 || !recipe.Install.Limits.WholeCgroupCleanup {
		return errors.New("Qwen3.8 offline install, limits, rollback, or cleanup contract changed")
	}
	for name, value := range map[string]string{"PIP_NO_INDEX": "1", "PIP_NO_INPUT": "1", "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1", "HF_HUB_DISABLE_TELEMETRY": "1", "VLLM_NO_USAGE_STATS": "1", "DO_NOT_TRACK": "1"} {
		if recipe.Install.TelemetryEnvironment[name] != value {
			return errors.New("Qwen3.8 offline or telemetry environment changed")
		}
	}
	q := recipe.HostQualification
	if q.EvidenceSealSHA256 != "26dc87b831efefd858f3ac494c204abe7c092bd4f87e84bebd5786f7c0128011" || q.ReferenceHostUUID != "811d7911-8843-466c-b128-512fa4838895" || q.Architecture != "arm64" || q.Python != "3.12.3" || q.SOABI != "cpython-312-aarch64-linux-gnu" || q.Glibc != "2.39" || q.SeedPip != "24.0" || q.Driver != "580.95.05" || q.DriverConsumers != 14 || q.MandatoryLoaderTargets != 17 || q.AbsoluteRunpathElfs != 10 || q.TrailingEmptyRunpathElfs != 3 || len(q.Providers) != 9 {
		return errors.New("Qwen3.8 qualified Spark provider or loader contract changed")
	}
	providerNames, libcuda, previousProvider := map[string]bool{}, false, ""
	providerClosure := sha256.New()
	for _, provider := range q.Providers {
		if providerNames[provider.Name] || previousProvider != "" && provider.Name <= previousProvider || !onboardingSHA.MatchString(provider.SHA256) || provider.Bytes <= 0 || !strings.HasPrefix(provider.Path, "/usr/lib/aarch64-linux-gnu/") || len(provider.Packages) == 0 {
			return errors.New("Qwen3.8 provider provenance is incomplete")
		}
		providerNames[provider.Name], previousProvider = true, provider.Name
		fmt.Fprintf(providerClosure, "%s\x00%s\x00%d\x00%s\n", provider.Name, provider.Path, provider.Bytes, provider.SHA256)
		for _, pkg := range provider.Packages {
			fmt.Fprintf(providerClosure, "%s\x00%s\n", pkg.Name, pkg.Identity)
		}
		if provider.Name == "libcuda.so.1" {
			libcuda = provider.Path == "/usr/lib/aarch64-linux-gnu/libcuda.so.580.95.05" && provider.SHA256 == "251f9f693581a092c1839e95a43c4684669c02f06efcff648f8b9bee4da55390"
		}
	}
	if !libcuda || hex.EncodeToString(providerClosure.Sum(nil)) != vllmQwen38ProviderClosureSHA256 || q.Capability.Torch != "2.13.0+cu130" || q.Capability.CUDA != "13.0" || q.Capability.NCCL != "2.30.7" || q.Capability.VLLM != recipe.Runtime.Version || q.Capability.Transformers != "5.17.0" || q.Capability.CompressedTensors != "0.17.0" || q.Capability.ModelOptClass != "ModelOptNvFp4Config" || q.Capability.ModelOptQuantMethod != "NVFP4" || q.Capability.Qwen4ExpClass != "Qwen4ExpForCausalLM" {
		return errors.New("Qwen3.8 driver or capability qualification changed")
	}
	if len(recipe.Layouts) != len(profile.Layouts) || !slices.Equal(recipe.Layouts, profile.Layouts) {
		return errors.New("Qwen3.8 supports only the exact two-Spark TP2+EP2 layout")
	}
	wantGates := []string{"exact-recipe", "sealed-wheel-bundle-every-member", "complete-local-snapshot-every-member", "storage-review-every-member", "provider-loader-qualification-every-member", "expert-parallel-owner", "rdma-admission", "qwen-launch-capability"}
	if !slices.Equal(recipe.RequiredGates, wantGates) {
		return errors.New("Qwen3.8 fail-closed gate set changed")
	}
	return nil
}

func qwen38HashedRequirements(recipe vllmQwen38Recipe) ([]byte, error) {
	if err := validateQwen38RuntimeRecipe(recipe); err != nil {
		return nil, err
	}
	artifacts := slices.Clone(recipe.Runtime.Artifacts)
	slices.SortFunc(artifacts, func(a, b vllmQwen38RecipeArtifact) int {
		return strings.Compare(normalizedVLLMDependencyName(a.Name), normalizedVLLMDependencyName(b.Name))
	})
	var body strings.Builder
	for _, artifact := range artifacts {
		fmt.Fprintf(&body, "%s==%s --hash=sha256:%s\n", normalizedVLLMDependencyName(artifact.Name), artifact.Version, artifact.SHA256)
	}
	return []byte(body.String()), nil
}

func qwen38OfflineInstallPlan(wheelsDir, environment string) (vllmQwen38InstallPlan, error) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		return vllmQwen38InstallPlan{}, err
	}
	if !filepath.IsAbs(wheelsDir) || !filepath.IsAbs(environment) || filepath.Clean(wheelsDir) != wheelsDir || filepath.Clean(environment) != environment {
		return vllmQwen38InstallPlan{}, errors.New("Qwen3.8 recipe requires exact absolute owned stage paths")
	}
	requirements, err := qwen38HashedRequirements(recipe)
	if err != nil {
		return vllmQwen38InstallPlan{}, err
	}
	requirementsPath, reportPath := filepath.Join(environment, "pair-qwen38-requirements.txt"), filepath.Join(environment, "pair-qwen38-pip-report.json")
	args := []string{"-I", "-B", "-m", "pip", "install"}
	args = append(args, recipe.Install.Flags...)
	args = append(args, "--find-links", wheelsDir, "--report", reportPath, "--requirement", requirementsPath)
	receipt := vllmQwen38RecipeReceipt{Schema: 2, LifecycleSchema: vllmRuntimeRecordSchema, ArtifactCount: recipe.Runtime.ArtifactCount, DriverConsumers: recipe.HostQualification.DriverConsumers, RecipeID: recipe.ID, RecipeSHA256: vllmQwen38RecipeSHA256, RuntimeVersion: recipe.Runtime.Version, RuntimeCommit: recipe.Runtime.Commit, WheelSHA256: recipe.Runtime.Wheel.SHA256, ArtifactClosureSHA256: vllmQwen38ArtifactClosureSHA256, ProviderClosureSHA256: vllmQwen38ProviderClosureSHA256, ReferenceProviderClosureSHA256: vllmQwen38ProviderClosureSHA256, EvidenceSealSHA256: recipe.HostQualification.EvidenceSealSHA256, ArtifactBytes: recipe.Runtime.ArtifactBytes, TimeoutSeconds: recipe.Install.Limits.TimeoutSeconds, TasksMax: recipe.Install.Limits.TasksMax, MemoryMaxBytes: recipe.Install.Limits.MemoryMaxBytes, StageMaxBytes: recipe.Install.Limits.StageMaxBytes, FreeReserveBytes: recipe.Install.Limits.FreeReserveBytes, WholeCgroupCleanup: recipe.Install.Limits.WholeCgroupCleanup, Offline: !recipe.Install.NetworkFetch, EmptyCwd: recipe.Install.EmptyCwd, TelemetryDisabled: true, StageOwner: "stageQwen38ManagedVLLM", ActivateOwner: "activateManagedVLLM", RollbackOwner: "restoreManagedVLLM", CleanupOwner: "uninstallManagedVLLMWithIO", ActivePointer: vllmRuntimeRecordFile}
	return vllmQwen38InstallPlan{RecipeID: recipe.ID, RecipeSHA256: vllmQwen38RecipeSHA256, RootWheel: filepath.Join(wheelsDir, recipe.Runtime.Wheel.Filename), WheelsDirectory: wheelsDir, Environment: environment, WorkingDirectory: filepath.Join(environment, "empty-cwd"), RequirementsPath: requirementsPath, ReportPath: reportPath, Arguments: args, EnvironmentVariables: recipe.Install.TelemetryEnvironment, TimeoutSeconds: recipe.Install.Limits.TimeoutSeconds, TasksMax: recipe.Install.Limits.TasksMax, MemoryMaxBytes: recipe.Install.Limits.MemoryMaxBytes, StageMaxBytes: recipe.Install.Limits.StageMaxBytes, FreeReserveBytes: recipe.Install.Limits.FreeReserveBytes, WholeCgroupCleanup: recipe.Install.Limits.WholeCgroupCleanup, Owners: map[string]string{"stage": recipe.Install.Owners.Stage, "activate": recipe.Install.Owners.Activate, "rollback": recipe.Install.Owners.Rollback, "cleanup": recipe.Install.Owners.Cleanup, "activePointer": recipe.Install.Owners.ActivePointer}, Receipts: slices.Clone(recipe.Install.Receipts), Requirements: requirements, Receipt: receipt}, nil
}

func admitQwen38Runtime(value vllmQwen38Admission) (vllmQwen38Layout, error) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil || value.RecipeSHA256 != vllmQwen38RecipeSHA256 {
		return vllmQwen38Layout{}, errors.New("Qwen3.8 exact runtime recipe is required")
	}
	want, err := qwen38Layout(value.Nodes)
	if err != nil {
		return vllmQwen38Layout{}, err
	}
	if value.Layout != want || value.MTP || value.DFlash {
		return vllmQwen38Layout{}, errors.New("Qwen3.8 layout must be TP2+EP2; MTP and DFlash are unsupported")
	}
	if !value.ArtifactBundleReady || !value.SnapshotReady || !value.StorageReady || !value.ProviderLoaderReady || !value.ExpertOwnerReady || !value.RDMAReady || !value.LaunchReady || want.EPLB && !value.EPLBOwnerReady {
		return vllmQwen38Layout{}, fmt.Errorf("Qwen3.8 %s remains held: sealed wheel bundle, snapshot, storage, provider/loader, EP, EPLB when applicable, RDMA and launch gates must all pass", recipe.ID)
	}
	return want, nil
}

func qwen38ServingGroupRequired() error {
	profile := qwen38Profile()
	return fmt.Errorf("%s uses managed runtime recipe %s and must be started through a reviewed two-Spark serving group using %s; standalone Load is unsupported", profile.ModelID, vllmQwen38RecipeID, profile.Layouts[0].Label)
}

// admittedRetainedQwen38Receipt accepts the original sealed schema-1 capture
// and this adaptation's schema-2 environment. The latter keeps the exact
// runtime bytes while moving activation, rollback and cleanup under the current
// Engine Manager lifecycle.
func admittedRetainedQwen38Receipt(receipt vllmRuntimeReceipt) bool {
	if !qwen38ReceiptCommon(receipt) {
		return false
	}
	if receipt.Schema == vllmLegacyReceiptSchema {
		return receipt.WheelURL == vllmQwen38WheelURL && receipt.WheelSHA256 == vllmQwen38WheelSHA256
	}
	return receipt.Schema == vllmEnvironmentReceiptSchema && receipt.UVURL == "" && receipt.UVSHA256 == "" &&
		validSHA256(receipt.CLISHA256) && receipt.WheelURL == "" && receipt.WheelSHA256 == "" && receipt.PythonSource == ""
}

func ownedRetainedQwen38Receipt(receipt vllmRuntimeReceipt) bool {
	if !qwen38OwnershipReceiptCommon(receipt) {
		return false
	}
	if receipt.Schema == vllmLegacyReceiptSchema {
		return receipt.WheelURL == vllmQwen38WheelURL && receipt.WheelSHA256 == vllmQwen38WheelSHA256
	}
	return receipt.Schema == vllmEnvironmentReceiptSchema && receipt.UVURL == "" && receipt.UVSHA256 == "" &&
		validSHA256(receipt.CLISHA256) && receipt.WheelURL == "" && receipt.WheelSHA256 == "" && receipt.PythonSource == ""
}

func qwen38ReceiptCommon(receipt vllmRuntimeReceipt) bool {
	return qwen38ReceiptBase(receipt) && receipt.RecipeID == vllmQwen38RecipeID && receipt.RecipeSHA256 == vllmQwen38RecipeSHA256
}

func qwen38OwnershipReceiptCommon(receipt vllmRuntimeReceipt) bool {
	return qwen38ReceiptBase(receipt) &&
		(receipt.RecipeID == vllmQwen38RecipeID && receipt.RecipeSHA256 == vllmQwen38RecipeSHA256 ||
			receipt.RecipeID == vllmQwen38RecipeV3ID && receipt.RecipeSHA256 == vllmQwen38RecipeV3SHA256 ||
			receipt.RecipeID == vllmQwen38RecipeV2ID && receipt.RecipeSHA256 == vllmQwen38RecipeV2SHA256 ||
			receipt.RecipeID == vllmQwen38RecipeV1ID && receipt.RecipeSHA256 == vllmQwen38RecipeV1SHA256)
}

func qwen38ReceiptBase(receipt vllmRuntimeReceipt) bool {
	return receipt.Version == vllmQwen38Runtime && receipt.Architecture == "arm64" &&
		validSHA256(receipt.PythonSHA256) && validSHA256(receipt.PipReportSHA256) &&
		validSHA256(receipt.RecipeReceiptSHA256) && validSHA256(receipt.BundleReceiptSHA256) &&
		cableIdentifier(receipt.NodeID, 128)
}

func legacyQwen38UVMarkerReceipt(receipt vllmRuntimeReceipt) bool {
	return qwen38OwnershipReceiptCommon(receipt) && receipt.Schema == vllmEnvironmentReceiptSchema &&
		receipt.UVURL == vllmQwen38LegacyMarkerUVURL && receipt.UVSHA256 == vllmQwen38LegacyMarkerUVSHA256 &&
		validSHA256(receipt.CLISHA256) && receipt.WheelURL == "" && receipt.WheelSHA256 == "" && receipt.PythonSource == ""
}
