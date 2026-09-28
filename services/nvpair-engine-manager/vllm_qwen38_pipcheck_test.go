// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestQwen38ConsistencyAcceptsOnlyTheDeclaredNCCLOverride(t *testing.T) {
	recipe, err := qwen38RuntimeRecipe()
	if err != nil {
		t.Fatal(err)
	}
	const override = `torch 2.13.0 has requirement nvidia-nccl-cu13==2.29.7; platform_system == "Linux", but you have nvidia-nccl-cu13 2.30.7.`
	for _, test := range []struct {
		name   string
		output string
		want   bool
	}{
		{"declared override", override + "\n", true},
		{"declared override without marker", "torch 2.13.0 has requirement nvidia-nccl-cu13==2.29.7, but you have nvidia-nccl-cu13 2.30.7.", true},
		{"no output", "", false},
		{"missing dependency beside the override", override + "\nvllm 0.28.1 requires xgrammar, which is not installed.", false},
		{"another pin", "vllm 0.28.1 has requirement torch==2.12.0, but you have torch 2.13.0.", false},
		{"different installed release", `torch 2.13.0 has requirement nvidia-nccl-cu13==2.29.7; platform_system == "Linux", but you have nvidia-nccl-cu13 2.30.8.`, false},
		{"different pinned release", "torch 2.13.0 has requirement nvidia-nccl-cu13==2.29.6, but you have nvidia-nccl-cu13 2.30.7.", false},
		{"unparsed output", "ERROR: unexpected failure", false},
	} {
		if got := qwen38ConflictsAreDeclaredOverrides([]byte(test.output), recipe); got != test.want {
			t.Errorf("%s: got %v, want %v", test.name, got, test.want)
		}
	}
}
