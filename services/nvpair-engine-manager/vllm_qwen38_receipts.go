// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"path/filepath"
	"time"
)

const (
	vllmQwen38BundleReceiptFile = "pair-qwen38-bundle.json"
	vllmQwen38RecipeReceiptFile = "pair-qwen38-recipe.json"
)

type vllmQwen38BundleMember struct {
	NodeID                                                                string `json:"nodeId"`
	ProviderClosureSHA256, ProviderReceiptSHA256, StorageReceiptSHA256    string
	SnapshotReceiptSHA256, ExpertParallelReceiptSHA256, EPLBReceiptSHA256 string
	RDMAReceiptSHA256, LaunchReceiptSHA256, Layout                        string
	Nodes                                                                 int
}

type vllmQwen38BundleReceipt struct {
	Schema                int
	Phase                 string
	RecipeID              string
	RecipeSHA256          string
	ArtifactClosureSHA256 string
	CleanupOwner          string
	ArtifactCount         int
	ArtifactBytes         int64
	Member                vllmQwen38BundleMember
}

func validateInstalledQwen38Receipts(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt) error {
	_, err := installedQwen38BundleReceipt(st, dir, runtimeReceipt)
	return err
}

func validatePreparedQwen38Receipts(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt) error {
	return validatePreparedQwen38ReceiptsMode(st, dir, runtimeReceipt, false)
}

func validatePreparedQwen38ReceiptsForOwnership(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt) error {
	return validatePreparedQwen38ReceiptsMode(st, dir, runtimeReceipt, true)
}

func validatePreparedQwen38ReceiptsMode(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt, ownershipOnly bool) error {
	current := admittedRetainedQwen38Receipt(runtimeReceipt)
	owned := ownedRetainedQwen38Receipt(runtimeReceipt) || legacyQwen38UVMarkerReceipt(runtimeReceipt)
	v1 := runtimeReceipt.RecipeID == vllmQwen38RecipeV1ID
	if runtimeReceipt.Schema != vllmEnvironmentReceiptSchema || !current && (!ownershipOnly || !owned) {
		return errors.New("prepared Qwen3.8 runtime receipt lacks exact schema-2 provenance")
	}
	recipeID, recipeSHA256 := vllmQwen38RecipeID, vllmQwen38RecipeSHA256
	artifactClosure, artifactBytes, ncclVersion := vllmQwen38ArtifactClosureSHA256, vllmQwen38ArtifactBytes, "2.30.7"
	switch {
	case v1:
		recipeID, recipeSHA256 = vllmQwen38RecipeV1ID, vllmQwen38RecipeV1SHA256
		artifactClosure, artifactBytes, ncclVersion = vllmQwen38ArtifactV1ClosureSHA256, vllmQwen38ArtifactV1Bytes, ""
	case runtimeReceipt.RecipeID == vllmQwen38RecipeV2ID:
		recipeID, recipeSHA256 = vllmQwen38RecipeV2ID, vllmQwen38RecipeV2SHA256
	case runtimeReceipt.RecipeID == vllmQwen38RecipeV3ID:
		recipeID, recipeSHA256 = vllmQwen38RecipeV3ID, vllmQwen38RecipeV3SHA256
	}
	recipePath := filepath.Join(dir, vllmQwen38RecipeReceiptFile)
	bundlePath := filepath.Join(dir, vllmQwen38BundleReceiptFile)
	for path, want := range map[string]string{recipePath: runtimeReceipt.RecipeReceiptSHA256, bundlePath: runtimeReceipt.BundleReceiptSHA256} {
		digest, err := managedVLLMFileHash(st.installDir, path)
		if err != nil || digest != want {
			return errors.New("prepared Qwen3.8 recipe or bundle receipt changed")
		}
	}
	var recipeReceipt vllmQwen38RecipeReceipt
	if err := readVLLMJSON(st.installDir, recipePath, 1<<20, &recipeReceipt); err != nil {
		return err
	}
	if recipeReceipt.Schema != 2 || recipeReceipt.LifecycleSchema != vllmRuntimeRecordSchema || recipeReceipt.ArtifactCount != vllmQwen38ArtifactCount ||
		recipeReceipt.RecipeID != recipeID || recipeReceipt.RecipeSHA256 != recipeSHA256 ||
		recipeReceipt.RuntimeVersion != vllmQwen38Runtime || recipeReceipt.RuntimeCommit != vllmQwen38MinimumCommit ||
		recipeReceipt.WheelSHA256 != vllmQwen38WheelSHA256 || recipeReceipt.ArtifactClosureSHA256 != artifactClosure ||
		!knownQwen38ProviderClosure(recipeReceipt.ProviderClosureSHA256) || recipeReceipt.ReferenceProviderClosureSHA256 != vllmQwen38ProviderClosureSHA256 ||
		recipeReceipt.ArtifactBytes != artifactBytes || !recipeReceipt.Offline || !recipeReceipt.EmptyCwd || !recipeReceipt.TelemetryDisabled ||
		recipeReceipt.StageOwner != "stageQwen38ManagedVLLM" || recipeReceipt.ActivateOwner != "activateManagedVLLM" ||
		recipeReceipt.RollbackOwner != "restoreManagedVLLM" || recipeReceipt.CleanupOwner != "uninstallManagedVLLMWithIO" ||
		recipeReceipt.ActivePointer != vllmRuntimeRecordFile || recipeReceipt.TimeoutSeconds != 1800 || recipeReceipt.MemoryMaxBytes != 24<<30 ||
		recipeReceipt.StageMaxBytes != 24<<30 || recipeReceipt.FreeReserveBytes != 40<<30 || recipeReceipt.TasksMax != 512 || !recipeReceipt.WholeCgroupCleanup {
		return errors.New("prepared Qwen3.8 recipe receipt changed")
	}
	var bundle vllmQwen38PreparedBundleReceipt
	if err := readVLLMJSON(st.installDir, bundlePath, 1<<20, &bundle); err != nil {
		return err
	}
	if bundle.Schema != 1 || bundle.Phase != vllmQwen38PreparedPhase || bundle.RecipeID != recipeID ||
		bundle.RecipeSHA256 != recipeSHA256 || bundle.ArtifactClosureSHA256 != artifactClosure ||
		bundle.ArtifactCount != vllmQwen38ArtifactCount || bundle.ArtifactBytes != artifactBytes || len(bundle.Artifacts) != vllmQwen38ArtifactCount || !validSHA256(bundle.LaunchReceiptSHA256) ||
		!bundle.Provider.Qualified || bundle.Provider.ProfileID == "" || !knownQwen38ProviderClosure(bundle.Provider.ObservedClosureSHA256) || bundle.Provider.ExpectedClosureSHA256 != bundle.Provider.ObservedClosureSHA256 || len(bundle.Provider.Mismatches) != 0 {
		return errors.New("prepared Qwen3.8 bundle receipt changed")
	}
	if _, err := time.Parse(time.RFC3339Nano, bundle.PreparedAt); err != nil {
		return errors.New("prepared Qwen3.8 bundle receipt has an invalid time")
	}
	launchPath := filepath.Join(dir, vllmQwen38LaunchReceiptFile)
	launchHash, err := managedVLLMFileHash(st.installDir, launchPath)
	if err != nil || launchHash != bundle.LaunchReceiptSHA256 {
		return errors.New("prepared Qwen3.8 launch receipt changed")
	}
	var launch vllmQwen38LaunchReceipt
	if err = readVLLMJSON(st.installDir, launchPath, 32<<10, &launch); err != nil {
		return err
	}
	if launch.Schema != 1 || launch.RecipeID != recipeID || launch.RecipeSHA256 != recipeSHA256 || launch.RuntimeVersion != vllmQwen38Runtime || !validSHA256(launch.RuntimeCompatibilitySHA256) || !knownQwen38ProviderClosure(launch.ProviderClosureSHA256) || launch.ProviderClosureSHA256 != bundle.Provider.ObservedClosureSHA256 || launch.Python != "3.12.3" || launch.SOABI != "cpython-312-aarch64-linux-gnu" || launch.Glibc != "2.39" || launch.Torch != "2.13.0+cu130" || launch.CUDA != "13.0" || launch.NCCL != ncclVersion || launch.Transformers != "5.17.0" || launch.CompressedTensors != "0.17.0" || launch.ModelOptClass != "ModelOptNvFp4Config" || launch.QwenClass != "Qwen4ExpForCausalLM" {
		return errors.New("prepared Qwen3.8 launch capability receipt changed")
	}
	if _, err = time.Parse(time.RFC3339Nano, launch.VerifiedAt); err != nil {
		return errors.New("prepared Qwen3.8 launch receipt has an invalid time")
	}
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		return err
	}
	for index, artifact := range recipe.Runtime.Artifacts {
		got := bundle.Artifacts[index]
		filename, sha256, bytes := artifact.Filename, artifact.SHA256, artifact.Bytes
		if v1 && artifact.Name == "nvidia-nccl-cu13" {
			filename, sha256, bytes = "nvidia_nccl_cu13-2.29.7-py3-none-manylinux_2_18_aarch64.whl", "674a12383e3c38a1bcccae7d4f3633b37852230b6047883cb2f4c2d1b36d9bf5", int64(206014712)
		}
		if got.Filename != filename || got.SHA256 != sha256 || got.Bytes != bytes {
			return errors.New("prepared Qwen3.8 artifact receipt changed")
		}
	}
	return nil
}

func preparedQwen38Receipts(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt) (vllmQwen38PreparedBundleReceipt, vllmQwen38LaunchReceipt, error) {
	return preparedQwen38ReceiptsMode(st, dir, runtimeReceipt, false)
}

func preparedQwen38ReceiptsForOwnership(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt) (vllmQwen38PreparedBundleReceipt, vllmQwen38LaunchReceipt, error) {
	return preparedQwen38ReceiptsMode(st, dir, runtimeReceipt, true)
}

func preparedQwen38ReceiptsMode(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt, allowLegacyMarker bool) (vllmQwen38PreparedBundleReceipt, vllmQwen38LaunchReceipt, error) {
	var bundle vllmQwen38PreparedBundleReceipt
	var launch vllmQwen38LaunchReceipt
	if err := validatePreparedQwen38ReceiptsMode(st, dir, runtimeReceipt, allowLegacyMarker); err != nil {
		return bundle, launch, err
	}
	if err := readVLLMJSON(st.installDir, filepath.Join(dir, vllmQwen38BundleReceiptFile), 1<<20, &bundle); err != nil {
		return bundle, launch, err
	}
	if err := readVLLMJSON(st.installDir, filepath.Join(dir, vllmQwen38LaunchReceiptFile), 32<<10, &launch); err != nil {
		return bundle, launch, err
	}
	return bundle, launch, nil
}

func installedQwen38BundleReceipt(st *engineState, dir string, runtimeReceipt vllmRuntimeReceipt) (vllmQwen38BundleReceipt, error) {
	var bundle vllmQwen38BundleReceipt
	if !ownedRetainedQwen38Receipt(runtimeReceipt) {
		return bundle, errors.New("Qwen3.8 runtime receipt lacks exact recipe and bundle provenance")
	}
	recipeID, recipeSHA256 := vllmQwen38RecipeID, vllmQwen38RecipeSHA256
	artifactClosure, artifactBytes := vllmQwen38ArtifactClosureSHA256, vllmQwen38ArtifactBytes
	switch runtimeReceipt.RecipeID {
	case vllmQwen38RecipeV1ID:
		recipeID, recipeSHA256 = vllmQwen38RecipeV1ID, vllmQwen38RecipeV1SHA256
		artifactClosure, artifactBytes = vllmQwen38ArtifactV1ClosureSHA256, vllmQwen38ArtifactV1Bytes
	case vllmQwen38RecipeV2ID:
		recipeID, recipeSHA256 = vllmQwen38RecipeV2ID, vllmQwen38RecipeV2SHA256
	}
	recipePath := filepath.Join(dir, vllmQwen38RecipeReceiptFile)
	bundlePath := filepath.Join(dir, vllmQwen38BundleReceiptFile)
	for path, want := range map[string]string{
		recipePath: runtimeReceipt.RecipeReceiptSHA256,
		bundlePath: runtimeReceipt.BundleReceiptSHA256,
	} {
		digest, err := managedVLLMFileHash(st.installDir, path)
		if err != nil || digest != want {
			return bundle, errors.New("Qwen3.8 installed recipe or bundle receipt changed")
		}
	}
	var recipe vllmQwen38RecipeReceipt
	if err := readVLLMJSON(st.installDir, recipePath, 1<<20, &recipe); err != nil {
		return bundle, err
	}
	if recipe.Schema != 1 || recipe.ArtifactCount != vllmQwen38ArtifactCount ||
		recipe.RecipeID != recipeID || recipe.RecipeSHA256 != recipeSHA256 ||
		recipe.RuntimeVersion != vllmQwen38Runtime || recipe.RuntimeCommit != vllmQwen38MinimumCommit ||
		recipe.WheelSHA256 != vllmQwen38WheelSHA256 || recipe.ArtifactClosureSHA256 != artifactClosure ||
		recipe.ProviderClosureSHA256 != vllmQwen38ProviderClosureSHA256 ||
		!recipe.Offline || !recipe.EmptyCwd || !recipe.TelemetryDisabled ||
		recipe.StageOwner != "stageVLLMEnvironment" || recipe.ActivateOwner != "activateVLLMEnvironment" ||
		recipe.RollbackOwner != "restoreVLLMRuntime" || recipe.CleanupOwner != "uninstallVLLMWithIO" ||
		recipe.ActivePointer != vllmRuntimeRecordFile {
		return bundle, errors.New("Qwen3.8 installed recipe receipt changed")
	}
	if err := readVLLMJSON(st.installDir, bundlePath, 1<<20, &bundle); err != nil {
		return bundle, err
	}
	layout, layoutErr := sealedQwen38Layout(recipeID, bundle.Member.Nodes)
	if layoutErr != nil || bundle.Schema != 1 || bundle.Phase != "" ||
		bundle.RecipeID != recipeID || bundle.RecipeSHA256 != recipeSHA256 ||
		bundle.ArtifactClosureSHA256 != artifactClosure ||
		bundle.CleanupOwner != "stageVLLMEnvironment" || bundle.ArtifactCount != vllmQwen38ArtifactCount ||
		bundle.ArtifactBytes != artifactBytes || bundle.Member.NodeID != runtimeReceipt.NodeID ||
		bundle.Member.ProviderClosureSHA256 != vllmQwen38ProviderClosureSHA256 ||
		bundle.Member.Layout != layout.Label {
		return vllmQwen38BundleReceipt{}, errors.New("Qwen3.8 installed bundle receipt changed")
	}
	for _, digest := range []string{
		bundle.Member.ProviderReceiptSHA256, bundle.Member.StorageReceiptSHA256,
		bundle.Member.SnapshotReceiptSHA256, bundle.Member.ExpertParallelReceiptSHA256,
		bundle.Member.RDMAReceiptSHA256, bundle.Member.LaunchReceiptSHA256,
	} {
		if !validSHA256(digest) {
			return vllmQwen38BundleReceipt{}, errors.New("Qwen3.8 installed bundle admission is incomplete")
		}
	}
	if layout.EPLB {
		if !validSHA256(bundle.Member.EPLBReceiptSHA256) {
			return vllmQwen38BundleReceipt{}, errors.New("Qwen3.8 installed EPLB bundle lacks EPLB admission")
		}
	} else if bundle.Member.EPLBReceiptSHA256 != "" {
		return vllmQwen38BundleReceipt{}, errors.New("Qwen3.8 installed bundle claimed EPLB admission its layout does not use")
	}
	return bundle, nil
}

// sealedQwen38Layout is the layout the given recipe sealed for this member
// count: the current profile, or the retired layout earlier recipes admitted.
func sealedQwen38Layout(recipeID string, nodes int) (vllmQwen38Layout, error) {
	if recipeID != vllmQwen38RecipeID {
		if retired, ok := qwen38RetiredLayout(nodes); ok {
			return retired, nil
		}
	}
	return qwen38Layout(nodes)
}
