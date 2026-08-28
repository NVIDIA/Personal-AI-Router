// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"
)

func TestLMSEntryMatchesModel(t *testing.T) {
	entry := lmsListEntry{
		ModelKey:               "text-embedding-nomic-embed-text-v1.5",
		Path:                   "nomic-ai/nomic-embed-text-v1.5-GGUF/nomic-embed-text-v1.5.Q4_K_M.gguf",
		IndexedModelIdentifier: "nomic-ai/nomic-embed-text-v1.5-GGUF/nomic-embed-text-v1.5.Q4_K_M.gguf",
	}
	cases := []struct {
		model string
		want  bool
	}{
		{"text-embedding-nomic-embed-text-v1.5", true},
		{"nomic-ai/nomic-embed-text-v1.5-GGUF/nomic-embed-text-v1.5.Q4_K_M.gguf", true},
		{"nomic-ai/nomic-embed-text-v1.5-GGUF", true},
		{"publisher/demo-model", false},
		{"", false},
	}
	for _, c := range cases {
		if got := lmsEntryMatchesModel(entry, c.model); got != c.want {
			t.Errorf("lmsEntryMatchesModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestSafeRemoveUnderRootRejectsMissingPath(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing-model")
	if err := safeRemoveUnderRoot(root, missing); err == nil {
		t.Fatal("expected missing path to fail")
	}
}
