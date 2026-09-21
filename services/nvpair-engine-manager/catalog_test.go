// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
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
	res, err := c.Catalog(context.Background(), "ollama", "linux")
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
	res, err := c.Catalog(context.Background(), "ollama", "linux")
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
			models, _, err := c.lmStudioCatalog(context.Background())
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
	if _, _, err := c.lmStudioCatalog(context.Background()); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}
	fail = true

	// Force a refresh by ageing the cache past its TTL.
	c.mu.Lock()
	c.lmFetched = time.Now().Add(-2 * catalogCacheTTL)
	c.mu.Unlock()

	before := requests.Load()
	for range 3 {
		models, _, err := c.lmStudioCatalog(context.Background())
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

func TestNormalizeCatalogEngine(t *testing.T) {
	cases := map[string]string{
		"ollama":    "ollama",
		"Ollama":    "ollama",
		"  ollama ": "ollama",
		"lmstudio":  "lmstudio",
		"LM Studio": "lmstudio",
		"lm-studio": "lmstudio",
		"vllm":      "vllm",
	}
	for in, want := range cases {
		if got := normalizeCatalogEngine(in); got != want {
			t.Errorf("normalizeCatalogEngine(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCatalogRejectsUnknownEngine checks an engine with no curated source errors
// rather than returning an empty list that reads as "nothing available".
func TestCatalogRejectsUnknownEngine(t *testing.T) {
	c := newCatalogService()
	if _, err := c.Catalog(context.Background(), "vllm", "linux"); err == nil {
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

// TestFilterForPlatform is the guard for the bug this replaced: the list was
// filtered by whichever machine served it, so browsing for a Mac peer from a
// Linux box hid every model that peer could actually use.
func TestFilterForPlatform(t *testing.T) {
	models := []CatalogModel{
		{ID: "plain/gguf"},
		{ID: "apple/mlx", AppleOnly: true},
	}

	if got := filterForPlatform(models, "darwin"); len(got) != 2 {
		t.Errorf("darwin target got %d models, want both", len(got))
	}
	for _, target := range []string{"linux", "windows"} {
		got := filterForPlatform(models, target)
		if len(got) != 1 || got[0].ID != "plain/gguf" {
			t.Errorf("%s target got %v, want only the portable model", target, got)
		}
	}
}

// TestCatalogEchoesTargetPlatform checks the reply says which platform it was
// filtered for, so a client can tell the operator rather than presenting a
// filtered list as universal.
func TestCatalogEchoesTargetPlatform(t *testing.T) {
	c := newCatalogService()
	res, err := c.Catalog(context.Background(), "ollama", "darwin")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if res.Platform != "darwin" {
		t.Errorf("Platform = %q, want the requested target", res.Platform)
	}

	// An omitted platform means this host.
	res, err = c.Catalog(context.Background(), "ollama", "")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if res.Platform != runtime.GOOS {
		t.Errorf("Platform = %q, want the local %q", res.Platform, runtime.GOOS)
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
