// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLMSEntryMatchesModel(t *testing.T) {
	entry := lmsListEntry{
		ModelKey:               "text-embedding-nomic-embed-text-v1.5",
		Path:                   "nomic-ai/nomic-embed-text-v1.5-GGUF/nomic-embed-text-v1.5.Q4_K_M.gguf",
		IndexedModelIdentifier: "nomic-ai/nomic-embed-text-v1.5-GGUF/nomic-embed-text-v1.5.Q4_K_M.gguf",
	}
	test := func(name, model string, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, lmsEntryMatchesModel(entry, model))
		})
	}
	test("model key", "text-embedding-nomic-embed-text-v1.5", true)
	test("full path", "nomic-ai/nomic-embed-text-v1.5-GGUF/nomic-embed-text-v1.5.Q4_K_M.gguf", true)
	test("repository", "nomic-ai/nomic-embed-text-v1.5-GGUF", true)
	test("other model", "publisher/demo-model", false)
	test("empty model", "", false)
}

func TestSafeRemoveUnderRootRejectsMissingPath(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing-model")
	require.Error(t, safeRemoveUnderRoot(root, missing), "expected missing path to fail")
}
