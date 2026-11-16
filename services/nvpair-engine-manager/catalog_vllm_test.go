// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func vllmRow(id string, mutate func(*map[string]any)) map[string]any {
	row := map[string]any{
		"id": id, "gated": false, "private": false, "downloads": 100, "likes": 5,
		"lastModified": "2026-01-02T03:04:05.000Z", "pipeline_tag": "text-generation",
		"tags":        []string{"transformers", "safetensors", "text-generation"},
		"safetensors": map[string]any{"parameters": map[string]int64{"BF16": 8_000_000_000}, "total": 8_000_000_000},
	}
	if mutate != nil {
		mutate(&row)
	}
	return row
}

func TestVLLMCatalog_KeepsOpenSafetensorsChatModels(t *testing.T) {
	var mu sync.Mutex
	var queries []map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		if r.URL.Query().Get("pipeline_tag") == "image-text-to-text" {
			assert.NoError(t, json.NewEncoder(w).Encode([]any{vllmRow("Qwen/Vision-8B", func(m *map[string]any) { (*m)["pipeline_tag"] = "image-text-to-text"; (*m)["downloads"] = 900 })}))
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode([]any{
			vllmRow("Qwen/Chat-8B", nil),
			vllmRow("Qwen/Gated", func(m *map[string]any) { (*m)["gated"] = "auto" }),
			vllmRow("Qwen/Private", func(m *map[string]any) { (*m)["private"] = true }),
			vllmRow("Qwen/Embed", func(m *map[string]any) { (*m)["pipeline_tag"] = "feature-extraction" }),
			vllmRow("Qwen/NoSafetensors", func(m *map[string]any) { (*m)["tags"] = []string{"gguf", "text-generation"} }),
			vllmRow("mlx-community/Chat-4bit", func(m *map[string]any) { (*m)["tags"] = []string{"safetensors", "mlx", "text-generation"} }),
			vllmRow("../bad", nil),
		}))
	}))
	defer srv.Close()
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(t.Context(), "vllm", "", "", "", "")

	require.NoError(t, err)
	var ids []string
	for _, m := range res.Models {
		ids = append(ids, m.ID)
	}
	require.Equal(t, []string{"Qwen/Vision-8B", "Qwen/Chat-8B"}, ids, "want the open chat models, most downloaded first")
	assert.True(t, res.Searchable, "the vLLM catalog can be searched")
	m := res.Models[1]
	// A pull-ready name, 8B parameters and 16 GB of BF16 weights.
	assert.Equal(t, "Qwen/Chat-8B", m.Name)
	assert.Equal(t, "8B", m.ParameterSize)
	assert.Equal(t, uint64(16_000_000_000), m.Size)
	assert.Equal(t, "Qwen", m.Author)
	pipelines := map[string]bool{}
	for _, q := range queries {
		require.NotEmpty(t, q["pipeline_tag"])
		require.NotEmpty(t, q["filter"])
		pipelines[q["pipeline_tag"][0]] = true
		assert.Equal(t, "safetensors", q["filter"][0], "a listing must filter on safetensors")
		assert.Contains(t, q["expand[]"], "safetensors", "a listing must expand safetensors")
	}
	assert.True(t, pipelines["text-generation"], "both chat pipelines must be listed")
	assert.True(t, pipelines["image-text-to-text"], "both chat pipelines must be listed")
}

func TestVLLMCatalog_SearchesWithTheTrimmedQuery(t *testing.T) {
	var mu sync.Mutex
	var searches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		searches = append(searches, r.URL.Query().Get("search"))
		mu.Unlock()
		assert.NoError(t, json.NewEncoder(w).Encode([]any{vllmRow("someone/Llama-Chat", nil)}))
	}))
	defer srv.Close()
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(t.Context(), "vllm", "", "", "  llama  ", "")

	require.NoError(t, err)
	assert.Equal(t, "llama", res.Query)
	require.Len(t, res.Models, 1)
	assert.Equal(t, "someone/Llama-Chat", res.Models[0].ID)
	require.NotEmpty(t, searches)
	assert.Equal(t, "llama", searches[0], "the upstream must be searched with the trimmed query")
	_, err = c.Catalog(t.Context(), "vllm", "", "", string(make([]byte, maxCatalogQuery+1)), "")
	assert.Error(t, err, "an over-long query is refused")
}

func TestParameterLabel_RoundsToMillionsOrBillions(t *testing.T) {
	test := func(name string, parameters int64, want string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, parameterLabel(parameters))
		})
	}
	test("millions round to the nearest million", 751_632_384, "752M")
	test("billions keep one decimal", 8_190_000_000, "8.2B")
	test("whole billions drop the decimal", 32_000_000_000, "32B")
	test("too few to label", 5_000, "")
}

func TestSafetensorsBytes_SumsKnownTensorTypesOnly(t *testing.T) {
	test := func(name string, parameters map[string]int64, want uint64) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, safetensorsBytes(parameters))
		})
	}
	test("two bytes per BF16 and four per F32", map[string]int64{"BF16": 10, "F32": 1}, 24)
	test("unknown when a type is not recognised", map[string]int64{"MYSTERY": 1}, 0)
}
