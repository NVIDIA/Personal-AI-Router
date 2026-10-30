// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// engine:catalog serves the set of models an engine can download, as opposed to
// engine:models, which reports what is already installed. It is the one model
// surface with no engine-local source: an engine's own API can list what it
// holds, but not what its upstream offers.
//
// It lives here rather than in a client so both the desktop app and the terminal
// interface see the same catalogue. Each engine has exactly one curated source,
// and they are deliberately different in kind:
//
//   - Ollama has no public library API, only a client-rendered web page. A live
//     scrape was fragile and broke whenever the markup changed, so the list is a
//     locked file, regenerated on demand by a developer who reviews the diff.
//     It is compiled in, so serving it needs no network and cannot fail.
//   - LM Studio's catalogue *is* a Hugging Face org (`lmstudio-community`), whose
//     repo ids are exactly the strings `lms get` accepts. That has a real API, so
//     it is fetched live and cached.
//   - llama.cpp downloads any GGUF repo on Hugging Face as `repo:quantization`.
//     The browse list is the GGUF repos of a few approved publishers, fetched
//     live and cached; a query searches all of Hugging Face, each query cached
//     on its own.

// catalogSourceKind distinguishes how an engine's catalogue is obtained.
type catalogSourceKind int

const (
	// catalogEmbedded is compiled into this binary.
	catalogEmbedded catalogSourceKind = iota
	// catalogFetched is retrieved over HTTP and cached.
	catalogFetched
)

// CatalogModel is one downloadable model, normalized across sources.
//
// Name is pull-ready: passing it straight to the engine's pull_model action
// works. That is the field's whole purpose, so neither source is allowed to put
// a display-only string here.
type CatalogModel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Author string `json:"author"`
	URL    string `json:"url"`
	// Size is the download size in bytes, omitted when the source does not
	// report one (Hugging Face does not expose it on the listing endpoint).
	Size      uint64   `json:"size,omitempty"`
	Downloads int      `json:"downloads"`
	Likes     int      `json:"likes"`
	UpdatedAt string   `json:"updatedAt"`
	Tags      []string `json:"tags"`
	Family    string   `json:"family,omitempty"`
	// ParameterSize is a human label such as "8B", not a number: sources report
	// it as text and it is display-only.
	ParameterSize string `json:"parameterSize,omitempty"`
	// AppleOnly marks a quantization that only installs on Apple Silicon (MLX).
	// Reported rather than silently dropped so the decision can be made against
	// the machine the model is destined for, which is not always this host.
	AppleOnly bool `json:"appleOnly,omitempty"`
}

// CatalogResult is the engine:catalog reply.
type CatalogResult struct {
	Models []CatalogModel `json:"models"`
	// Source describes where the list came from, for support and diagnostics.
	Source string `json:"source"`
	// FetchedAt is when the served list was obtained. For an embedded list this
	// is when it was scraped, which tells an operator how stale it is.
	FetchedAt string `json:"fetchedAt,omitempty"`
	// Platform and Arch are the GOOS and GOARCH this list was filtered for.
	// Echoed back so a client can tell the operator which machine the list
	// actually applies to, rather than presenting a filtered list as universal.
	// Arch is empty when the caller named a platform without one.
	Platform string `json:"platform,omitempty"`
	Arch     string `json:"arch,omitempty"`
	// Searchable reports that this engine's source answers a query, so a client
	// can offer a search of the upstream rather than only filtering this list.
	Searchable bool `json:"searchable,omitempty"`
	// Query is the search this list answers, empty for the browse list.
	Query string `json:"query,omitempty"`
}

// catalogParams is the engine:catalog request.
//
// Platform and Arch are the GOOS and GOARCH the models will actually be
// installed on. They matter because some quantizations are locked to one kind
// of machine, and the machine asking is not always the machine downloading — a
// client driving a peer should say which. Omitting both means this host, which
// is the common case.
//
// Query searches the upstream, for a source that reports Searchable. Any other
// source ignores it and returns its whole list for the client to filter.
type catalogParams struct {
	Engine   string `json:"engine"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Query    string `json:"query"`
}

//go:embed catalog/ollama-models.json
var catalogFS embed.FS

// ollamaCatalogPath is the embedded locked list.
const ollamaCatalogPath = "catalog/ollama-models.json"

// The LM Studio catalogue endpoint. Its repo ids are the pull-ready strings.
const (
	hfModelsAPI    = "https://huggingface.co/api/models"
	lmStudioAuthor = "lmstudio-community"
	// lmStudioLimit is one page of the listing, deliberately not followed by
	// more. The listing is sorted by downloads, so what falls off the end is the
	// least-used tail, and every page past the first would be another request
	// inside the same timeout on a cold cache. A model beyond it is still one
	// download-by-name away; the catalogue is for browsing, not the boundary of
	// what can be installed.
	lmStudioLimit      = 500
	catalogCacheTTL    = 6 * time.Hour
	catalogHTTPTimeout = 20 * time.Second
	// catalogRetryAfterFailure keeps a failed fetch from turning every later
	// call into a fresh 20-second attempt. Without it an unreachable upstream
	// made each caller wait out the whole timeout before being handed the stale
	// list — warm-up, a desktop modal, and the terminal browser stalling in
	// turn — because only a success stamped the cache.
	catalogRetryAfterFailure = 2 * time.Minute
	// maxCatalogBody bounds the listing response. The real payload is a few
	// hundred KiB; this refuses to buffer an arbitrarily large or compressed
	// reply into a supervised worker, and matches the ceiling this package
	// already applies to engine action reads.
	maxCatalogBody = 8 << 20
)

// The llama.cpp catalogue: GGUF repos, offered at one quantization.
const (
	// llamaCPPQuantization is the quantization every entry is offered at, and
	// the suffix that makes a repo id a llama.cpp download name. Q4_K_M is the
	// usual balance of size and quality, and nearly every GGUF repo ships it.
	llamaCPPQuantization = "Q4_K_M"
	// llamaCPPLimit is the page size for each publisher and for each search.
	llamaCPPLimit = 50
	// llamaCPPSearchCacheLimit bounds how many distinct queries are kept, oldest
	// use evicted first. Each is a short list, but the set of possible queries
	// is not bounded at all.
	llamaCPPSearchCacheLimit = 20
	// maxCatalogQuery bounds a query passed upstream, in characters. A longer
	// one is refused rather than shortened, which would search for something
	// the operator did not type.
	maxCatalogQuery = 100
)

// llamaCPPPublishers are the Hugging Face accounts whose GGUF repos make up the
// llama.cpp browse list: llama.cpp's own org and two widely used quantizers.
var llamaCPPPublishers = []string{"ggml-org", "bartowski", "unsloth"}

// ollamaLibraryFile is the committed list's on-disk shape.
type ollamaLibraryFile struct {
	ScrapedAt string             `json:"scrapedAt"`
	Source    string             `json:"source"`
	Count     int                `json:"count"`
	Models    []ollamaLibraryRow `json:"models"`
}

// ollamaLibraryRow is one committed entry, shaped like an Ollama tags response.
type ollamaLibraryRow struct {
	Name       string               `json:"name"`
	Model      string               `json:"model"`
	ModifiedAt string               `json:"modified_at"`
	Size       uint64               `json:"size"`
	Digest     string               `json:"digest"`
	Details    ollamaLibraryDetails `json:"details"`
}

// ollamaLibraryDetails is the subset of an entry's details the catalogue uses.
type ollamaLibraryDetails struct {
	Family        string `json:"family"`
	ParameterSize string `json:"parameter_size"`
	Quantization  string `json:"quantization_level"`
}

// hfModelRow is the subset of a Hugging Face listing entry that matters here.
// The last four fields are only present when the listing is asked for in full,
// which only the llama.cpp catalogue does.
type hfModelRow struct {
	ID           string   `json:"id"`
	ModelID      string   `json:"modelId"`
	Downloads    int      `json:"downloads"`
	Likes        int      `json:"likes"`
	LastModified string   `json:"lastModified"`
	CreatedAt    string   `json:"createdAt"`
	Tags         []string `json:"tags"`
	Private      bool     `json:"private"`
	// Gated is false for an open repo and a string ("auto", "manual") for one
	// that needs its terms accepted first, so it is kept raw.
	Gated       json.RawMessage `json:"gated"`
	PipelineTag string          `json:"pipeline_tag"`
	Siblings    []hfSibling     `json:"siblings"`
}

// hfSibling is one file in a Hugging Face repo.
type hfSibling struct {
	RFilename string `json:"rfilename"`
}

// catalogService answers engine:catalog. The embedded list is parsed once; each
// fetched list is a listingCache.
type catalogService struct {
	client *http.Client

	ollamaOnce sync.Once
	ollama     []CatalogModel
	ollamaMeta ollamaLibraryFile
	ollamaErr  error

	lmStudio *listingCache
	llamaCPP *listingCache

	// searchMu guards the llama.cpp searches, kept by lowercased query with
	// searchOrder from least to most recently used.
	searchMu      sync.Mutex
	llamaSearches map[string]*listingCache
	searchOrder   []string

	// baseURL overrides the listing endpoint in tests. Empty means the real one;
	// the caching and coalescing around the fetch is the part worth testing, and
	// it cannot be exercised against the live API.
	baseURL string
}

// listingCache holds one fetched catalogue. It serves from memory while the
// list is fresh, keeps the previous list through a failed refresh, retries a
// failing upstream on a backoff rather than on every call, and gives any number
// of concurrent callers one request between them.
type listingCache struct {
	// label names the catalogue in errors and logs.
	label string
	fetch func(context.Context) ([]CatalogModel, error)
	// emptyOK accepts an empty list as an answer. A search can match nothing;
	// a browse list that comes back empty has failed.
	emptyOK bool

	mu      sync.Mutex
	models  []CatalogModel
	fetched time.Time
	// failed is when the last fetch failed, so a dead upstream is retried on a
	// backoff rather than on every call.
	failed   time.Time
	inflight chan struct{}
}

// endpoint is the listing URL to fetch.
func (c *catalogService) endpoint() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return hfModelsAPI
}

func newCatalogService() *catalogService {
	// The same redirect policy the rest of this package's HTTP uses: a catalogue
	// endpoint has no reason to redirect, and following one blindly would let an
	// upstream (or a captive portal) move the request somewhere else.
	c := &catalogService{
		client:        newEngineHTTPClient(catalogHTTPTimeout),
		llamaSearches: map[string]*listingCache{},
	}
	c.lmStudio = &listingCache{label: "lm studio catalog", fetch: c.fetchLmStudio}
	c.llamaCPP = &listingCache{label: "llama.cpp catalog", fetch: c.fetchLlamaCPP}
	return c
}

// Catalog returns the downloadable models for an engine, filtered for the
// machine they will be installed on, or the results of a query for a source
// that can search.
func (c *catalogService) Catalog(ctx context.Context, engine, platform, arch, query string) (CatalogResult, error) {
	platform, arch = catalogTarget(platform, arch)
	switch normalizeCatalogEngine(engine) {
	case "ollama":
		models, meta, err := c.ollamaCatalog()
		if err != nil {
			return CatalogResult{}, err
		}
		return CatalogResult{
			Models:   models,
			Source:   meta.Source,
			Platform: platform,
			Arch:     arch,
			// The committed Ollama list carries no machine-locked entries, so
			// nothing is dropped and the caller's target does not change it.
			FetchedAt: meta.ScrapedAt,
		}, nil
	case "lmstudio":
		models, fetchedAt, err := c.lmStudio.get(ctx)
		if err != nil {
			return CatalogResult{}, err
		}
		return CatalogResult{
			Models:    filterForTarget(models, platform, arch),
			Source:    hfModelsAPI + "?author=" + lmStudioAuthor,
			Platform:  platform,
			Arch:      arch,
			FetchedAt: fetchedAt.UTC().Format(time.RFC3339),
		}, nil
	case "llamacpp":
		query, err := catalogQuery(query)
		if err != nil {
			return CatalogResult{}, err
		}
		models, fetchedAt, err := c.llamaCPPCatalog(ctx, query)
		if err != nil {
			return CatalogResult{}, err
		}
		source := hfModelsAPI + "?filter=gguf&author=" + strings.Join(llamaCPPPublishers, ",")
		if query != "" {
			source = hfModelsAPI + "?filter=gguf&search=" + url.QueryEscape(query)
		}
		return CatalogResult{
			// GGUF has no machine-locked quantizations, so nothing is filtered
			// for the target.
			Models:     models,
			Source:     source,
			Platform:   platform,
			Arch:       arch,
			FetchedAt:  fetchedAt.UTC().Format(time.RFC3339),
			Searchable: true,
			Query:      query,
		}, nil
	default:
		return CatalogResult{}, fmt.Errorf("no model catalog for engine %q", engine)
	}
}

// catalogQuery trims a query and refuses one over maxCatalogQuery characters.
// The error does not repeat the query; search text stays out of every log.
func catalogQuery(query string) (string, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) > maxCatalogQuery {
		return "", fmt.Errorf("a model search is limited to %d characters", maxCatalogQuery)
	}
	return query, nil
}

// catalogTarget resolves the machine a catalogue is filtered for.
//
// Omitting both means this host. A caller that names a platform but not an
// architecture is describing some other machine, so this host's architecture is
// not borrowed to fill the gap: the architecture stays unknown, and anything
// that depends on it is left out rather than guessed at.
func catalogTarget(platform, arch string) (string, string) {
	if platform == "" && arch == "" {
		return runtime.GOOS, runtime.GOARCH
	}
	return platform, arch
}

// filterForTarget drops models that cannot install on the target.
//
// MLX is Apple's framework and only runs on Apple Silicon; `lms get` refuses it
// anywhere else, an Intel Mac included, so the operating system alone does not
// decide it. Filtering happens here, against the *target* rather than this
// host, so a client driving a peer is not offered models that peer can never
// install.
func filterForTarget(models []CatalogModel, platform, arch string) []CatalogModel {
	if platform == "darwin" && arch == "arm64" {
		return models
	}
	out := make([]CatalogModel, 0, len(models))
	for _, m := range models {
		if m.AppleOnly {
			continue
		}
		out = append(out, m)
	}
	return out
}

// normalizeCatalogEngine tolerates the spellings a client might send.
func normalizeCatalogEngine(engine string) string {
	e := strings.ToLower(strings.TrimSpace(engine))
	switch e {
	case "lm-studio", "lm studio", "lmstudio":
		return "lmstudio"
	case "llama-cpp", "llama.cpp", "llamacpp":
		return "llamacpp"
	default:
		return e
	}
}

// ollamaCatalog parses the embedded list once and serves it thereafter.
func (c *catalogService) ollamaCatalog() ([]CatalogModel, ollamaLibraryFile, error) {
	c.ollamaOnce.Do(func() {
		raw, err := catalogFS.ReadFile(ollamaCatalogPath)
		if err != nil {
			c.ollamaErr = fmt.Errorf("read embedded ollama catalog: %w", err)
			return
		}
		var file ollamaLibraryFile
		if err := json.Unmarshal(raw, &file); err != nil {
			c.ollamaErr = fmt.Errorf("parse embedded ollama catalog: %w", err)
			return
		}
		c.ollamaMeta = file
		c.ollama = normalizeOllamaRows(file.Models)
		slog.Info("ollama model catalog loaded",
			"models", len(c.ollama), "scrapedAt", file.ScrapedAt)
	})
	return c.ollama, c.ollamaMeta, c.ollamaErr
}

// normalizeOllamaRows converts committed rows into the shared shape, dropping
// malformed entries and de-duplicating by pull name.
func normalizeOllamaRows(rows []ollamaLibraryRow) []CatalogModel {
	seen := make(map[string]int, len(rows))
	out := make([]CatalogModel, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			name = strings.TrimSpace(r.Model)
		}
		if name == "" {
			continue
		}
		// The library page is the only URL an Ollama model has; the tag suffix
		// is not part of the path.
		base := name
		if i := strings.Index(base, ":"); i > 0 {
			base = base[:i]
		}
		model := CatalogModel{
			ID:            name,
			Name:          name,
			Author:        "ollama",
			URL:           "https://ollama.com/library/" + base,
			Size:          r.Size,
			UpdatedAt:     r.ModifiedAt,
			Tags:          catalogTags(r.Details.Family, r.Details.Quantization),
			Family:        r.Details.Family,
			ParameterSize: r.Details.ParameterSize,
		}
		if idx, dup := seen[name]; dup {
			out[idx] = model
			continue
		}
		seen[name] = len(out)
		out = append(out, model)
	}
	return out
}

// catalogTags builds a small tag set from the fields the committed list carries.
func catalogTags(values ...string) []string {
	tags := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			tags = append(tags, v)
		}
	}
	return tags
}

// get serves the cached list, refreshing it when stale. A refresh that fails
// keeps the previous list: a stale catalogue is far more useful than an empty
// one.
func (l *listingCache) get(ctx context.Context) ([]CatalogModel, time.Time, error) {
	l.mu.Lock()
	// Serve the cache when it is fresh, and also when a recent attempt failed:
	// re-fetching on every call against a dead upstream just made each caller
	// wait out the full timeout for the same stale answer.
	haveList := !l.fetched.IsZero()
	fresh := haveList && time.Since(l.fetched) < catalogCacheTTL
	backingOff := !l.failed.IsZero() && time.Since(l.failed) < catalogRetryAfterFailure
	if fresh || (haveList && backingOff) {
		models, at := l.models, l.fetched
		l.mu.Unlock()
		return models, at, nil
	}
	// The backoff holds with nothing cached too. That is the likelier case for
	// it — a machine that has never reached Hugging Face — and without it every
	// caller in turn waited out the full timeout to learn the same thing.
	if backingOff {
		retry := catalogRetryAfterFailure - time.Since(l.failed)
		l.mu.Unlock()
		return nil, time.Time{}, fmt.Errorf("%s unavailable; retrying in %s",
			l.label, retry.Round(time.Second))
	}
	// Coalesce concurrent callers onto one request.
	if l.inflight != nil {
		wait := l.inflight
		l.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, time.Time{}, ctx.Err()
		}
		l.mu.Lock()
		models, at := l.models, l.fetched
		l.mu.Unlock()
		if at.IsZero() {
			return nil, time.Time{}, fmt.Errorf("%s unavailable", l.label)
		}
		return models, at, nil
	}
	done := make(chan struct{})
	l.inflight = done
	l.mu.Unlock()

	models, err := l.fetch(ctx)
	if err == nil && len(models) == 0 && !l.emptyOK {
		err = fmt.Errorf("%s returned no models", l.label)
	}

	l.mu.Lock()
	if err == nil {
		l.models, l.fetched, l.failed = models, time.Now(), time.Time{}
	} else {
		l.failed = time.Now()
	}
	served, at := l.models, l.fetched
	l.inflight = nil
	// Closed while still holding the lock, so clearing inflight and releasing
	// the waiters is atomic with respect to new arrivals. Closing after the
	// unlock left a window where a caller saw no in-flight request and started
	// a second one — exactly when a herd is most likely, right after a failure.
	close(done)
	l.mu.Unlock()

	if at.IsZero() {
		return nil, time.Time{}, err
	}
	if err != nil {
		slog.Warn("model catalog refresh failed; serving the previous list",
			"catalog", l.label, "models", len(served), "err", err)
	}
	return served, at, nil
}

func (c *catalogService) fetchLmStudio(ctx context.Context) ([]CatalogModel, error) {
	q := url.Values{}
	q.Set("author", lmStudioAuthor)
	q.Set("sort", "downloads")
	q.Set("direction", "-1")
	q.Set("limit", fmt.Sprint(lmStudioLimit))
	rows, err := c.fetchHF(ctx, q)
	if err != nil {
		return nil, err
	}
	return normalizeHFRows(rows), nil
}

// llamaCPPCatalog serves the browse list, or the results of a query.
func (c *catalogService) llamaCPPCatalog(ctx context.Context, query string) ([]CatalogModel, time.Time, error) {
	if query == "" {
		return c.llamaCPP.get(ctx)
	}
	return c.llamaCPPSearch(query).get(ctx)
}

// llamaCPPSearch is the cache for one query, created on first use. Queries
// differing only in case share one.
func (c *catalogService) llamaCPPSearch(query string) *listingCache {
	key := strings.ToLower(query)
	c.searchMu.Lock()
	defer c.searchMu.Unlock()
	search, ok := c.llamaSearches[key]
	if !ok {
		search = &listingCache{
			label:   "llama.cpp search",
			emptyOK: true,
			fetch: func(ctx context.Context) ([]CatalogModel, error) {
				rows, err := c.fetchHF(ctx, llamaCPPListing("search", query))
				if err != nil {
					return nil, err
				}
				return normalizeLlamaCPPRows(rows), nil
			},
		}
		c.llamaSearches[key] = search
	}
	for i, k := range c.searchOrder {
		if k == key {
			c.searchOrder = append(c.searchOrder[:i], c.searchOrder[i+1:]...)
			break
		}
	}
	c.searchOrder = append(c.searchOrder, key)
	for len(c.searchOrder) > llamaCPPSearchCacheLimit {
		delete(c.llamaSearches, c.searchOrder[0])
		c.searchOrder = c.searchOrder[1:]
	}
	return search
}

// fetchLlamaCPP reads every approved publisher's GGUF repos. Any publisher
// failing fails the whole fetch, so a refresh never replaces the cached list
// with one that is silently missing a publisher.
func (c *catalogService) fetchLlamaCPP(ctx context.Context) ([]CatalogModel, error) {
	batches := make([][]hfModelRow, len(llamaCPPPublishers))
	errs := make([]error, len(llamaCPPPublishers))
	var wg sync.WaitGroup
	for i, publisher := range llamaCPPPublishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batches[i], errs[i] = c.fetchHF(ctx, llamaCPPListing("author", publisher))
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var rows []hfModelRow
	for _, batch := range batches {
		rows = append(rows, batch...)
	}
	return normalizeLlamaCPPRows(rows), nil
}

// withoutURL drops the request URL a net/http error carries, keeping the cause.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// llamaCPPListing is a GGUF listing request narrowed by one field, author or
// search. It asks for the full entry, whose file list is what shows whether a
// repo has the quantization offered.
func llamaCPPListing(field, value string) url.Values {
	q := url.Values{}
	q.Set(field, value)
	q.Set("filter", "gguf")
	q.Set("sort", "downloads")
	q.Set("direction", "-1")
	q.Set("limit", fmt.Sprint(llamaCPPLimit))
	q.Set("full", "true")
	return q
}

// fetchHF reads one page of the Hugging Face model listing.
//
// A failed request is reported without its URL. net/http puts the whole URL in
// the error, and for a search that includes what the operator typed, which
// would then be logged here and by every client the error is relayed to.
func (c *catalogService) fetchHF(ctx context.Context, q url.Values) ([]hfModelRow, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint()+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build model catalog request: %w", withoutURL(err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PAIR/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model catalog request failed: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model catalog returned %s", resp.Status)
	}

	var rows []hfModelRow
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBody)).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	return rows, nil
}

// mlxPattern matches an MLX marker as a whole token in a repo id.
var mlxPattern = regexp.MustCompile(`(?i)(?:^|[-_/])mlx(?:[-_/]|$)`)

// normalizeHFRows converts listing entries into the shared shape.
//
// MLX quantizations are Apple's framework and only run on Apple Silicon; `lms
// get` refuses them elsewhere with "no download options available". Listing them
// off-Mac would offer models that can never install, so they are dropped there.
func normalizeHFRows(rows []hfModelRow) []CatalogModel {
	seen := make(map[string]int, len(rows))
	out := make([]CatalogModel, 0, len(rows))
	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			id = strings.TrimSpace(r.ModelID)
		}
		if id == "" {
			continue
		}
		author := lmStudioAuthor
		if i := strings.Index(id, "/"); i > 0 {
			author = id[:i]
		}
		updated := r.LastModified
		if updated == "" {
			updated = r.CreatedAt
		}
		model := CatalogModel{
			ID:        id,
			Name:      id,
			Author:    author,
			URL:       "https://huggingface.co/" + id,
			Downloads: r.Downloads,
			Likes:     r.Likes,
			UpdatedAt: updated,
			Tags:      r.Tags,
			AppleOnly: isMLX(id, r.Tags),
		}
		if idx, dup := seen[id]; dup {
			out[idx] = model
			continue
		}
		seen[id] = len(out)
		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Downloads > out[j].Downloads })
	return out
}

// isMLX reports an Apple-only quantization, by tag or by repo-id token.
func isMLX(id string, tags []string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), "mlx") {
			return true
		}
	}
	return mlxPattern.MatchString(id)
}

// The shapes a llama.cpp download depends on.
var (
	// llamaCPPRepoPattern is a repo id safe to pass to llama.cpp: owner/name,
	// each starting with a letter or digit, nothing that could read as a path
	// or a flag.
	llamaCPPRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// llamaCPPQuantFile matches the offered quantization in a GGUF file name.
	llamaCPPQuantFile = regexp.MustCompile(`(?i)` + llamaCPPQuantization + `[.-]`)
	// splitGGUFPattern matches a shard of a model split across files.
	splitGGUFPattern = regexp.MustCompile(`(?i)-([0-9]{5})-of-([0-9]{5})\.gguf$`)
)

// generativePipelines are the Hugging Face pipeline tags of a model that
// answers a chat or completion request.
var generativePipelines = map[string]bool{
	"text-generation":    true,
	"image-text-to-text": true,
	"any-to-any":         true,
}

// normalizeLlamaCPPRows converts listing entries into llama.cpp downloads,
// dropping the ones llama.cpp cannot fetch or serve, de-duplicating across
// publishers, and putting the most-downloaded first.
func normalizeLlamaCPPRows(rows []hfModelRow) []CatalogModel {
	seen := make(map[string]int, len(rows))
	out := make([]CatalogModel, 0, len(rows))
	for _, r := range rows {
		model, ok := llamaCPPModel(r)
		if !ok {
			continue
		}
		if idx, dup := seen[model.ID]; dup {
			out[idx] = model
			continue
		}
		seen[model.ID] = len(out)
		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Downloads > out[j].Downloads })
	return out
}

// llamaCPPModel turns one listing entry into a download, or reports that it is
// not one. The listing's own GGUF filter is not enough on its own: it also
// matches gated and private repos, which fail to download without credentials;
// embedding and reranking models, which the chat endpoint cannot serve; and
// repos with no file at the quantization offered, which fail once the download
// has begun.
func llamaCPPModel(r hfModelRow) (CatalogModel, bool) {
	repo := strings.TrimSpace(r.ID)
	if repo == "" {
		repo = strings.TrimSpace(r.ModelID)
	}
	if !llamaCPPRepoPattern.MatchString(repo) {
		return CatalogModel{}, false
	}
	// Only an explicit false is open. A gated repo reports how it is gated, and
	// one the listing says nothing about cannot be assumed open.
	if r.Private || string(bytes.TrimSpace(r.Gated)) != "false" {
		return CatalogModel{}, false
	}
	if !hasTag(r.Tags, "gguf") || !isGenerative(r) || !hasOfferedQuant(r.Siblings) {
		return CatalogModel{}, false
	}
	updated := r.LastModified
	if updated == "" {
		updated = r.CreatedAt
	}
	if _, err := time.Parse(time.RFC3339, updated); err != nil {
		return CatalogModel{}, false
	}
	id := repo + ":" + llamaCPPQuantization
	tags := r.Tags
	if !hasTag(tags, llamaCPPQuantization) {
		tags = append(append([]string(nil), tags...), llamaCPPQuantization)
	}
	return CatalogModel{
		ID:        id,
		Name:      id,
		Author:    repo[:strings.Index(repo, "/")],
		URL:       "https://huggingface.co/" + repo,
		Downloads: max(r.Downloads, 0),
		Likes:     max(r.Likes, 0),
		UpdatedAt: updated,
		Tags:      tags,
	}, true
}

// hasTag reports a tag, ignoring case.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// isGenerative reports a model that answers chat or completion requests, by
// pipeline or, for a repo that declares none, by tag. A declared pipeline is
// authoritative: an embedding model can still carry a conversational tag.
func isGenerative(r hfModelRow) bool {
	pipeline := strings.ToLower(strings.TrimSpace(r.PipelineTag))
	if pipeline != "" {
		return generativePipelines[pipeline]
	}
	return hasTag(r.Tags, "text-generation") || hasTag(r.Tags, "conversational")
}

// hasOfferedQuant reports a repo with a file llama.cpp would download for the
// offered quantization.
func hasOfferedQuant(files []hfSibling) bool {
	for _, f := range files {
		if isOfferedQuantFile(f.RFilename) {
			return true
		}
	}
	return false
}

// llamaCPPAuxiliaryGGUF names the GGUF files llama.cpp's own downloader will
// not take as the model (gguf_filename_is_model in common/download.cpp). It
// has to match the release manifests/llamacpp.json installs, or the catalog
// offers a repo whose only matching file llama.cpp refuses to pull.
var llamaCPPAuxiliaryGGUF = []string{"mmproj", "imatrix", "mtp-", "eagle3-", "dflash-", "dspark-"}

// isOfferedQuantFile reports a model file at the offered quantization: a GGUF
// that is the model itself rather than a vision projector, an importance
// matrix, or a speculative-decoding head, and, for a model split across files,
// the first shard.
func isOfferedQuantFile(path string) bool {
	lower := strings.ToLower(path)
	if !strings.HasSuffix(lower, ".gguf") {
		return false
	}
	file := lower[strings.LastIndex(lower, "/")+1:]
	for _, auxiliary := range llamaCPPAuxiliaryGGUF {
		if strings.Contains(file, auxiliary) {
			return false
		}
	}
	if !llamaCPPQuantFile.MatchString(path) {
		return false
	}
	shard := splitGGUFPattern.FindStringSubmatch(path)
	return shard == nil || shard[1] == "00001"
}
