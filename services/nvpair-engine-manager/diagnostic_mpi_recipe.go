// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "errors"

const diagnosticMPILegacyRecipe = "pair-two-spark-nccl-socket-smoke-v1"
const diagnosticMPIQuickRecipe = "pair-two-spark-nccl-socket-correctness-v2"
const diagnosticMPITripleRecipe = "pair-three-spark-nccl-socket-correctness-v3"

func diagnosticMPIRecipeRanks(recipe string) int {
	switch recipe {
	case diagnosticMPILegacyRecipe, diagnosticMPIQuickRecipe:
		return 2
	case diagnosticMPITripleRecipe:
		return 3
	default:
		return 0
	}
}

// Only optional historical metadata can omit the recipe. A plan must name it.
func diagnosticMPIStoredRecipe(recipe string) string {
	if recipe == "" {
		return diagnosticMPILegacyRecipe
	}
	return recipe
}

func diagnosticMPIRecipeArgs(recipe string) ([]string, uint64, error) {
	switch recipe {
	case diagnosticMPILegacyRecipe:
		return append([]string(nil), diagnosticNCCLArgs...), 8388608, nil
	case diagnosticMPIQuickRecipe, diagnosticMPITripleRecipe:
		return []string{"-b", "8", "-e", "65536", "-f", "2", "-g", "1", "-t", "1", "-n", "3", "-w", "1", "-c", "1", "-N", "1", "-T", "10", "-d", "float", "-o", "sum"}, 65536, nil
	default:
		return nil, 0, errors.New("unknown fixed managed MPI recipe")
	}
}
