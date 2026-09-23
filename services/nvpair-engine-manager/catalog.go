// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"embed"
	"encoding/json"
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
)

// engine:catalog serves the set of models an engine can download, as opposed to
// engine:models, which reports what is already installed. It is the one model
// surface with no engine-local source: an engine's own API can list what it
// holds, but not what its upstream offers.
//
// It lives here rather than in a client so both the desktop app and the terminal
// interface see the same catalogue. Each engine has exactly one curated source,
// and the two are deliberately different in kind:
//
//   - Ollama has no public library API, only a client-rendered web page. A live
//     scrape was fragile and broke whenever the markup changed, so the list is a
//     locked file, regenerated on demand by a developer who reviews the diff.
//     It is compiled in, so serving it needs no network and cannot fail.
//   - LM Studio's catalogue *is* a Hugging Face org (`lmstudio-community`), whose
//     repo ids are exactly the strings `lms get` accepts. That has a real API, so
//     it is fetched live and cached.

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
	// the platform the model is destined for, which is not always this host.
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
	// Platform is the GOOS this list was filtered for. Echoed back so a client
	// can tell the operator which machine the list actually applies to, rather
	// than presenting a platform-filtered list as universal.
	Platform string `json:"platform,omitempty"`
}

// catalogParams is the engine:catalog request.
type catalogParams struct {
	Engine string `json:"engine"`
	// Platform is the GOOS the models will actually be installed on. It matters
	// because some quantizations are platform-locked, and the machine asking is
	// not always the machine downloading — a client driving a peer should say
	// which peer. Empty means this host, which is the common case and preserves
	// the behaviour of a caller that does not know.
	Platform string `json:"platform"`
}

//go:embed catalog/ollama-models.json
var catalogFS embed.FS

// ollamaCatalogPath is the embedded locked list.
const ollamaCatalogPath = "catalog/ollama-models.json"

// The LM Studio catalogue endpoint. Its repo ids are the pull-ready strings.
const (
	hfModelsAPI        = "https://huggingface.co/api/models"
	lmStudioAuthor     = "lmstudio-community"
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

// ollamaLibraryFile is the committed list's on-disk shape.
type ollamaLibraryFile struct {
	ScrapedAt string             `json:"scrapedAt"`
	Source    string             `json:"source"`
	Count     int                `json:"count"`
	Models    []ollamaLibraryRow `json:"models"`
}

// ollamaLibraryRow is one committed entry, shaped like an Ollama tags response.
type ollamaLibraryRow struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	ModifiedAt string `json:"modified_at"`
	Size       uint64 `json:"size"`
	Digest     string `json:"digest"`
	Details    struct {
		Family        string `json:"family"`
		ParameterSize string `json:"parameter_size"`
		Quantization  string `json:"quantization_level"`
	} `json:"details"`
}

// hfModelRow is the subset of a Hugging Face listing entry that matters here.
type hfModelRow struct {
	ID           string   `json:"id"`
	ModelID      string   `json:"modelId"`
	Downloads    int      `json:"downloads"`
	Likes        int      `json:"likes"`
	LastModified string   `json:"lastModified"`
	CreatedAt    string   `json:"createdAt"`
	Tags         []string `json:"tags"`
}

// catalogService answers engine:catalog. The embedded list is parsed once; the
// fetched list is cached with an in-flight guard so concurrent callers share one
// request rather than each starting their own.
type catalogService struct {
	client *http.Client

	ollamaOnce sync.Once
	ollama     []CatalogModel
	ollamaMeta ollamaLibraryFile
	ollamaErr  error

	mu        sync.Mutex
	lmStudio  []CatalogModel
	lmFetched time.Time
	// lmFailed is when the last fetch failed, so a dead upstream is retried on a
	// backoff rather than on every call.
	lmFailed time.Time
	inflight chan struct{}
	// baseURL overrides the listing endpoint in tests. Empty means the real one;
	// the caching and coalescing around the fetch is the part worth testing, and
	// it cannot be exercised against the live API.
	baseURL string
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
	return &catalogService{client: newEngineHTTPClient(catalogHTTPTimeout)}
}

// Catalog returns the downloadable models for an engine, filtered for the
// platform they will be installed on.
func (c *catalogService) Catalog(ctx context.Context, engine, platform string) (CatalogResult, error) {
	if platform == "" {
		platform = runtime.GOOS
	}
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
			// The committed Ollama list carries no platform-locked entries, so
			// nothing is dropped and the caller's platform does not change it.
			FetchedAt: meta.ScrapedAt,
		}, nil
	case "lmstudio":
		models, fetchedAt, err := c.lmStudioCatalog(ctx)
		if err != nil {
			return CatalogResult{}, err
		}
		return CatalogResult{
			Models:    filterForPlatform(models, platform),
			Source:    hfModelsAPI + "?author=" + lmStudioAuthor,
			Platform:  platform,
			FetchedAt: fetchedAt.UTC().Format(time.RFC3339),
		}, nil
	default:
		return CatalogResult{}, fmt.Errorf("no model catalog for engine %q", engine)
	}
}

// filterForPlatform drops models that cannot install on the target.
//
// MLX quantizations are Apple's framework and only run on Apple Silicon; `lms
// get` refuses them elsewhere. Filtering happens here, against the *target's*
// platform rather than this host's, so a client driving a peer is not offered
// models that peer can never install.
func filterForPlatform(models []CatalogModel, platform string) []CatalogModel {
	if platform == "darwin" {
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

// lmStudioCatalog serves the cached Hugging Face listing, refreshing it when
// stale. A refresh that fails keeps the previous list: a stale catalogue is far
// more useful than an empty one.
func (c *catalogService) lmStudioCatalog(ctx context.Context) ([]CatalogModel, time.Time, error) {
	c.mu.Lock()
	// Serve the cache when it is fresh, and also when a recent attempt failed:
	// re-fetching on every call against a dead upstream just made each caller
	// wait out the full timeout for the same stale answer.
	haveList := len(c.lmStudio) > 0
	fresh := haveList && time.Since(c.lmFetched) < catalogCacheTTL
	backingOff := haveList && time.Since(c.lmFailed) < catalogRetryAfterFailure
	if fresh || backingOff {
		models, at := c.lmStudio, c.lmFetched
		c.mu.Unlock()
		return models, at, nil
	}
	// Coalesce concurrent callers onto one request.
	if c.inflight != nil {
		wait := c.inflight
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, time.Time{}, ctx.Err()
		}
		c.mu.Lock()
		models, at := c.lmStudio, c.lmFetched
		c.mu.Unlock()
		if len(models) == 0 {
			return nil, time.Time{}, fmt.Errorf("lm studio catalog unavailable")
		}
		return models, at, nil
	}
	done := make(chan struct{})
	c.inflight = done
	c.mu.Unlock()

	models, err := c.fetchLmStudio(ctx)

	c.mu.Lock()
	if err == nil && len(models) > 0 {
		c.lmStudio = models
		c.lmFetched = time.Now()
		c.lmFailed = time.Time{}
	} else {
		c.lmFailed = time.Now()
	}
	served, at := c.lmStudio, c.lmFetched
	c.inflight = nil
	// Closed while still holding the lock, so clearing inflight and releasing
	// the waiters is atomic with respect to new arrivals. Closing after the
	// unlock left a window where a caller saw no in-flight request and started
	// a second one — exactly when a herd is most likely, right after a failure.
	close(done)
	c.mu.Unlock()

	if len(served) == 0 {
		if err != nil {
			return nil, time.Time{}, err
		}
		return nil, time.Time{}, fmt.Errorf("lm studio catalog returned no models")
	}
	if err != nil {
		slog.Warn("lm studio catalog refresh failed; serving the previous list",
			"models", len(served), "err", err)
	}
	return served, at, nil
}

func (c *catalogService) fetchLmStudio(ctx context.Context) ([]CatalogModel, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()

	q := url.Values{}
	q.Set("author", lmStudioAuthor)
	q.Set("sort", "downloads")
	q.Set("direction", "-1")
	q.Set("limit", fmt.Sprint(lmStudioLimit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint()+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PAIR/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model catalog returned %s", resp.Status)
	}

	var rows []hfModelRow
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBody)).Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	return normalizeHFRows(rows), nil
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
