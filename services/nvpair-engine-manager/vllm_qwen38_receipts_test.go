// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledQwen38ReceiptsRequireExactSealedSidecars(t *testing.T) {
	st := managedVLLMTestState(t)
	dir := filepath.Join(st.installDir, "environments", "v0-qwen")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	recipe := vllmQwen38RecipeReceipt{
		Schema: vllmLegacyReceiptSchema, ArtifactCount: vllmQwen38ArtifactCount,
		RecipeID: vllmQwen38RecipeID, RecipeSHA256: vllmQwen38RecipeSHA256,
		RuntimeVersion: vllmQwen38Runtime, RuntimeCommit: vllmQwen38MinimumCommit,
		WheelSHA256: vllmQwen38WheelSHA256, ArtifactClosureSHA256: vllmQwen38ArtifactClosureSHA256,
		ProviderClosureSHA256: vllmQwen38ProviderClosureSHA256,
		Offline:               true, EmptyCwd: true, TelemetryDisabled: true,
		StageOwner: "stageVLLMEnvironment", ActivateOwner: "activateVLLMEnvironment",
		RollbackOwner: "restoreVLLMRuntime", CleanupOwner: "uninstallVLLMWithIO",
		ActivePointer: vllmRuntimeRecordFile,
	}
	bundle := vllmQwen38BundleReceipt{
		Schema: 1, RecipeID: vllmQwen38RecipeID, RecipeSHA256: vllmQwen38RecipeSHA256,
		ArtifactClosureSHA256: vllmQwen38ArtifactClosureSHA256,
		CleanupOwner:          "stageVLLMEnvironment", ArtifactCount: vllmQwen38ArtifactCount,
		ArtifactBytes: vllmQwen38ArtifactBytes,
		Member: vllmQwen38BundleMember{
			NodeID: "node-a", ProviderClosureSHA256: vllmQwen38ProviderClosureSHA256,
			ProviderReceiptSHA256: strings.Repeat("1", 64), StorageReceiptSHA256: strings.Repeat("2", 64),
			SnapshotReceiptSHA256: strings.Repeat("3", 64), ExpertParallelReceiptSHA256: strings.Repeat("4", 64),
			RDMAReceiptSHA256: strings.Repeat("5", 64), LaunchReceiptSHA256: strings.Repeat("6", 64),
			Layout: "TP2+EP2", Nodes: 2,
		},
	}
	recipePath := filepath.Join(dir, vllmQwen38RecipeReceiptFile)
	bundlePath := filepath.Join(dir, vllmQwen38BundleReceiptFile)
	if err := writeVLLMJSON(st.installDir, recipePath, recipe); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMJSON(st.installDir, bundlePath, bundle); err != nil {
		t.Fatal(err)
	}
	recipeHash, _ := managedVLLMFileHash(st.installDir, recipePath)
	bundleHash, _ := managedVLLMFileHash(st.installDir, bundlePath)
	receipt := vllmRuntimeReceipt{
		Schema: vllmLegacyReceiptSchema, Version: vllmQwen38Runtime, Architecture: "arm64",
		WheelURL: vllmQwen38WheelURL, WheelSHA256: vllmQwen38WheelSHA256,
		PythonSHA256: strings.Repeat("7", 64), PipReportSHA256: strings.Repeat("8", 64),
		RecipeID: vllmQwen38RecipeID, RecipeSHA256: vllmQwen38RecipeSHA256,
		RecipeReceiptSHA256: recipeHash, BundleReceiptSHA256: bundleHash, NodeID: "node-a",
	}
	if _, err := installedQwen38BundleReceipt(st, dir, receipt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundlePath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installedQwen38BundleReceipt(st, dir, receipt); err == nil {
		t.Fatal("changed installed bundle sidecar retained Qwen authority")
	}
}
