// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// TestOllamaCatalogLoadsFromEmbeddedFile checks the committed list is compiled in
// and parses. If the embed directive or the file shape ever breaks, the catalog
// silently becomes empty, and an empty catalog looks identical to "this engine
// has nothing to offer".
func TestOllamaCatalogLoadsFromEmbeddedFile(t *testing.T) {
	c := newCatalogService()
	res, err := c.Catalog(context.Background(), "ollama", "linux", "amd64", "")
	if err != nil {
		t.Fatalf("ollama catalog: %v", err)
	}
	if len(res.Models) == 0 {
		t.Fatal("embedded ollama catalog is empty")
	}
	if res.FetchedAt == "" {
		t.Error("no scrape timestamp; an operator cannot tell how stale the list is")
	}
	if !strings.Contains(res.Source, "ollama.com") {
		t.Errorf("source = %q", res.Source)
	}

	for _, m := range res.Models {
		if m.Name == "" {
			t.Fatal("a model has no pull name")
		}
		if m.ID != m.Name {
			t.Fatalf("id %q and name %q disagree; both must be pull-ready", m.ID, m.Name)
		}
		if !strings.HasPrefix(m.URL, "https://ollama.com/library/") {
			t.Fatalf("model %q has url %q", m.Name, m.URL)
		}
		// The tag must not leak into the library URL path.
		if strings.Contains(strings.TrimPrefix(m.URL, "https://ollama.com/library/"), ":") {
			t.Fatalf("model %q url carries a tag: %q", m.Name, m.URL)
		}
	}
}

// TestOllamaCatalogIsServedFromCache checks the embedded list is parsed once
// rather than on every request: it is several thousand entries.
func TestOllamaCatalogIsServedFromCache(t *testing.T) {
	c := newCatalogService()
	first, _, err := c.ollamaCatalog()
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, _, err := c.ollamaCatalog()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if len(first) != len(second) {
		t.Fatal("repeat load produced a different list")
	}
	if len(first) > 0 && &first[0] != &second[0] {
		t.Error("catalog re-parsed instead of being reused")
	}
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
func TestOllamaCatalogFitsInAFrame(t *testing.T) {
	c := newCatalogService()
	res, err := c.Catalog(context.Background(), "ollama", "linux", "amd64", "")
	if err != nil {
		t.Fatalf("ollama catalog: %v", err)
	}
	body, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	// The real frame carries a JSON-RPC envelope around this; leave room for it.
	const envelopeAllowance = 4096
	if len(body)+envelopeAllowance > jsonrpc.WorkerFrameBytes {
		t.Errorf("marshalled ollama catalog is %d bytes, over the %d-byte frame cap; "+
			"filter or paginate engine:catalog rather than raising the cap again",
			len(body), jsonrpc.WorkerFrameBytes)
	}
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
func TestLmStudioCatalogCoalescesConcurrentCallers(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		<-release // hold the request open so the callers genuinely overlap
		_, _ = w.Write([]byte(`[{"id":"lmstudio-community/Model-GGUF","downloads":5}]`))
	}))
	defer srv.Close()

	c := newCatalogService()
	c.baseURL = srv.URL

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	counts := make([]int, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, _, err := c.lmStudio.get(context.Background())
			errs[i], counts[i] = err, len(models)
		}()
	}

	// Let the callers pile up behind the one in flight, then answer.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := requests.Load(); got != 1 {
		t.Errorf("%d callers produced %d upstream requests, want 1", callers, got)
	}
	for i := range callers {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
		}
		if counts[i] != 1 {
			t.Errorf("caller %d got %d models, want 1", i, counts[i])
		}
	}
}

// TestLmStudioCatalogBacksOffAfterFailure is the guard for a stall: only a
// success stamped the cache, so against a dead upstream every later call retried
// and waited out the full timeout before handing back the same stale list.
func TestLmStudioCatalogBacksOffAfterFailure(t *testing.T) {
	var requests atomic.Int32
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"lmstudio-community/Model-GGUF","downloads":5}]`))
	}))
	defer srv.Close()

	c := newCatalogService()
	c.baseURL = srv.URL

	// Seed a good list, then start failing.
	fail = false
	if _, _, err := c.lmStudio.get(context.Background()); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}
	fail = true

	// Force a refresh by ageing the cache past its TTL.
	c.lmStudio.mu.Lock()
	c.lmStudio.fetched = time.Now().Add(-2 * catalogCacheTTL)
	c.lmStudio.mu.Unlock()

	before := requests.Load()
	for range 3 {
		models, _, err := c.lmStudio.get(context.Background())
		if err != nil {
			t.Fatalf("a failed refresh should still serve the stale list: %v", err)
		}
		if len(models) != 1 {
			t.Errorf("stale list lost its models: %d", len(models))
		}
	}
	if got := requests.Load() - before; got != 1 {
		t.Errorf("three calls after a failure made %d requests, want 1 then backoff", got)
	}
}

// TestLmStudioCatalogBacksOffWithNothingCached is the same guard for a machine
// that has never reached the upstream. The backoff was keyed on having a list
// to serve, so with an empty cache every call started a fresh fetch and waited
// out the full timeout — the warm-up, then each modal open, then the terminal
// browser, each in turn.
func TestLmStudioCatalogBacksOffWithNothingCached(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newCatalogService()
	c.baseURL = srv.URL

	for i := range 3 {
		if _, _, err := c.lmStudio.get(context.Background()); err == nil {
			t.Fatalf("call %d: a failing upstream with nothing cached returned no error", i)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("three calls against a failing upstream made %d requests, want 1 then backoff", got)
	}

	// Once the backoff has run out, the next call tries again.
	c.lmStudio.mu.Lock()
	c.lmStudio.failed = time.Now().Add(-2 * catalogRetryAfterFailure)
	c.lmStudio.mu.Unlock()
	_, _, _ = c.lmStudio.get(context.Background())
	if got := requests.Load(); got != 2 {
		t.Errorf("after the backoff expired: %d requests, want a second attempt", got)
	}
}

func TestNormalizeCatalogEngine(t *testing.T) {
	expectNormalized := func(in, want string) {
		t.Helper()
		if got := normalizeCatalogEngine(in); got != want {
			t.Errorf("normalizeCatalogEngine(%q) = %q, want %q", in, got, want)
		}
	}
	expectNormalized("ollama", "ollama")
	expectNormalized("Ollama", "ollama")
	expectNormalized("  ollama ", "ollama")
	expectNormalized("lmstudio", "lmstudio")
	expectNormalized("LM Studio", "lmstudio")
	expectNormalized("lm-studio", "lmstudio")
	expectNormalized("llamacpp", "llamacpp")
	expectNormalized("llama-cpp", "llamacpp")
	expectNormalized("llama.cpp", "llamacpp")
	expectNormalized("vllm", "vllm")
}

// TestCatalogRejectsUnknownEngine checks an engine with no curated source errors
// rather than returning an empty list that reads as "nothing available".
func TestCatalogRejectsUnknownEngine(t *testing.T) {
	c := newCatalogService()
	if _, err := c.Catalog(context.Background(), "vllm", "linux", "amd64", ""); err == nil {
		t.Error("unknown engine returned a catalog")
	}
}

// TestNormalizeHFRowsMarksMLX checks Apple-only quantizations are labelled
// rather than dropped during normalization. `lms get` refuses them anywhere but
// Apple Silicon, but the machine that asks is not always the machine that
// installs, so the drop decision belongs to the caller's platform.
func TestNormalizeHFRowsMarksMLX(t *testing.T) {
	rows := []hfModelRow{
		{ID: "lmstudio-community/Qwen3-8B-GGUF", Downloads: 10},
		{ID: "lmstudio-community/Qwen3-8B-MLX-4bit", Downloads: 99},
		{ID: "lmstudio-community/Tagged-Model", Downloads: 50, Tags: []string{"MLX"}},
	}

	got := normalizeHFRows(rows)
	if len(got) != 3 {
		t.Fatalf("normalize kept %d models, want all three (filtering happens later)", len(got))
	}
	marked := map[string]bool{}
	for _, m := range got {
		marked[m.ID] = m.AppleOnly
	}
	if marked["lmstudio-community/Qwen3-8B-GGUF"] {
		t.Error("a GGUF model was marked Apple-only")
	}
	if !marked["lmstudio-community/Qwen3-8B-MLX-4bit"] {
		t.Error("an MLX repo id was not marked Apple-only")
	}
	if !marked["lmstudio-community/Tagged-Model"] {
		t.Error("an MLX tag was not marked Apple-only")
	}
}

// TestFilterForTarget is the guard for two versions of one bug. The list was
// first filtered by whichever machine served it, so browsing for a Mac peer
// from a Linux box hid every model that peer could use; then by operating
// system alone, so an Intel Mac was offered MLX models it can never install.
func TestFilterForTarget(t *testing.T) {
	models := []CatalogModel{
		{ID: "plain/gguf"},
		{ID: "apple/mlx", AppleOnly: true},
	}

	if got := filterForTarget(models, "darwin", "arm64"); len(got) != 2 {
		t.Errorf("Apple Silicon target got %d models, want both", len(got))
	}
	for _, target := range []struct{ platform, arch string }{
		{"darwin", "amd64"},
		{"darwin", ""},
		{"linux", "arm64"},
		{"windows", "amd64"},
	} {
		got := filterForTarget(models, target.platform, target.arch)
		if len(got) != 1 || got[0].ID != "plain/gguf" {
			t.Errorf("%s/%s target got %v, want only the portable model",
				target.platform, target.arch, got)
		}
	}
}

// TestCatalogEchoesTarget checks the reply says which machine it was filtered
// for, so a client can tell the operator rather than presenting a filtered list
// as universal.
func TestCatalogEchoesTarget(t *testing.T) {
	c := newCatalogService()
	res, err := c.Catalog(context.Background(), "ollama", "darwin", "amd64", "")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if res.Platform != "darwin" || res.Arch != "amd64" {
		t.Errorf("target = %s/%s, want the requested darwin/amd64", res.Platform, res.Arch)
	}

	// Omitting both means this host.
	res, err = c.Catalog(context.Background(), "ollama", "", "", "")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if res.Platform != runtime.GOOS || res.Arch != runtime.GOARCH {
		t.Errorf("target = %s/%s, want the local %s/%s",
			res.Platform, res.Arch, runtime.GOOS, runtime.GOARCH)
	}

	// A named platform does not borrow this host's architecture: that would
	// describe a machine the caller did not ask about.
	res, err = c.Catalog(context.Background(), "ollama", "darwin", "", "")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if res.Arch != "" {
		t.Errorf("Arch = %q for a platform named without one, want it left unknown", res.Arch)
	}
}

// TestNormalizeHFRowsSortsByDownloads checks the most-used models lead, which is
// what makes an unfiltered first page useful.
func TestNormalizeHFRowsSortsByDownloads(t *testing.T) {
	rows := []hfModelRow{
		{ID: "a/low", Downloads: 1},
		{ID: "a/high", Downloads: 100},
		{ID: "a/mid", Downloads: 50},
	}
	got := normalizeHFRows(rows)
	if got[0].ID != "a/high" || got[2].ID != "a/low" {
		t.Errorf("order = %q, %q, %q", got[0].ID, got[1].ID, got[2].ID)
	}
}

// TestNormalizeHFRowsSkipsUnusableEntries checks an entry with no id is dropped
// rather than becoming a row that cannot be pulled.
func TestNormalizeHFRowsSkipsUnusableEntries(t *testing.T) {
	rows := []hfModelRow{
		{ID: "", ModelID: ""},
		{ID: "", ModelID: "a/from-modelid"},
		{ID: "a/normal"},
	}
	got := normalizeHFRows(rows)
	if len(got) != 2 {
		t.Fatalf("kept %d rows, want 2", len(got))
	}
	for _, m := range got {
		if m.Name == "" || m.ID == "" {
			t.Error("kept a row with no pull id")
		}
		if !strings.HasPrefix(m.URL, "https://huggingface.co/") {
			t.Errorf("url = %q", m.URL)
		}
		if m.Author != "a" {
			t.Errorf("author = %q, want the id's owner", m.Author)
		}
	}
}

// TestNormalizeOllamaRowsDedupes checks a duplicated pull name yields one row.
func TestNormalizeOllamaRowsDedupes(t *testing.T) {
	rows := []ollamaLibraryRow{
		{Name: "llama3.2:latest", Size: 1},
		{Name: "llama3.2:latest", Size: 2},
		{Name: "qwen3:8b"},
		{Name: "   "},
	}
	got := normalizeOllamaRows(rows)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	for _, m := range got {
		if m.Name == "llama3.2:latest" && m.Size != 2 {
			t.Errorf("duplicate kept size %d, want the later entry's 2", m.Size)
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
func TestNormalizeLlamaCPPRowsKeepsOnlyDownloadable(t *testing.T) {
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
	if strings.Join(ids, ",") != "ggml-org/kept:Q4_K_M,a/split:Q4_K_M" {
		t.Fatalf("kept %v, want only the two downloadable repos, most downloaded first", ids)
	}

	m := got[0]
	if m.Name != m.ID {
		t.Errorf("name %q and id %q disagree; both must be the download name", m.Name, m.ID)
	}
	if m.Author != "ggml-org" || m.URL != "https://huggingface.co/ggml-org/kept" {
		t.Errorf("author %q, url %q", m.Author, m.URL)
	}
	if !hasTag(m.Tags, "Q4_K_M") {
		t.Errorf("tags %v do not name the quantization offered", m.Tags)
	}
}

// TestNormalizeLlamaCPPRowsDedupesAcrossPublishers checks a repo listed twice,
// as it is when a search and a publisher page overlap, yields one row.
func TestNormalizeLlamaCPPRowsDedupesAcrossPublishers(t *testing.T) {
	got := normalizeLlamaCPPRows([]hfModelRow{ggufRow("a/model", 1), ggufRow("a/model", 7)})
	if len(got) != 1 || got[0].Downloads != 7 {
		t.Errorf("got %+v, want one row carrying the later entry", got)
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
		if q.Get("filter") != "gguf" || q.Get("full") != "true" {
			t.Errorf("listing asked for %q, want GGUF repos in full", r.URL.RawQuery)
		}
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
		_ = json.NewEncoder(w).Encode(rows)
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
func TestLlamaCPPCatalogReadsEveryPublisher(t *testing.T) {
	srv, _ := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(context.Background(), "llama.cpp", "linux", "arm64", "")
	if err != nil {
		t.Fatalf("llama.cpp catalog: %v", err)
	}
	if len(res.Models) != len(llamaCPPPublishers) {
		t.Fatalf("got %d models, want one from each of %v", len(res.Models), llamaCPPPublishers)
	}
	for _, publisher := range llamaCPPPublishers {
		if !slices.ContainsFunc(res.Models, func(m CatalogModel) bool { return m.Author == publisher }) {
			t.Errorf("nothing from %s", publisher)
		}
	}
	if !res.Searchable || res.Query != "" {
		t.Errorf("searchable %v, query %q; want a searchable browse list", res.Searchable, res.Query)
	}
}

// TestLlamaCPPCatalogFailsWhole checks one publisher failing fails the fetch,
// rather than caching for six hours a list that is silently missing it.
func TestLlamaCPPCatalogFailsWhole(t *testing.T) {
	srv, _ := llamaCPPServer(t, "bartowski")
	c := newCatalogService()
	c.baseURL = srv.URL

	if res, err := c.Catalog(context.Background(), "llamacpp", "", "", ""); err == nil {
		t.Errorf("a failed publisher still produced a list of %d", len(res.Models))
	}
}

// TestLlamaCPPSearch checks a query reaches the upstream once, is cached
// regardless of case, and that a search matching nothing is an answer rather
// than a failure.
func TestLlamaCPPSearch(t *testing.T) {
	srv, requests := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	res, err := c.Catalog(context.Background(), "llamacpp", "", "", "  Gemma 3  ")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Query != "Gemma 3" || len(res.Models) != 1 || res.Models[0].ID != "found/Gemma-3:Q4_K_M" {
		t.Fatalf("query %q answered with %+v", res.Query, res.Models)
	}
	if _, err := c.Catalog(context.Background(), "llamacpp", "", "", "gemma 3"); err != nil {
		t.Fatalf("repeat search: %v", err)
	}
	if got := len(requests()); got != 1 {
		t.Errorf("the same query twice made %d requests, want 1", got)
	}

	res, err = c.Catalog(context.Background(), "llamacpp", "", "", "nothing")
	if err != nil {
		t.Fatalf("a search matching nothing failed: %v", err)
	}
	if len(res.Models) != 0 {
		t.Errorf("got %d models for a query that matches none", len(res.Models))
	}
}

// TestFailedSearchDoesNotRevealTheQuery is the guard for search text leaking
// into logs. net/http puts the request URL in its error, and a search's URL
// carries what the operator typed; returned as-is, it was logged here and by
// every client that relays the error.
//
// The message cannot be compared whole: it ends in the operating system's
// connection error, which names a random port. What has to hold is that the
// error carrying the URL is gone.
func TestFailedSearchDoesNotRevealTheQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // every request now fails to connect
	c := newCatalogService()
	c.baseURL = srv.URL

	_, err := c.Catalog(context.Background(), "llamacpp", "", "", "private model name")
	if err == nil {
		t.Fatal("a search against an unreachable upstream succeeded")
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Errorf("the error still carries the request URL: %v", err)
	}
}

// TestOverLongSearchIsRefused checks a query past maxCatalogQuery is an error
// that never reaches the upstream, rather than being shortened into a search
// for something the operator did not type, and that the error does not repeat
// the query.
func TestOverLongSearchIsRefused(t *testing.T) {
	srv, requests := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	query := strings.TrimSpace(strings.Repeat("private ", maxCatalogQuery/len("private ")+1))
	_, err := c.Catalog(context.Background(), "llamacpp", "", "", query)
	if err == nil {
		t.Fatalf("a %d-character query was answered", len(query))
	}
	if got, want := err.Error(), "a model search is limited to 100 characters"; got != want {
		t.Errorf("error %q, want %q", got, want)
	}
	if got := requests(); len(got) != 0 {
		t.Errorf("an over-long query reached the upstream: %v", got)
	}
}

// TestSearchAtTheLengthLimitIsAnswered checks the limit counts the query the
// operator meant: exactly maxCatalogQuery characters is searched in full, and
// surrounding space does not count against it.
func TestSearchAtTheLengthLimitIsAnswered(t *testing.T) {
	srv, requests := llamaCPPServer(t, "")
	c := newCatalogService()
	c.baseURL = srv.URL

	query := strings.Repeat("a", maxCatalogQuery)
	res, err := c.Catalog(context.Background(), "llamacpp", "", "", "  "+query+"  ")
	if err != nil {
		t.Fatalf("a query at the limit failed: %v", err)
	}
	if res.Query != query {
		t.Errorf("searched for %d characters, want all %d", len(res.Query), len(query))
	}
	if got := len(requests()); got != 1 {
		t.Errorf("a query at the limit made %d requests, want 1", got)
	}
}

// TestLlamaCPPSearchesAreBounded checks the per-query caches are evicted least
// recently used first, since the set of possible queries is unbounded.
func TestLlamaCPPSearchesAreBounded(t *testing.T) {
	c := newCatalogService()
	first := c.llamaCPPSearch("first")
	for i := range llamaCPPSearchCacheLimit {
		c.llamaCPPSearch(fmt.Sprint("query ", i))
	}
	if len(c.llamaSearches) != llamaCPPSearchCacheLimit {
		t.Errorf("%d searches kept, want at most %d", len(c.llamaSearches), llamaCPPSearchCacheLimit)
	}
	if c.llamaCPPSearch("first") == first {
		t.Error("the least recently used search was not evicted")
	}
}

// TestQueryIsIgnoredByASourceThatCannotSearch checks a query sent for an engine
// whose source cannot search returns the whole list, marked unsearchable, for
// the client to filter itself.
func TestQueryIsIgnoredByASourceThatCannotSearch(t *testing.T) {
	c := newCatalogService()
	all, err := c.Catalog(context.Background(), "ollama", "", "", "")
	if err != nil {
		t.Fatalf("ollama catalog: %v", err)
	}
	queried, err := c.Catalog(context.Background(), "ollama", "", "", "llama")
	if err != nil {
		t.Fatalf("ollama catalog with a query: %v", err)
	}
	if queried.Searchable || queried.Query != "" || len(queried.Models) != len(all.Models) {
		t.Errorf("searchable %v, query %q, %d of %d models",
			queried.Searchable, queried.Query, len(queried.Models), len(all.Models))
	}
}
