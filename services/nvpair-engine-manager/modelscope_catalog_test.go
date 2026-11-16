// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelScopeCatalog_ReturnsOnlyOpenGenerativeQ4Models(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/openapi/v1/models" {
			assert.Equal(t, "Qwen", r.URL.Query().Get("search"))
			assert.Equal(t, "gguf", r.URL.Query().Get("filter.library"))
			assert.Equal(t, "10", r.URL.Query().Get("page_size"))
			_, err := w.Write([]byte(`{"success":true,"data":{"models":[
				{"id":"owner/chat","gated":false,"tags":["library:gguf"],"tasks":["text-generation"],"last_modified":"2026-01-01T00:00:00Z","downloads":12},
				{"id":"owner/q8","gated":false,"tags":["library:gguf"],"tasks":["text-generation"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/large","gated":false,"tags":["library:gguf"],"tasks":["text-generation"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/embedding","gated":false,"tags":["library:gguf"],"tasks":["sentence-embedding"]},
				{"id":"owner/private","private":true,"gated":false,"tags":["library:gguf"],"tasks":["text-generation"]},
				{"id":"owner/gated","gated":true,"tags":["library:gguf"],"tasks":["text-generation"]}
			]}}`))
			assert.NoError(t, err)
			return
		}
		assert.Equal(t, "master", r.URL.Query().Get("Revision"))
		assert.Equal(t, "true", r.URL.Query().Get("Recursive"))
		file := "model-Q4_K_M-00001-of-00002.gguf"
		switch r.URL.Path {
		case "/api/v1/models/owner/chat/repo/files":
		case "/api/v1/models/owner/q8/repo/files":
			file = "model-Q8_0.gguf"
		case "/api/v1/models/owner/large/repo/files":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"Success": true, "Data": map[string]any{"Files": make([]modelScopeFile, 3000)}}))
			return
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"Success": true,
			"Data": map[string]any{"Files": []modelScopeFile{{
				Path: file, Type: "blob", Revision: "0123456789012345678901234567890123456789",
				SHA256: "0123456789012345678901234567890123456789012345678901234567890123", Size: 42,
			}}},
		}))
	}))
	defer srv.Close()
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	models, err := c.fetchModelScope(context.Background(), "Qwen")
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "owner/chat:Q4_K_M", models[0].ID)
	assert.Equal(t, "https://modelscope.cn/models/owner/chat", models[0].URL)
	assert.Equal(t, 12, models[0].Downloads)
}

func TestModelScopeCatalog_AcceptsAnEmptySearchResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`{"success":true,"data":{"models":[]}}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL
	models, err := c.fetchModelScope(context.Background(), "absent")
	require.NoError(t, err)
	assert.Empty(t, models)
}

func TestCatalog_KeepsSourceCachesSeparateWhenSelectionChanges(t *testing.T) {
	var huggingFaceCalls, modelScopeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/openapi/") {
			modelScopeCalls.Add(1)
			_, err := w.Write([]byte(`{"success":true,"data":{"models":[]}}`))
			assert.NoError(t, err)
		} else {
			huggingFaceCalls.Add(1)
			_, err := w.Write([]byte(`[]`))
			assert.NoError(t, err)
		}
	}))
	t.Cleanup(server.Close)
	catalog := newCatalogService()
	catalog.baseURL, catalog.modelScopeEndpoint = server.URL, server.URL
	selected := "huggingface"
	catalog.llamaSource = func() (string, error) { return selected, nil }
	for _, source := range []string{"huggingface", "modelscope", "huggingface", "modelscope"} {
		selected = source
		result, err := catalog.Catalog(context.Background(), "llamacpp", "", "", "absent", "")
		require.NoError(t, err)
		assert.Contains(t, result.Source, source)
	}
	assert.Equal(t, int32(1), huggingFaceCalls.Load())
	assert.Equal(t, int32(1), modelScopeCalls.Load())
}

func TestModelScopeCatalog_DoesNotRevealSearchTextInErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL
	_, err := c.fetchModelScope(context.Background(), "private model name")
	require.Error(t, err)
	var urlErr *url.Error
	assert.NotErrorAs(t, err, &urlErr)
	assert.NotContains(t, err.Error(), "private model name")
}

func TestRunCatalog_UsesRequestedSourceInsteadOfLocalSelection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `[]`
		if r.URL.Path == "/openapi/v1/models" {
			body = `{"success":true,"data":{"models":[]}}`
		}
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	test := func(name, localSource, peerSource, expectedURL string) {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			executor := NewExecutor(loadWithOverrides(t, t.TempDir()), NewReporter(nil), func(string, any) {}, t.TempDir())
			manager := NewManager(NewCodec(&output), executor, nil)
			manager.catalog.baseURL, manager.catalog.modelScopeEndpoint = server.URL, server.URL
			manager.catalog.llamaSource = func() (string, error) { return localSource, nil }
			params, err := json.Marshal(catalogParams{Engine: "llamacpp", Query: "absent", ModelSource: peerSource})
			require.NoError(t, err)
			id := json.RawMessage("1")
			manager.runCatalog(context.Background(), &Message{ID: &id, Params: params})
			var reply Message
			require.NoError(t, json.Unmarshal(output.Bytes(), &reply))
			require.Nil(t, reply.Error)
			var result CatalogResult
			require.NoError(t, json.Unmarshal(reply.Result, &result))
			assert.Equal(t, expectedURL, result.Source)
		})
	}
	test("peer uses ModelScope", "huggingface", "modelscope", "https://modelscope.cn/models")
	test("peer uses Hugging Face", "modelscope", "huggingface", "https://huggingface.co/api/models?filter=gguf&search=absent")
}
