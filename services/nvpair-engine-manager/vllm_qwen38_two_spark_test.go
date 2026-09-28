// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestQwen38ServesOnlyOnTwoSparks(t *testing.T) {
	two, err := qwen38Layout(2)
	if err != nil || two != (vllmQwen38Layout{Label: "TP2+EP2", Nodes: 2, TensorParallel: 2, PipelineParallel: 1, DataParallel: 1, ExpertParallel: 2}) {
		t.Fatalf("two-node layout = %+v, err=%v", two, err)
	}
	if _, err := qwen38Layout(3); !errors.Is(err, errQwen38TwoSparks) {
		t.Fatalf("three-node layout err = %v, want the two-Spark reason", err)
	}
	if _, err := qwen38GroupTopology(3, strings.Repeat("c", 64)); !errors.Is(err, errQwen38TwoSparks) {
		t.Fatalf("three-node topology err = %v, want the two-Spark reason", err)
	}
	topology, err := qwen38GroupTopology(2, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	pipeline := topology
	pipeline.TensorParallel, pipeline.PipelineParallel, pipeline.ExpertParallel = 1, 3, 1
	dataParallel := topology
	dataParallel.TensorParallel, dataParallel.DataParallel, dataParallel.ExpertParallel, dataParallel.EPLB, dataParallel.RedundantExperts = 1, 3, 3, true, 1
	for name, value := range map[string]vllmGroupTopology{"pipeline": pipeline, "data parallel": dataParallel} {
		if err := validateQwen38GroupTopology(value, 3); !errors.Is(err, errQwen38TwoSparks) {
			t.Fatalf("three-node %s topology err = %v, want the two-Spark reason", name, err)
		}
	}
}

// A three-Spark request is refused with the reason before any member is
// inspected, so the refusal never surfaces as an unrelated fabric failure.
func TestQwen38ThreeSparkSelectionIsRefusedBeforeReview(t *testing.T) {
	qwen := vllmGroupSelection{NodeIDs: []string{"node-a", "node-b", "node-c"}, Model: vllmQwen38ModelID}
	if err := validateVLLMGroupSelection(qwen); !errors.Is(err, errQwen38TwoSparks) {
		t.Fatalf("three-Spark Qwen3.8 selection err = %v, want the two-Spark reason", err)
	}
	qwen.NodeIDs = qwen.NodeIDs[:2]
	if err := validateVLLMGroupSelection(qwen); err != nil {
		t.Fatalf("two-Spark Qwen3.8 selection was refused: %v", err)
	}
	ordinary := vllmGroupSelection{NodeIDs: []string{"node-a", "node-b", "node-c"}, Model: "local:" + strings.Repeat("b", 64)}
	if err := validateVLLMGroupSelection(ordinary); err != nil {
		t.Fatalf("an ordinary three-node selection was refused: %v", err)
	}
}

func TestQwen38TwoSparkLayoutIsOnePipelineStage(t *testing.T) {
	_, config, _, _ := qwen38MetadataFixture(t)
	sum := sha256.Sum256(config)
	topology, err := qwen38GroupTopology(2, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if partition, err := vllmGroupLayerPartition(topology, 2, config); err != nil || partition != "48" {
		t.Fatalf("two-Spark partition = %q, err=%v", partition, err)
	}
}

func TestQwen38AdmissionNeedsNoEPLBOwnerAndRefusesThreeSparks(t *testing.T) {
	layout, _ := qwen38Layout(2)
	admission := vllmQwen38Admission{RecipeSHA256: vllmQwen38RecipeSHA256, Nodes: 2, Layout: layout, ArtifactBundleReady: true, SnapshotReady: true, StorageReady: true, ProviderLoaderReady: true, ExpertOwnerReady: true, RDMAReady: true, LaunchReady: true}
	if got, err := admitQwen38Runtime(admission); err != nil || got != layout {
		t.Fatalf("two-Spark admission = %+v, err=%v", got, err)
	}
	retired, ok := qwen38RetiredLayout(3)
	if !ok || !retired.EPLB {
		t.Fatalf("retired three-node layout = %+v ok=%v", retired, ok)
	}
	admission.Nodes, admission.Layout, admission.EPLBOwnerReady = 3, retired, true
	if _, err := admitQwen38Runtime(admission); !errors.Is(err, errQwen38TwoSparks) {
		t.Fatalf("three-Spark admission err = %v, want the two-Spark reason", err)
	}
}

func TestQwen38RecipeV4SealsOnlyTheTwoSparkLayout(t *testing.T) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil || recipe.ID != "qwen38-flash-next-nvfp4-d4d703c-spark-a-v4" || recipe.PreferredNodes != 2 || !slices.Equal(recipe.Layouts, qwen38Profile().Layouts) || len(recipe.Layouts) != 1 {
		t.Fatalf("recipe = %q preferred=%d layouts=%+v err=%v", recipe.ID, recipe.PreferredNodes, recipe.Layouts, err)
	}
	for _, gate := range []string{"eplb-owner-for-three-nodes", "nccl-peer-subnet-routing"} {
		if slices.Contains(recipe.RequiredGates, gate) {
			t.Fatalf("two-Spark recipe still requires the three-node gate %q", gate)
		}
	}
}

// Rewrites a prepared runtime's receipts as an earlier recipe prepared the
// same runtime bytes.
func relabelPreparedQwen38(t *testing.T, st *engineState, dir string, receipt vllmRuntimeReceipt, recipeID, recipeSHA256 string) vllmRuntimeReceipt {
	t.Helper()
	var recipeReceipt vllmQwen38RecipeReceipt
	recipePath := filepath.Join(dir, vllmQwen38RecipeReceiptFile)
	if err := readVLLMJSON(st.installDir, recipePath, 1<<20, &recipeReceipt); err != nil {
		t.Fatal(err)
	}
	recipeReceipt.RecipeID, recipeReceipt.RecipeSHA256 = recipeID, recipeSHA256
	if err := writeManagedVLLMJSON(st.installDir, recipePath, recipeReceipt); err != nil {
		t.Fatal(err)
	}
	var launch vllmQwen38LaunchReceipt
	launchPath := filepath.Join(dir, vllmQwen38LaunchReceiptFile)
	if err := readVLLMJSON(st.installDir, launchPath, 32<<10, &launch); err != nil {
		t.Fatal(err)
	}
	launch.RecipeID, launch.RecipeSHA256 = recipeID, recipeSHA256
	if err := writeManagedVLLMJSON(st.installDir, launchPath, launch); err != nil {
		t.Fatal(err)
	}
	launchHash, _ := managedVLLMFileHash(st.installDir, launchPath)
	var bundle vllmQwen38PreparedBundleReceipt
	bundlePath := filepath.Join(dir, vllmQwen38BundleReceiptFile)
	if err := readVLLMJSON(st.installDir, bundlePath, 1<<20, &bundle); err != nil {
		t.Fatal(err)
	}
	bundle.RecipeID, bundle.RecipeSHA256, bundle.LaunchReceiptSHA256 = recipeID, recipeSHA256, launchHash
	if err := writeManagedVLLMJSON(st.installDir, bundlePath, bundle); err != nil {
		t.Fatal(err)
	}
	receipt.RecipeID, receipt.RecipeSHA256 = recipeID, recipeSHA256
	receipt.RecipeReceiptSHA256, _ = managedVLLMFileHash(st.installDir, recipePath)
	receipt.BundleReceiptSHA256, _ = managedVLLMFileHash(st.installDir, bundlePath)
	return receipt
}

func TestQwen38EarlierPreparedRuntimesAreOwnershipOnly(t *testing.T) {
	for _, earlier := range []struct{ id, sha256 string }{
		{vllmQwen38RecipeV2ID, vllmQwen38RecipeV2SHA256},
		{vllmQwen38RecipeV3ID, vllmQwen38RecipeV3SHA256},
	} {
		t.Run(earlier.id, func(t *testing.T) {
			st, dir, receipt, _ := preparedQwen38FactsFixture(t)
			relabeled := relabelPreparedQwen38(t, st, dir, receipt, earlier.id, earlier.sha256)
			if !ownedRetainedQwen38Receipt(relabeled) || admittedRetainedQwen38Receipt(relabeled) {
				t.Fatal("runtime receipt was not ownership-only")
			}
			if err := validatePreparedQwen38ReceiptsForOwnership(st, dir, relabeled); err != nil {
				t.Fatalf("prepared receipts lost ownership: %v", err)
			}
			if err := validatePreparedQwen38Receipts(st, dir, relabeled); err == nil {
				t.Fatal("prepared receipts were admitted")
			}
			if _, err := qwen38GroupFacts(st, dir, relabeled, strings.Repeat("b", 64), strings.Repeat("e", 64), 2); err == nil {
				t.Fatal("an earlier-recipe runtime produced group facts")
			}
		})
	}
}
