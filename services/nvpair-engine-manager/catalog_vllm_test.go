// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
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

func TestVLLMCatalogKeepsOpenSafetensorsChatModels(t *testing.T) {
	var mu sync.Mutex
	var queries []map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		if r.URL.Query().Get("pipeline_tag") == "image-text-to-text" {
			_ = json.NewEncoder(w).Encode([]any{vllmRow("Qwen/Vision-8B", func(m *map[string]any) { (*m)["pipeline_tag"] = "image-text-to-text"; (*m)["downloads"] = 900 })})
			return
		}
		_ = json.NewEncoder(w).Encode([]any{
			vllmRow("Qwen/Chat-8B", nil),
			vllmRow("Qwen/Gated", func(m *map[string]any) { (*m)["gated"] = "auto" }),
			vllmRow("Qwen/Private", func(m *map[string]any) { (*m)["private"] = true }),
			vllmRow("Qwen/Embed", func(m *map[string]any) { (*m)["pipeline_tag"] = "feature-extraction" }),
			vllmRow("Qwen/NoSafetensors", func(m *map[string]any) { (*m)["tags"] = []string{"gguf", "text-generation"} }),
			vllmRow("mlx-community/Chat-4bit", func(m *map[string]any) { (*m)["tags"] = []string{"safetensors", "mlx", "text-generation"} }),
			vllmRow("../bad", nil),
		})
	}))
	defer srv.Close()
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(context.Background(), "vllm", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range res.Models {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"Qwen/Vision-8B", "Qwen/Chat-8B"}) {
		t.Fatalf("ids = %v, want the open chat models, most downloaded first", ids)
	}
	if !res.Searchable {
		t.Fatal("the vLLM catalog can be searched")
	}
	m := res.Models[1]
	if m.Name != "Qwen/Chat-8B" || m.ParameterSize != "8B" || m.Size != 16_000_000_000 || m.Author != "Qwen" {
		t.Fatalf("model = %+v, want a pull-ready name, 8B and 16 GB", m)
	}
	pipelines := map[string]bool{}
	for _, q := range queries {
		pipelines[q["pipeline_tag"][0]] = true
		if q["filter"][0] != "safetensors" || !slices.Contains(q["expand[]"], "safetensors") {
			t.Fatalf("a listing must filter on safetensors and expand them: %v", q)
		}
	}
	if !pipelines["text-generation"] || !pipelines["image-text-to-text"] {
		t.Fatalf("both chat pipelines must be listed: %v", pipelines)
	}
}

func TestVLLMCatalogSearchesWithTheQuery(t *testing.T) {
	var mu sync.Mutex
	var searches []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		searches = append(searches, r.URL.Query().Get("search"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode([]any{vllmRow("someone/Llama-Chat", nil)})
	}))
	defer srv.Close()
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(context.Background(), "vllm", "", "", "  llama  ")
	if err != nil {
		t.Fatal(err)
	}
	if res.Query != "llama" || len(res.Models) != 1 || res.Models[0].ID != "someone/Llama-Chat" {
		t.Fatalf("result = %+v", res)
	}
	if len(searches) == 0 || searches[0] != "llama" {
		t.Fatalf("the upstream must be searched with the trimmed query: %v", searches)
	}
	if _, err := c.Catalog(context.Background(), "vllm", "", "", string(make([]byte, maxCatalogQuery+1))); err == nil {
		t.Fatal("an over-long query is refused")
	}
}

func TestParameterLabelAndSize(t *testing.T) {
	for n, want := range map[int64]string{751_632_384: "752M", 8_190_000_000: "8.2B", 32_000_000_000: "32B", 5_000: ""} {
		if got := parameterLabel(n); got != want {
			t.Errorf("parameterLabel(%d) = %q, want %q", n, got, want)
		}
	}
	if safetensorsBytes(map[string]int64{"BF16": 10, "F32": 1}) != 24 || safetensorsBytes(map[string]int64{"MYSTERY": 1}) != 0 {
		t.Error("size sums known tensor types and is unknown when one is not recognised")
	}
}
