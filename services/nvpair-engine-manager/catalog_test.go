// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/jsonrpc"
)

// TestOllamaCatalogLoadsFromEmbeddedFile checks the committed list is compiled in
// and parses. If the embed directive or the file shape ever breaks, the catalog
// silently becomes empty, and an empty catalog looks identical to "this engine
// has nothing to offer".
func TestOllamaCatalog_LoadsFromEmbeddedFile(t *testing.T) {
	c := newCatalogService()
	res, err := c.Catalog(t.Context(), "ollama", "linux", "amd64", "")
	require.NoError(t, err, "ollama catalog")
	require.NotEmpty(t, res.Models, "embedded ollama catalog is empty")
	assert.NotEmpty(t, res.FetchedAt, "no scrape timestamp; an operator cannot tell how stale the list is")
	assert.Contains(t, res.Source, "ollama.com")

	for _, m := range res.Models {
		require.NotEmpty(t, m.Name, "a model has no pull name")
		assert.Equal(t, m.Name, m.ID, "id and name must both be pull-ready")
		assert.True(t, strings.HasPrefix(m.URL, "https://ollama.com/library/"), "model %q URL %q", m.Name, m.URL)
		// The tag must not leak into the library URL path.
		assert.NotContains(t, strings.TrimPrefix(m.URL, "https://ollama.com/library/"), ":", "model %q URL carries a tag", m.Name)
	}
}

// The embedded list has several thousand entries, so requests for different
// targets must reuse it while keeping their own target metadata.
func TestOllamaCatalog_ReusesModelsAcrossRequestTargets(t *testing.T) {
	c := newCatalogService()
	first, err := c.Catalog(t.Context(), "ollama", "linux", "amd64", "")
	require.NoError(t, err, "first load")
	second, err := c.Catalog(t.Context(), "ollama", "darwin", "arm64", "")
	require.NoError(t, err, "second load")
	assert.Equal(t, "linux", first.Platform)
	assert.Equal(t, "amd64", first.Arch)
	assert.Equal(t, "darwin", second.Platform)
	assert.Equal(t, "arm64", second.Arch)
	require.NotEmpty(t, first.Models)
	require.Len(t, second.Models, len(first.Models), "repeat load produced a different list")
	assert.Same(t, &first.Models[0], &second.Models[0], "catalog re-parsed instead of being reused")
}

// TestOllamaCatalogFitsInAFrame is the guard for the failure that made this
// method unusable: the reply is one JSON-RPC line, and a line over the
// worker-path frame cap is a terminal read error, not a dropped message. The
// broker's peer then closes while the child keeps running, so nothing restarts
// and every later engine:* call hangs.
//
// The check is on the marshalled result rather than the model count, because it
// is bytes on the wire that matter and a regenerated catalog can grow either by
// adding rows or by widening them.
func TestOllamaCatalog_FitsInAFrame(t *testing.T) {
	c := newCatalogService()
	res, err := c.Catalog(t.Context(), "ollama", "linux", "amd64", "")
	require.NoError(t, err, "ollama catalog")
	body, err := json.Marshal(res)
	require.NoError(t, err, "marshal catalog")
	// The real frame carries a JSON-RPC envelope around this; leave room for it.
	const envelopeAllowance = 4096
	assert.LessOrEqual(t, len(body)+envelopeAllowance, jsonrpc.WorkerFrameBytes,
		"filter or paginate engine:catalog rather than raising the frame cap again")
	t.Logf("ollama catalog: %d models, %d bytes (%.0f%% of the %d-byte frame cap)",
		len(res.Models), len(body),
		100*float64(len(body))/float64(jsonrpc.WorkerFrameBytes), jsonrpc.WorkerFrameBytes)
}

// TestLmStudioCatalogCoalescesConcurrentCallers is the guard for the request
// herd. Both front ends plus the warm-up can ask at once, and the point of the
// in-flight channel is that they share one upstream request.
//
// Run under -race this also covers the close-under-lock fix: the channel used to
// be closed after releasing the mutex, leaving a window where an arriving caller
// saw no request in flight and started a second one.
func TestLmStudioCatalog_CoalescesConcurrentCallers(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		<-release // hold the request open so the callers genuinely overlap
		writeTestResponse(t, w, []byte(`[{"id":"lmstudio-community/Model-GGUF","downloads":5}]`))
	}))
	defer srv.Close()

	c := newCatalogService()
	c.baseURL = srv.URL

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	counts := make([]int, callers)
	for i := range callers {
		wg.Go(func() {
			models, _, err := c.lmStudio.get(t.Context())
			errs[i], counts[i] = err, len(models)
		})
	}

	// Let the callers pile up behind the one in flight, then answer.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), requests.Load(), "upstream requests from %d callers", callers)
	for i := range callers {
		require.NoError(t, errs[i], "caller %d", i)
		assert.Equal(t, 1, counts[i], "caller %d model count", i)
	}
}

// TestLmStudioCatalogBacksOffAfterFailure is the guard for a stall: only a
// success stamped the cache, so against a dead upstream every later call retried
// and waited out the full timeout before handing back the same stale list.
func TestLmStudioCatalog_BacksOffAfterFailure(t *testing.T) {
	var requests atomic.Int32
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		writeTestResponse(t, w, []byte(`[{"id":"lmstudio-community/Model-GGUF","downloads":5}]`))
	}))
	defer srv.Close()

	c := newCatalogService()
	c.baseURL = srv.URL

	// Seed a good list, then start failing.
	fail = false
	_, _, err := c.lmStudio.get(t.Context())
	require.NoError(t, err, "seed fetch")
	fail = true

	// Force a refresh by ageing the cache past its TTL.
	c.lmStudio.mu.Lock()
	c.lmStudio.fetched = time.Now().Add(-2 * catalogCacheTTL)
	c.lmStudio.mu.Unlock()

	before := requests.Load()
	for range 3 {
		models, _, err := c.lmStudio.get(t.Context())
		require.NoError(t, err, "a failed refresh should still serve the stale list")
		assert.Len(t, models, 1, "stale list lost its models")
	}
	assert.Equal(t, int32(1), requests.Load()-before, "three calls after a failure must back off after one request")
}

// TestLmStudioCatalogBacksOffWithNothingCached is the same guard for a machine
// that has never reached the upstream. The backoff was keyed on having a list
// to serve, so with an empty cache every call started a fresh fetch and waited
// out the full timeout — the warm-up, then each modal open, then the terminal
// browser, each in turn.
func TestLmStudioCatalog_BacksOffWithNothingCached(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newCatalogService()
	c.baseURL = srv.URL

	for i := range 3 {
		_, _, err := c.lmStudio.get(t.Context())
		require.Error(t, err, "call %d: a failing upstream with nothing cached returned no error", i)
	}
	assert.Equal(t, int32(1), requests.Load(), "three calls against a failing upstream must back off after one request")

	// Once the backoff has run out, the next call tries again.
	c.lmStudio.mu.Lock()
	c.lmStudio.failed = time.Now().Add(-2 * catalogRetryAfterFailure)
	c.lmStudio.mu.Unlock()
	_, _, err := c.lmStudio.get(t.Context())
	assert.Error(t, err, "retry against a failing upstream must still report failure")
	assert.Equal(t, int32(2), requests.Load(), "backoff expiry must permit a second attempt")
}

func TestNormalizeCatalogEngine_ResolvesSupportedEngineNames(t *testing.T) {
	test := func(name, input, want string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, normalizeCatalogEngine(input))
		})
	}
	test("canonical Ollama", "ollama", "ollama")
	test("case insensitive Ollama", "Ollama", "ollama")
	test("trimmed Ollama", "  ollama ", "ollama")
	test("canonical LM Studio", "lmstudio", "lmstudio")
	test("spaced LM Studio", "LM Studio", "lmstudio")
	test("hyphenated LM Studio", "lm-studio", "lmstudio")
	test("canonical llama.cpp", "llamacpp", "llamacpp")
	test("hyphenated llama.cpp", "llama-cpp", "llamacpp")
	test("dotted llama.cpp", "llama.cpp", "llamacpp")
	test("capitalized Colibri", "Colibri", "colibri")
	test("other engine preserved", "vllm", "vllm")
}

// TestCatalogRejectsUnknownEngine checks an engine with no curated source errors
// rather than returning an empty list that reads as "nothing available".
func TestCatalog_RejectsUnknownEngine(t *testing.T) {
	c := newCatalogService()
	_, err := c.Catalog(t.Context(), "vllm", "linux", "amd64", "")
	assert.Error(t, err, "unknown engine returned a catalog")
}

// TestNormalizeHFRowsMarksMLX checks Apple-only quantizations are labelled
// rather than dropped during normalization. `lms get` refuses them anywhere but
// Apple Silicon, but the machine that asks is not always the machine that
// installs, so the drop decision belongs to the caller's platform.
func TestNormalizeHFRows_MarksMLX(t *testing.T) {
	rows := []hfModelRow{
		{ID: "lmstudio-community/Qwen3-8B-GGUF", Downloads: 10},
		{ID: "lmstudio-community/Qwen3-8B-MLX-4bit", Downloads: 99},
		{ID: "lmstudio-community/Tagged-Model", Downloads: 50, Tags: []string{"MLX"}},
	}

	got := normalizeHFRows(rows)
	require.Len(t, got, 3, "all models must survive normalization; filtering happens later")
	marked := map[string]bool{}
	for _, m := range got {
		marked[m.ID] = m.AppleOnly
	}
	assert.False(t, marked["lmstudio-community/Qwen3-8B-GGUF"], "a GGUF model was marked Apple-only")
	assert.True(t, marked["lmstudio-community/Qwen3-8B-MLX-4bit"], "an MLX repo id was not marked Apple-only")
	assert.True(t, marked["lmstudio-community/Tagged-Model"], "an MLX tag was not marked Apple-only")
}

// TestFilterForTarget is the guard for two versions of one bug. The list was
// first filtered by whichever machine served it, so browsing for a Mac peer
// from a Linux box hid every model that peer could use; then by operating
// system alone, so an Intel Mac was offered MLX models it can never install.
func TestFilterForTarget_RestrictsMLXModelsToAppleSilicon(t *testing.T) {
	models := []CatalogModel{
		{ID: "plain/gguf"},
		{ID: "apple/mlx", AppleOnly: true},
	}

	assert.Len(t, filterForTarget(models, "darwin", "arm64"), 2, "Apple Silicon target must keep both models")
	test := func(name, platform, arch string) {
		t.Run(name, func(t *testing.T) {
			got := filterForTarget(models, platform, arch)
			if assert.Len(t, got, 1, "target must keep only the portable model") {
				assert.Equal(t, "plain/gguf", got[0].ID)
			}
		})
	}
	test("Intel Mac", "darwin", "amd64")
	test("Mac with unknown architecture", "darwin", "")
	test("ARM Linux", "linux", "arm64")
	test("Windows", "windows", "amd64")
}

// TestCatalogEchoesTarget checks the reply says which machine it was filtered
// for, so a client can tell the operator rather than presenting a filtered list
// as universal.
func TestCatalog_EchoesTarget(t *testing.T) {
	c := newCatalogService()
	res, err := c.Catalog(t.Context(), "ollama", "darwin", "amd64", "")
	require.NoError(t, err, "catalog")
	assert.Equal(t, "darwin", res.Platform)
	assert.Equal(t, "amd64", res.Arch)

	// Omitting both means this host.
	res, err = c.Catalog(t.Context(), "ollama", "", "", "")
	require.NoError(t, err, "catalog")
	assert.Equal(t, runtime.GOOS, res.Platform)
	assert.Equal(t, runtime.GOARCH, res.Arch)

	// A named platform does not borrow this host's architecture: that would
	// describe a machine the caller did not ask about.
	res, err = c.Catalog(t.Context(), "ollama", "darwin", "", "")
	require.NoError(t, err, "catalog")
	assert.Empty(t, res.Arch, "a platform named without an architecture must leave it unknown")
}

// TestNormalizeHFRowsSortsByDownloads checks the most-used models lead, which is
// what makes an unfiltered first page useful.
func TestNormalizeHFRows_SortsByDownloads(t *testing.T) {
	rows := []hfModelRow{
		{ID: "a/low", Downloads: 1},
		{ID: "a/high", Downloads: 100},
		{ID: "a/mid", Downloads: 50},
	}
	got := normalizeHFRows(rows)
	assert.Equal(t, "a/high", got[0].ID, "most downloaded model must lead")
	assert.Equal(t, "a/low", got[2].ID, "least downloaded model must come last")
}

// TestNormalizeHFRowsSkipsUnusableEntries checks an entry with no id is dropped
// rather than becoming a row that cannot be pulled.
func TestNormalizeHFRows_SkipsUnusableEntries(t *testing.T) {
	rows := []hfModelRow{
		{ID: "", ModelID: ""},
		{ID: "", ModelID: "a/from-modelid"},
		{ID: "a/normal"},
	}
	got := normalizeHFRows(rows)
	require.Len(t, got, 2)
	for _, m := range got {
		assert.NotEmpty(t, m.Name, "kept a row with no pull name")
		assert.NotEmpty(t, m.ID, "kept a row with no pull id")
		assert.True(t, strings.HasPrefix(m.URL, "https://huggingface.co/"), "URL %q", m.URL)
		assert.Equal(t, "a", m.Author, "author must be the id's owner")
	}
}

// TestNormalizeOllamaRowsDedupes checks a duplicated pull name yields one row.
func TestNormalizeOllamaRows_Dedupes(t *testing.T) {
	rows := []ollamaLibraryRow{
		{Name: "llama3.2:latest", Size: 1},
		{Name: "llama3.2:latest", Size: 2},
		{Name: "qwen3:8b"},
		{Name: "   "},
	}
	got := normalizeOllamaRows(rows)
	require.Len(t, got, 2)
	for _, m := range got {
		if m.Name == "llama3.2:latest" {
			assert.Equal(t, uint64(2), m.Size, "duplicate must keep the later entry's size")
		}
	}
}

// ggufRow is a listing entry llama.cpp can download, for a test to break one
// field of.
func ggufRow(id string, downloads int, files ...string) hfModelRow {
	if len(files) == 0 {
		files = []string{"model-Q4_K_M.gguf"}
	}
	siblings := make([]hfSibling, len(files))
	for i, f := range files {
		siblings[i] = hfSibling{RFilename: f}
	}
	return hfModelRow{
		ID:           id,
		Downloads:    downloads,
		LastModified: "2026-09-01T12:00:00.000Z",
		Tags:         []string{"gguf", "conversational"},
		Gated:        json.RawMessage(`false`),
		PipelineTag:  "text-generation",
		Siblings:     siblings,
	}
}

// TestNormalizeLlamaCPPRowsKeepsOnlyDownloadable is the guard for offering a
// download that cannot succeed. The listing's GGUF filter also matches gated
// and private repos, embedding models, and repos with no file at the offered
// quantization, and every one of those fails only after the operator picked it.
func TestNormalizeLlamaCPPRows_KeepsOnlyDownloadableModels(t *testing.T) {
	gated := ggufRow("a/gated", 1)
	gated.Gated = json.RawMessage(`"auto"`)
	unstated := ggufRow("a/unstated", 1)
	unstated.Gated = nil
	private := ggufRow("a/private", 1)
	private.Private = true
	untagged := ggufRow("a/untagged", 1)
	untagged.Tags = []string{"conversational"}
	embedding := ggufRow("a/embedding", 1)
	embedding.PipelineTag = "feature-extraction"
	undated := ggufRow("a/undated", 1)
	undated.LastModified = "yesterday"

	rows := []hfModelRow{
		ggufRow("ggml-org/kept", 10),
		ggufRow("a/split", 5, "q4/model-Q4_K_M-00001-of-00002.gguf", "q4/model-Q4_K_M-00002-of-00002.gguf"),
		gated, unstated, private, untagged, embedding, undated,
		ggufRow("a/no-q4", 1, "model-Q8_0.gguf"),
		ggufRow("a/projector-only", 1, "mmproj-model-Q4_K_M.gguf"),
		ggufRow("a/eagle3-head-only", 1, "eagle3-model-Q4_K_M.gguf"),
		ggufRow("a/dflash-head-only", 1, "dflash-model-Q4_K_M.gguf"),
		ggufRow("a/dspark-head-only", 1, "dspark-model-Q4_K_M.gguf"),
		ggufRow("a/second-shard-only", 1, "model-Q4_K_M-00002-of-00002.gguf"),
		ggufRow("../escape", 1),
		ggufRow("a/has space", 1),
	}
	got := normalizeLlamaCPPRows(rows)
	ids := make([]string, len(got))
	for i, m := range got {
		ids[i] = m.ID
	}
	require.Equal(t, []string{"ggml-org/kept:Q4_K_M", "a/split:Q4_K_M"}, ids, "keep only downloadable repos, most downloaded first")

	m := got[0]
	assert.Equal(t, m.ID, m.Name, "name and id must both be the download name")
	assert.Equal(t, "ggml-org", m.Author)
	assert.Equal(t, "https://huggingface.co/ggml-org/kept", m.URL)
	assert.True(t, hasTag(m.Tags, "Q4_K_M"), "tags must name the quantization offered")
}

// TestNormalizeLlamaCPPRowsDedupesAcrossPublishers checks a repo listed twice,
// as it is when a search and a publisher page overlap, yields one row.
func TestNormalizeLlamaCPPRows_DedupesAcrossPublishers(t *testing.T) {
	got := normalizeLlamaCPPRows([]hfModelRow{ggufRow("a/model", 1), ggufRow("a/model", 7)})
	if assert.Len(t, got, 1, "one row must carry the later entry") {
		assert.Equal(t, 7, got[0].Downloads)
	}
}

// llamaCPPServer answers the listing for each approved publisher with one repo
// of its own, and a search with a repo named after the query. It records every
// request's query string.
func llamaCPPServer(t *testing.T, failAuthor string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		requests = append(requests, r.URL.RawQuery)
		mu.Unlock()
		assert.Equal(t, "gguf", q.Get("filter"), "listing must request GGUF repos")
		assert.Equal(t, "true", q.Get("full"), "listing must request full repos")
		var rows []hfModelRow
		switch author, search := q.Get("author"), q.Get("search"); {
		case author != "" && author == failAuthor:
			w.WriteHeader(http.StatusBadGateway)
			return
		case author != "":
			rows = []hfModelRow{ggufRow(author+"/model-GGUF", 1)}
		case search == "nothing":
		case search != "":
			rows = []hfModelRow{ggufRow("found/"+strings.ReplaceAll(search, " ", "-"), 1)}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(rows), "encode catalog response")
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
}

// TestLlamaCPPCatalogReadsEveryPublisher checks the browse list is the union of
// the approved publishers, and that the reply says a search is possible.
func TestLlamaCPPCatalog_ReadsEveryPublisher(t *testing.T) {
	srv, _ := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(t.Context(), "llama.cpp", "linux", "arm64", "")
	require.NoError(t, err, "llama.cpp catalog")
	require.Len(t, res.Models, len(llamaCPPPublishers), "one model from each publisher")
	for _, publisher := range llamaCPPPublishers {
		assert.True(t, slices.ContainsFunc(res.Models, func(m CatalogModel) bool { return m.Author == publisher }), "nothing from %s", publisher)
	}
	assert.True(t, res.Searchable, "browse list must be searchable")
	assert.Empty(t, res.Query, "browse list must not have a query")
}

// TestLlamaCPPCatalogFailsWhole checks one publisher failing fails the fetch,
// rather than caching for six hours a list that is silently missing it.
func TestLlamaCPPCatalog_FailsWhenAnyPublisherRequestFails(t *testing.T) {
	srv, _ := llamaCPPServer(t, "bartowski")
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(t.Context(), "llamacpp", "", "", "")
	assert.Error(t, err, "a failed publisher still produced %d models", len(res.Models))
}

// TestLlamaCPPSearch checks a query reaches the upstream once, is cached
// regardless of case, and that a search matching nothing is an answer rather
// than a failure.
func TestLlamaCPPSearch_ReturnsMatchingModelsAndCachesResults(t *testing.T) {
	srv, requests := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(t.Context(), "llamacpp", "", "", "  Gemma 3  ")
	require.NoError(t, err, "search")
	require.Equal(t, "Gemma 3", res.Query)
	require.Len(t, res.Models, 1)
	require.Equal(t, "found/Gemma-3:Q4_K_M", res.Models[0].ID)
	_, err = c.Catalog(t.Context(), "llamacpp", "", "", "gemma 3")
	require.NoError(t, err, "repeat search")
	assert.Len(t, requests(), 1, "the same query twice must make one request")

	res, err = c.Catalog(t.Context(), "llamacpp", "", "", "nothing")
	require.NoError(t, err, "a search matching nothing failed")
	assert.Empty(t, res.Models, "a query that matches none must return no models")
}

// TestFailedSearchDoesNotRevealTheQuery is the guard for search text leaking
// into logs. net/http puts the request URL in its error, and a search's URL
// carries what the operator typed; returned as-is, it was logged here and by
// every client that relays the error.
//
// The message cannot be compared whole: it ends in the operating system's
// connection error, which names a random port. What has to hold is that the
// error carrying the URL is gone.
func TestFailedSearch_DoesNotRevealTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // every request now fails to connect
	c := newCatalogService()
	c.baseURL = srv.URL

	_, err := c.Catalog(t.Context(), "llamacpp", "", "", "private model name")
	require.Error(t, err, "a search against an unreachable upstream succeeded")
	var urlErr *url.Error
	assert.NotErrorAs(t, err, &urlErr, "the error still carries the request URL")
}

// TestOverLongSearchIsRefused checks a query past maxCatalogQuery is an error
// that never reaches the upstream, rather than being shortened into a search
// for something the operator did not type, and that the error does not repeat
// the query.
func TestOverLongSearch_IsRefused(t *testing.T) {
	srv, requests := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	query := strings.TrimSpace(strings.Repeat("private ", maxCatalogQuery/len("private ")+1))
	_, err := c.Catalog(t.Context(), "llamacpp", "", "", query)
	require.Error(t, err, "a %d-character query was answered", len(query))
	assert.EqualError(t, err, "a model search is limited to 100 characters")
	assert.Empty(t, requests(), "an over-long query reached the upstream")
}

// TestSearchAtTheLengthLimitIsAnswered checks the limit counts the query the
// operator meant: exactly maxCatalogQuery characters is searched in full, and
// surrounding space does not count against it.
func TestSearchAtTheLengthLimit_IsAnswered(t *testing.T) {
	srv, requests := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	query := strings.Repeat("a", maxCatalogQuery)
	res, err := c.Catalog(t.Context(), "llamacpp", "", "", "  "+query+"  ")
	require.NoError(t, err, "a query at the limit failed")
	assert.Equal(t, query, res.Query, "a query at the limit must be searched in full")
	assert.Len(t, requests(), 1, "a query at the limit must make one request")
}

// TestCatalogSearch_BoundsQueriesAcrossSources checks the per-query caches are
// evicted least recently used first, since the set of possible queries is
// unbounded, and that the sources share the bound without sharing entries.
func TestCatalogSearch_BoundsQueriesAcrossSources(t *testing.T) {
	c := newCatalogService()
	first := c.llamaCPPSearch("first")
	require.NotSame(t, first, c.colibriSearch("first"), "one query of two sources shares a cache")
	for i := range catalogSearchCacheLimit {
		c.llamaCPPSearch(fmt.Sprint("query ", i))
	}
	assert.Len(t, c.searches, catalogSearchCacheLimit)
	assert.NotSame(t, first, c.llamaCPPSearch("first"), "the least recently used search was not evicted")
}

// TestQueryIsIgnoredByASourceThatCannotSearch checks a query sent for an engine
// whose source cannot search returns the whole list, marked unsearchable, for
// the client to filter itself.
func TestQuery_IsIgnoredByASourceThatCannotSearch(t *testing.T) {
	c := newCatalogService()
	all, err := c.Catalog(t.Context(), "ollama", "", "", "")
	require.NoError(t, err, "ollama catalog")
	queried, err := c.Catalog(t.Context(), "ollama", "", "", "llama")
	require.NoError(t, err, "ollama catalog with a query")
	assert.False(t, queried.Searchable)
	assert.Empty(t, queried.Query)
	assert.Len(t, queried.Models, len(all.Models))
}

// colibriRow is an open, complete Colibri model container.
func colibriRow(id string, downloads int) hfModelRow {
	return hfModelRow{
		ID:           id,
		Downloads:    downloads,
		LastModified: "2026-10-02T12:00:00.000Z",
		Tags:         []string{"colibri", "moe", "int4"},
		Gated:        json.RawMessage("false"),
		PipelineTag:  "text-generation",
		Siblings:     []hfSibling{{RFilename: "config.json"}, {RFilename: "model-00000.safetensors"}},
	}
}

// TestNormalizeColibriRowsKeepsOnlyServable checks the Colibri list offers
// only open, complete containers the upstream engine can serve, most
// downloaded first, each under its pull-ready repository id.
func TestNormalizeColibriRowsKeepsOnlyServable(t *testing.T) {
	gated := colibriRow("o/gated", 9)
	gated.Gated = json.RawMessage(`"manual"`)
	private := colibriRow("o/private", 9)
	private.Private = true
	untagged := colibriRow("o/untagged", 9)
	untagged.Tags = []string{"moe"}
	noConfig := colibriRow("o/no-config", 9)
	noConfig.Siblings = []hfSibling{{RFilename: "sub/config.json"}}
	image := colibriRow("o/image", 9)
	image.PipelineTag = "text-to-image"
	unknownPipeline := colibriRow("o/no-pipeline", 3)
	unknownPipeline.PipelineTag = ""
	speedyTag := colibriRow("o/fork", 9)
	speedyTag.Tags = append(speedyTag.Tags, "speedycolibri")
	badDate := colibriRow("o/bad-date", 9)
	badDate.LastModified = "yesterday"

	got := normalizeColibriRows([]hfModelRow{
		colibriRow("o/small", 1),
		gated, private, untagged, noConfig, image, speedyTag, badDate,
		colibriRow("o/GLM-5.2-speedy-colibri-nvfp4", 9),
		colibriRow("../escape", 9),
		unknownPipeline,
		colibriRow("o/big", 50),
		colibriRow("o/small", 2), // a duplicate replaces the earlier entry
	}, nil)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
		if m.Name != m.ID || m.URL != "https://huggingface.co/"+m.ID || m.Author != "o" {
			t.Errorf("%+v is not pull-ready", m)
		}
	}
	if want := []string{"o/big", "o/no-pipeline", "o/small"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("colibri catalog = %v, want %v", ids, want)
	}
}

// TestColibriCatalogAndSearch checks the browse list asks for the colibri tag
// in full, that a query searches within the tag, and that each query is cached
// on its own.
func TestColibriCatalogAndSearch(t *testing.T) {
	var mu sync.Mutex
	var requests []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		requests = append(requests, q)
		mu.Unlock()
		rows := []hfModelRow{colibriRow("Justvugg/GLM-5.3-colibri-int4-g64", 10)}
		if s := q.Get("search"); s != "" {
			rows = []hfModelRow{colibriRow("found/"+strings.ReplaceAll(s, " ", "-"), 1)}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(context.Background(), "colibri", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Searchable || len(res.Models) != 1 || res.Models[0].Name != "Justvugg/GLM-5.3-colibri-int4-g64" {
		t.Fatalf("browse = %+v", res)
	}
	for range 2 {
		res, err = c.Catalog(context.Background(), "colibri", "", "", " qwen ")
		if err != nil {
			t.Fatal(err)
		}
	}
	if res.Query != "qwen" || len(res.Models) != 1 || res.Models[0].ID != "found/qwen" {
		t.Fatalf("search = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("%d upstream requests, want one browse and one cached search", len(requests))
	}
	for _, q := range requests {
		if q.Get("filter") != "colibri" || q.Get("full") != "true" || q.Get("config") != "true" || q.Get("sort") != "downloads" {
			t.Errorf("listing asked for %v, want colibri repos in full by downloads", q)
		}
	}
	if requests[1].Get("search") != "qwen" {
		t.Errorf("search sent %v", requests[1])
	}
}

// listedHub is a Hugging Face stand-in for the listed-model tests: the colibri
// listing (or a search of it) at the endpoint itself, and one repository's
// fields at endpoint/<owner>/<name>.
func listedHub(t *testing.T, tagged []hfModelRow, repos map[string]hfModelRow) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if repo := strings.TrimPrefix(r.URL.Path, "/"); repo != "" {
			row, ok := repos[repo]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(row)
			return
		}
		rows := tagged
		if s := r.URL.Query().Get("search"); s != "" {
			rows = nil
			for _, row := range tagged {
				if strings.Contains(strings.ToLower(row.ID), strings.ToLower(s)) {
					rows = append(rows, row)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestColibriCatalogListsManifestModelsFirst checks the models Colibri's
// manifest lists by repository lead the browse list under their manifest names
// and sizes, with the repository's live downloads and date; that a listed
// repository the tag also finds appears once; and that one since gated, or
// gone, is left out without failing the rest.
func TestColibriCatalogListsManifestModelsFirst(t *testing.T) {
	official := colibriRow("vendor/Big-Model", 7)
	official.Tags = []string{"text-generation"}
	alsoTagged := colibriRow("o/tagged-official", 3)
	gated := colibriRow("vendor/Gated", 1)
	gated.Gated = json.RawMessage(`"manual"`)
	srv := listedHub(t,
		[]hfModelRow{colibriRow("o/popular", 99), alsoTagged},
		map[string]hfModelRow{"vendor/Big-Model": official, "o/tagged-official": alsoTagged, "vendor/Gated": gated})
	c := newCatalogService()
	c.baseURL = srv.URL
	c.listed = func(engine string) []CatalogEntry {
		if engine != "colibri" {
			t.Errorf("listed asked for %q", engine)
		}
		return []CatalogEntry{
			{Repo: "vendor/Big-Model", Name: "Big Model", Parameters: "284B", SizeGB: 166.9},
			{Repo: "o/tagged-official", Name: "Tagged Official", SizeGB: 19.4},
			{Repo: "vendor/Gated", Name: "Gated"},
			{Repo: "vendor/Gone", Name: "Gone"},
		}
	}

	res, err := c.Catalog(context.Background(), "colibri", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range res.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "vendor/Big-Model,o/tagged-official,o/popular" {
		t.Fatalf("browse order = %v", ids)
	}
	big := res.Models[0]
	if big.Name != "Big Model" || big.Author != "vendor" || big.Size != 166_900_000_000 || big.ParameterSize != "284B" ||
		big.Downloads != 7 || big.UpdatedAt != official.LastModified || big.URL != "https://huggingface.co/vendor/Big-Model" {
		t.Fatalf("listed model = %+v", big)
	}
	if res.Models[1].Name != "Tagged Official" {
		t.Errorf("a tagged listed repository kept its tag-search shape: %+v", res.Models[1])
	}
}

// TestColibriSearchFindsListedModels checks a query matches listed models by
// repository id or display name, beside what the tag search returns.
func TestColibriSearchFindsListedModels(t *testing.T) {
	srv := listedHub(t,
		[]hfModelRow{colibriRow("o/kimi-colibri", 5), colibriRow("o/other", 4)},
		map[string]hfModelRow{"moonshotai/K3": colibriRow("moonshotai/K3", 2), "vendor/DeepSeek-V4": colibriRow("vendor/DeepSeek-V4", 1)})
	c := newCatalogService()
	c.baseURL = srv.URL
	c.listed = func(string) []CatalogEntry {
		return []CatalogEntry{{Repo: "moonshotai/K3", Name: "Kimi K3"}, {Repo: "vendor/DeepSeek-V4", Name: "DeepSeek V4 Flash"}}
	}

	res, err := c.Catalog(context.Background(), "colibri", "", "", "KIMI")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range res.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "moonshotai/K3,o/kimi-colibri" {
		t.Fatalf("search for kimi = %v, want the listed Kimi K3 (by name) then the tag hit", ids)
	}
}

// TestColibriCatalogWithoutListedModels checks a listed model that cannot be
// read leaves the tagged list intact.
func TestColibriCatalogWithoutListedModels(t *testing.T) {
	srv := listedHub(t, []hfModelRow{colibriRow("o/popular", 99)}, nil)
	c := newCatalogService()
	c.baseURL = srv.URL
	c.listed = func(string) []CatalogEntry { return []CatalogEntry{{Repo: "vendor/Gone", Name: "Gone"}} }

	res, err := c.Catalog(context.Background(), "colibri", "", "", "")
	if err != nil || len(res.Models) != 1 || res.Models[0].ID != "o/popular" {
		t.Fatalf("catalog = %+v, %v", res, err)
	}
}

// TestNormalizeColibriRowsKeepsOnlyRunnableModels checks a tagged repository is
// offered only when its config.json model_type is one Colibri runs, and never
// when it is built for a port of Colibri (SpeedyColibri, peng).
func TestNormalizeColibriRowsKeepsOnlyRunnableModels(t *testing.T) {
	typed := func(id, modelType string) hfModelRow {
		r := colibriRow(id, 1)
		r.Config = &hfModelConfig{ModelType: modelType}
		return r
	}
	peng := typed("o/mimo-peng", "mimo_v2")
	peng.Tags = append(peng.Tags, "peng")
	untyped := colibriRow("o/untyped", 1)
	serves := (&Manifest{ModelTypes: []string{"glm_moe_dsa", "mimo_v2"}}).servesModelType

	got := normalizeColibriRows([]hfModelRow{
		typed("o/glm", "glm_moe_dsa"), typed("o/upper", "GLM_MOE_DSA"), typed("o/hy3", "hy_v3"),
		typed("o/laguna", "laguna"), peng, untyped,
	}, serves)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "o/glm,o/upper" {
		t.Fatalf("runnable = %v, want only the glm_moe_dsa repositories", ids)
	}

	if got := normalizeColibriRows([]hfModelRow{untyped}, nil); len(got) != 1 {
		t.Errorf("with no model_types declared, an untyped repository was dropped: %v", got)
	}
}
