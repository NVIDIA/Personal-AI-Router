// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelScopeCatalog_ReturnsOnlyOpenGenerativeQ4Models(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/openapi/v1/models" {
			assert.Equal(t, "Qwen", r.URL.Query().Get("search"))
			assert.Equal(t, "gguf", r.URL.Query().Get("filter.library"))
			assert.Equal(t, "50", r.URL.Query().Get("page_size"))
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

// writeModelScopeQ4Listing answers a file-list request with one valid Q4_K_M
// model file.
func writeModelScopeQ4Listing(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeModelScopeListing(t, w, "model-Q4_K_M.gguf")
}

// writeModelScopeListing answers a file-list request with one valid file.
func writeModelScopeListing(t *testing.T, w http.ResponseWriter, path string) {
	t.Helper()
	assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"Success": true,
		"Data": map[string]any{"Files": []modelScopeFile{{
			Path: path, Type: "blob", Revision: "0123456789012345678901234567890123456789",
			SHA256: "0123456789012345678901234567890123456789012345678901234567890123", Size: 42,
		}}},
	}))
}

// closeNotifyingBody reports when the client has finished with a response.
type closeNotifyingBody struct {
	io.ReadCloser
	closed func()
}

func (b closeNotifyingBody) Close() error {
	err := b.ReadCloser.Close()
	b.closed()
	return err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// writeModelScopeChatPage answers a catalog request with one page of open GGUF
// repositories that each declare text generation.
func writeModelScopeChatPage(t *testing.T, w http.ResponseWriter, repos ...string) {
	t.Helper()
	entries := make([]map[string]any, len(repos))
	for i, repo := range repos {
		entries[i] = map[string]any{
			"id": repo, "gated": false, "tags": []string{"library:gguf"},
			"tasks": []string{"text-generation"}, "last_modified": "2026-01-01T00:00:00Z",
		}
	}
	assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"models": entries}}))
}

func TestModelScopeCatalog_TreatsUndeclaredTaskAsChatUnlessMarkedOtherwise(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi/v1/models" {
			_, err := w.Write([]byte(`{"success":true,"data":{"models":[
				{"id":"owner/DeepSeek-R1-GGUF","gated":false,"tags":["library:gguf","custom_tag:deepseek"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/Qwen3.6-35B-A3B-GGUF","gated":false,"tags":["library:gguf"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/VoxCPM2-GGUF","gated":false,"tags":["library:gguf","custom_tag:text-to-speech"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/LFM2.5-Audio-1.5B-JP-GGUF","gated":false,"tags":["library:gguf","custom_tag:speech-to-speech"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/LFM2-Audio-1.5B-GGUF","gated":false,"tags":["library:gguf"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/MiniMax-H3-GGUF","gated":false,"tags":["library:gguf","custom_tag:text-to-video"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/Wan2.2-Animate-14B-GGUF","gated":false,"tags":["library:gguf"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/Open_Text_To_Video-GGUF","gated":false,"tags":["library:gguf"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/image-studio-ltx-video","gated":false,"tags":["library:gguf"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/studio-GGUF","gated":false,"tags":["library:gguf","custom_tag:image-generation"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/Qwen3-Embedding-8B-GGUF","gated":false,"tags":["library:gguf"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/bge-m3-GGUF","gated":false,"tags":["library:gguf","custom_tag:sentence-embedding"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/declared-embedding","gated":false,"tags":["library:gguf"],"tasks":["sentence-embedding"],"last_modified":"2026-01-01T00:00:00Z"}
			]}}`))
			assert.NoError(t, err)
			return
		}
		writeModelScopeQ4Listing(t, w)
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	models, err := c.fetchModelScope(context.Background(), "")
	require.NoError(t, err)

	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	// ModelScope declares no task for many chat repos, so only a speech, image,
	// video, or embedding marker in the tags or name keeps an undeclared one
	// out. Version numbers in a name, as in Qwen3.6, are not markers.
	assert.ElementsMatch(t, []string{"owner/DeepSeek-R1-GGUF:Q4_K_M", "owner/Qwen3.6-35B-A3B-GGUF:Q4_K_M"}, ids)
}

func TestModelScopeCatalog_DropsMarkedRepositoryThatCarriesAChatTaskOrTag(t *testing.T) {
	var fileListRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi/v1/models" {
			_, err := w.Write([]byte(`{"success":true,"data":{"models":[
				{"id":"owner/voice-GGUF","gated":false,"tags":["library:gguf","custom_tag:tts","task:text-generation"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/nomic-embed-code-GGUF","gated":false,"tags":["library:gguf","custom_tag:sentence-similarity"],"tasks":["text-generation"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/whisper-large-v3-turbo-gguf","gated":false,"tags":["library:gguf"],"tasks":["text-generation"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/awesome-comfyui-models","gated":false,"tags":["library:gguf"],"tasks":["any-to-any"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/klein-GGUF","gated":false,"tags":["library:gguf","custom_tag:text-to-image"],"tasks":["image-text-to-text"],"last_modified":"2026-01-01T00:00:00Z"},
				{"id":"owner/code-GGUF","gated":false,"tags":["library:gguf","custom_tag:sentence-transformers"],"tasks":["text-generation"],"last_modified":"2026-01-01T00:00:00Z"}
			]}}`))
			assert.NoError(t, err)
			return
		}
		fileListRequests.Add(1)
		writeModelScopeQ4Listing(t, w)
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	models, err := c.fetchModelScope(context.Background(), "")
	require.NoError(t, err)

	// ModelScope declares a chat task on some embedding, speech, and image
	// repos, which would otherwise pass the generative check.
	assert.Empty(t, models)
	assert.Equal(t, int32(0), fileListRequests.Load())
}

func TestModelScopeCatalog_SkipsRepositoryWhoseFileListFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi/v1/models":
			writeModelScopeChatPage(t, w, "owner/a", "owner/b", "owner/c", "owner/broken")
		case "/api/v1/models/owner/broken/repo/files":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			writeModelScopeQ4Listing(t, w)
		}
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	// One in four is the most the failure rate allows.
	models, err := c.fetchModelScope(context.Background(), "")
	require.NoError(t, err)

	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	assert.ElementsMatch(t, []string{"owner/a:Q4_K_M", "owner/b:Q4_K_M", "owner/c:Q4_K_M"}, ids)
}

func TestModelScopeCatalog_FailsWhenMoreThanAQuarterOfFileListsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi/v1/models":
			writeModelScopeChatPage(t, w, "owner/a", "owner/b", "owner/c", "owner/broken-1", "owner/broken-2")
		case "/api/v1/models/owner/broken-1/repo/files", "/api/v1/models/owner/broken-2/repo/files":
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			writeModelScopeQ4Listing(t, w)
		}
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	// Two of five is an upstream in trouble, not two broken repositories; the
	// three left would be cached as the catalogue.
	_, err := c.fetchModelScope(context.Background(), "")
	assert.EqualError(t, err, "2 of 5 ModelScope file lists failed: ModelScope metadata returned HTTP 429")
}

func TestModelScopeCatalog_FailsWhenEveryFileListFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi/v1/models" {
			writeModelScopeChatPage(t, w, "owner/first", "owner/second")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	// One summary line rather than one per repository: the terminal status
	// line shows the message as is.
	_, err := c.fetchModelScope(context.Background(), "")
	assert.EqualError(t, err, "2 of 2 ModelScope file lists failed: ModelScope metadata returned HTTP 503")
}

func TestModelScopeCatalog_FailsWhenOnlyOversizedListingsSurviveAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi/v1/models":
			writeModelScopeChatPage(t, w, "owner/oversized", "owner/broken")
		case "/api/v1/models/owner/oversized/repo/files":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"Success": true, "Data": map[string]any{"Files": make([]modelScopeFile, 3000)}}))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	// An oversized repository is dropped without counting as listed, so the
	// one failure is every listing that could have counted.
	_, err := c.fetchModelScope(context.Background(), "")
	assert.EqualError(t, err, "1 of 1 ModelScope file lists failed: ModelScope metadata returned HTTP 503")
}

func TestModelScopeCatalog_ReturnsWhatItListedWhenTheRequestEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	const slowListing = "/api/v1/models/owner/slow/repo/files"
	var read sync.WaitGroup
	read.Add(8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi/v1/models":
			writeModelScopeChatPage(t, w, "owner/a", "owner/b", "owner/c", "owner/d", "owner/e", "owner/f", "owner/g", "owner/h", "owner/slow")
		case slowListing:
			read.Wait()
			cancel()
			<-r.Context().Done()
		default:
			writeModelScopeQ4Listing(t, w)
		}
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL
	transport := c.client.Transport
	c.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := transport.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/repo/files") && r.URL.Path != slowListing {
			resp.Body = closeNotifyingBody{ReadCloser: resp.Body, closed: read.Done}
		}
		return resp, err
	})

	// The request ends once the client has read every other listing, so the
	// slow one is the only failure: under the failure rate, which this fetch
	// would otherwise pass as complete.
	models, err := c.fetchModelScope(ctx, "")
	require.ErrorIs(t, err, errCatalogPartial)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "1 of 9 ModelScope file lists failed: context canceled")
	assert.Len(t, models, 8)
}

func TestModelScopeCatalog_FailsAnEmptyResultWhenAFileListFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi/v1/models":
			writeModelScopeChatPage(t, w, "owner/q8-a", "owner/q8-b", "owner/q8-c", "owner/broken")
		case "/api/v1/models/owner/broken/repo/files":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			writeModelScopeListing(t, w, "model-Q8_0.gguf")
		}
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	// One failure in four is under the failure rate, but the failed listing
	// may have held the only model the filters keep.
	_, err := c.fetchModelScope(context.Background(), "Qwen")
	assert.EqualError(t, err, "1 of 4 ModelScope file lists failed: ModelScope metadata returned HTTP 503")
}

func TestModelScopeCatalog_RunsEightFileListRequestsAtOnce(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi/v1/models" {
			repos := make([]string, modelScopeCatalogLimit)
			for i := range repos {
				repos[i] = fmt.Sprintf("owner/model-%d", i)
			}
			writeModelScopeChatPage(t, w, repos...)
			return
		}
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			seen := peak.Load()
			if current <= seen || peak.CompareAndSwap(seen, current) {
				break
			}
		}
		// Hold every request until the limit is reached, so the peak shows
		// the limit itself rather than how quickly requests happened to finish.
		if current == modelScopeFileListConcurrency {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Errorf("only %d file-list requests ran at once", current)
		}
		writeModelScopeQ4Listing(t, w)
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.modelScopeEndpoint = srv.URL

	models, err := c.fetchModelScope(context.Background(), "")
	require.NoError(t, err)
	assert.Len(t, models, modelScopeCatalogLimit)
	assert.Equal(t, int32(modelScopeFileListConcurrency), peak.Load())
}

func TestCatalog_FailsAnEmptyModelScopeBrowseList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`{"success":true,"data":{"models":[]}}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	catalog := newCatalogService()
	catalog.modelScopeEndpoint = server.URL

	// A search can match nothing, but the most-downloaded page cannot be
	// empty; caching it would show no models for catalogCacheTTL.
	_, err := catalog.Catalog(context.Background(), "llamacpp", "", "", "", "modelscope")
	assert.ErrorContains(t, err, "ModelScope catalog returned no models")
}

func TestCatalog_KeepsTheModelScopeBrowseListThroughManySearches(t *testing.T) {
	var browses atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path != "/openapi/v1/models":
			writeModelScopeQ4Listing(t, w)
		case r.URL.Query().Get("search") == "":
			browses.Add(1)
			writeModelScopeChatPage(t, w, "owner/popular")
		default:
			writeModelScopeChatPage(t, w)
		}
	}))
	t.Cleanup(server.Close)
	catalog := newCatalogService()
	catalog.modelScopeEndpoint = server.URL

	// The browse list is the fallback through a ModelScope outage, so searches
	// past the search cache's limit must not evict it.
	for i := 0; i <= searchCacheLimit; i++ {
		query := ""
		if i > 0 {
			query = fmt.Sprintf("query-%d", i)
		}
		_, err := catalog.Catalog(context.Background(), "llamacpp", "", "", query, "modelscope")
		require.NoError(t, err)
	}
	result, err := catalog.Catalog(context.Background(), "llamacpp", "", "", "", "modelscope")
	require.NoError(t, err)
	require.Len(t, result.Models, 1)
	assert.Equal(t, int32(1), browses.Load())
}

func TestCatalog_KeepsAModelScopeSearchResultThroughAnEmptyRefreshWithAFailure(t *testing.T) {
	var broken atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi/v1/models":
			writeModelScopeChatPage(t, w, "owner/a", "owner/q8-b", "owner/q8-c", "owner/q8-d")
		case "/api/v1/models/owner/a/repo/files":
			if broken.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writeModelScopeQ4Listing(t, w)
		default:
			writeModelScopeListing(t, w, "model-Q8_0.gguf")
		}
	}))
	t.Cleanup(server.Close)
	catalog := newCatalogService()
	catalog.modelScopeEndpoint = server.URL
	result, err := catalog.Catalog(context.Background(), "llamacpp", "", "", "qwen", "modelscope")
	require.NoError(t, err)
	require.Len(t, result.Models, 1)

	broken.Store(true)
	catalog.searchMu.Lock()
	search := catalog.searches["modelscope:qwen"]
	catalog.searchMu.Unlock()
	search.mu.Lock()
	search.fetched = time.Now().Add(-2 * catalogCacheTTL)
	search.mu.Unlock()

	result, err = catalog.Catalog(context.Background(), "llamacpp", "", "", "qwen", "modelscope")
	require.NoError(t, err)
	assert.Len(t, result.Models, 1, "an empty refresh with a failed listing must not replace the result")
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
