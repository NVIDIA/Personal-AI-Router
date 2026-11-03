// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVLLMCatalog_ExcludesKnownIncompatibleModelOnlyForMacTargets(t *testing.T) {
	rows, err := json.Marshal([]any{
		vllmRow("nvidia/Llama-3.1-8B-Instruct-FP8", nil),
		vllmRow("Qwen/Qwen3.5-4B", nil),
		vllmRow("someone/Other-FP8", nil),
		vllmRow("nvidia/Llama-3.1-8B-Instruct-FP8-copy", nil),
	})
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(rows)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	cases := []struct {
		name, platform, arch, query string
		excluded                    bool
	}{
		{"Apple Silicon browse", "darwin", "arm64", "", true},
		{"Apple Silicon search", "darwin", "arm64", "Llama-3.1-8B-Instruct-FP8", true},
		{"Intel Mac browse", "darwin", "amd64", "", true},
		{"Intel Mac search", "darwin", "amd64", "Llama-3.1-8B-Instruct-FP8", true},
		{"Mac without architecture", "darwin", "", "", true},
		{"Spark browse", "linux", "arm64", "", false},
		{"Spark search", "linux", "arm64", "Llama-3.1-8B-Instruct-FP8", false},
		{"Linux x64 browse", "linux", "amd64", "", false},
		{"Linux x64 search", "linux", "amd64", "Llama-3.1-8B-Instruct-FP8", false},
		{"Windows browse", "windows", "amd64", "", false},
		{"Windows search", "windows", "amd64", "Llama-3.1-8B-Instruct-FP8", false},
		{"unknown OS", "", "arm64", "", false},
		{"local host browse", "", "", "", runtime.GOOS == "darwin"},
		{"local host search", "", "", "Llama-3.1-8B-Instruct-FP8", runtime.GOOS == "darwin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCatalogService()
			c.baseURL = srv.URL

			result, err := c.Catalog(context.Background(), "vllm", tc.platform, tc.arch, tc.query)
			require.NoError(t, err)
			ids := make([]string, 0, len(result.Models))
			for _, model := range result.Models {
				ids = append(ids, model.ID)
			}
			if tc.excluded {
				assert.Equal(t, []string{"Qwen/Qwen3.5-4B", "someone/Other-FP8", "nvidia/Llama-3.1-8B-Instruct-FP8-copy"}, ids)
			} else {
				assert.Equal(t, []string{"nvidia/Llama-3.1-8B-Instruct-FP8", "Qwen/Qwen3.5-4B", "someone/Other-FP8", "nvidia/Llama-3.1-8B-Instruct-FP8-copy"}, ids)
			}
			assert.True(t, result.Searchable)
			assert.Equal(t, tc.query, result.Query)
			if tc.platform != "" || tc.arch != "" {
				assert.Equal(t, tc.platform, result.Platform)
				assert.Equal(t, tc.arch, result.Arch)
			} else {
				assert.Equal(t, runtime.GOOS, result.Platform)
				assert.Equal(t, runtime.GOARCH, result.Arch)
			}
		})
	}
}

func TestVLLMCatalog_MacFilteringPreservesCachedModelsForOtherTargets(t *testing.T) {
	rows, err := json.Marshal([]any{vllmRow("nvidia/Llama-3.1-8B-Instruct-FP8", nil)})
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(rows)
		assert.NoError(t, err)
	}))
	defer srv.Close()
	for _, query := range []string{"", "Llama-3.1-8B-Instruct-FP8"} {
		name := "browse cache"
		if query != "" {
			name = "search cache"
		}
		t.Run(name, func(t *testing.T) {
			c := newCatalogService()
			c.baseURL = srv.URL
			mac, err := c.Catalog(context.Background(), "vllm", "darwin", "arm64", query)
			require.NoError(t, err)
			assert.NotNil(t, mac.Models, "an empty catalog must serialize as an array")
			assert.Empty(t, mac.Models)

			spark, err := c.Catalog(context.Background(), "vllm", "linux", "arm64", query)
			require.NoError(t, err)
			require.Len(t, spark.Models, 1)
			assert.Equal(t, "nvidia/Llama-3.1-8B-Instruct-FP8", spark.Models[0].ID)

			macAgain, err := c.Catalog(context.Background(), "vllm", "darwin", "arm64", query)
			require.NoError(t, err)
			assert.Empty(t, macAgain.Models)
		})
	}
}
