// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// modelScopeCatalogLimit is one page of the listing, and the largest page
	// ModelScope serves. Every repository on it costs its own file-list request
	// to check the offered quantization, all inside the catalog timeout.
	modelScopeCatalogLimit = 50
	// modelScopeFileListConcurrency bounds the file-list requests in flight, so
	// a full page fits the timeout without opening one connection per entry.
	modelScopeFileListConcurrency = 8
	// modelScopeFileListFailureRate fails the fetch when more than this share
	// of the file lists fails. A few broken repositories are skipped; more
	// than that is an upstream problem, and a list cut short by it would be
	// cached as the catalogue for catalogCacheTTL.
	modelScopeFileListFailureRate = 0.25
)

// modelScopeNonChatMarkers are tags and repo-name words of models that do not
// answer chat requests. They override a declared task, which ModelScope sets
// to text-generation on some embedding and speech repos. ModelScope also
// leaves the task empty for many chat GGUF repos, so a repository without one
// is taken as chat unless a marker says otherwise. A blocklist cannot
// name every non-chat model; it covers the families seen on ModelScope.
var modelScopeNonChatMarkers = map[string]bool{
	"embed": true, "embedding": true, "embeddings": true, "feature-extraction": true, "sentence-similarity": true,
	"sentence-embedding": true, "sentence-transformers": true, "text-embeddings-inference": true,
	"rerank": true, "reranker": true, "reranking": true, "text-ranking": true,
	"audio": true, "speech": true, "tts": true, "asr": true, "whisper": true, "voice-cloning": true,
	"text-to-speech": true, "speech-to-speech": true, "speech-to-text": true, "text-to-audio": true, "audio-to-audio": true,
	"automatic-speech-recognition": true, "speech-recognition": true,
	"text-to-image": true, "text-to-image-synthesis": true, "image-to-image": true, "image-generation": true,
	"text-to-video": true, "image-to-video": true, "video-generation": true,
	"diffusion": true, "stable-diffusion": true, "stable-diffusion.cpp": true, "sdxl": true, "sd3": true, "flux": true, "vae": true,
	"comfy": true, "comfyui": true, "wan": true, "hunyuanvideo": true, "ltx": true, "ltxv": true,
}

// modelScopeMarkedNonChat reports a repository with a non-chat marker in its
// tags or its name. A name is matched by word, and by word without a trailing
// version number so wan2 matches wan, and a multi-word marker is matched as
// consecutive words.
func modelScopeMarkedNonChat(repo string, tags []string) bool {
	for _, tag := range tags {
		if modelScopeNonChatMarkers[strings.ToLower(strings.TrimSpace(tag))] {
			return true
		}
	}
	words := strings.NewReplacer("_", "-", ".", "-")
	_, name, _ := strings.Cut(strings.ToLower(repo), "/")
	name = words.Replace(name)
	for _, word := range strings.Split(name, "-") {
		if modelScopeNonChatMarkers[word] || modelScopeNonChatMarkers[strings.TrimRight(word, "0123456789")] {
			return true
		}
	}
	for marker := range modelScopeNonChatMarkers {
		if phrase := words.Replace(marker); strings.Contains(phrase, "-") && strings.Contains("-"+name+"-", "-"+phrase+"-") {
			return true
		}
	}
	return false
}

func (c *catalogService) fetchModelScope(ctx context.Context, query string) ([]CatalogModel, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()
	endpoint := c.modelScopeEndpoint
	if endpoint == "" {
		endpoint = modelScopeURL
	}
	q := url.Values{
		"search": {query}, "sort": {"downloads"}, "page_number": {"1"},
		"page_size": {fmt.Sprint(modelScopeCatalogLimit)}, "filter.library": {"gguf"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/openapi/v1/models?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build ModelScope catalog request: %w", withoutURL(err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PAIR/1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		cause := withoutURL(err)
		return nil, &catalogRequestError{message: fmt.Sprintf("ModelScope catalog request failed: %v", cause), cause: cause}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &catalogRequestError{
			message: fmt.Sprintf("ModelScope catalog returned HTTP %d", resp.StatusCode), status: resp.StatusCode,
			retryAt: catalogRetryTime(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Models []struct {
				hfModelRow
				Tasks        []string `json:"tasks"`
				LastModified string   `json:"last_modified"`
				CreatedAt    string   `json:"created_at"`
			} `json:"models"`
		} `json:"data"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBody+1))
	if err != nil || len(data) > maxCatalogBody || json.Unmarshal(data, &result) != nil || !result.Success || result.Data.Models == nil || len(result.Data.Models) > modelScopeCatalogLimit {
		return nil, fmt.Errorf("invalid ModelScope catalog response")
	}
	rows := make([]hfModelRow, len(result.Data.Models))
	errs := make([]error, len(rows))
	listed := make([]bool, len(rows))
	slots := make(chan struct{}, modelScopeFileListConcurrency)
	var wg sync.WaitGroup
	for i, entry := range result.Data.Models {
		row := entry.hfModelRow
		row.LastModified, row.CreatedAt = entry.LastModified, entry.CreatedAt
		for j, tag := range row.Tags {
			if _, value, ok := strings.Cut(tag, ":"); ok {
				row.Tags[j] = value
			}
		}
		if modelScopeMarkedNonChat(row.ID, row.Tags) {
			continue
		}
		row.PipelineTag = "text-generation"
		if len(entry.Tasks) > 0 {
			row.PipelineTag = entry.Tasks[0]
		}
		if row.Private || string(row.Gated) != "false" || !hfRepoPattern.MatchString(row.ID) || !hasTag(row.Tags, "gguf") || !isGenerative(row) {
			continue
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			files, err := fetchModelScopeFiles(ctx, c.client, endpoint, row.ID, "master")
			if errors.Is(err, errModelScopeListingTooLarge) {
				return
			}
			if err != nil {
				errs[i] = err
				return
			}
			for _, file := range files {
				row.Siblings = append(row.Siblings, hfSibling{RFilename: file.Path})
			}
			rows[i], listed[i] = row, true
		})
	}
	wg.Wait()
	failed, succeeded := 0, 0
	var listingErr error
	now := time.Now()
	for i, err := range errs {
		switch {
		case err != nil:
			failed++
			if listingErr == nil || catalogRetryDelay(err, now) > catalogRetryDelay(listingErr, now) {
				listingErr = err
			}
		case listed[i]:
			succeeded++
		}
	}
	models := normalizeListing(rows, llamaCPPModel)
	for i := range models {
		repo, _, _ := strings.Cut(models[i].ID, ":")
		models[i].URL = modelScopeURL + "/models/" + repo
	}
	if failed == 0 {
		return models, nil
	}
	// An ended request is what failed the fetch, whichever listing happened
	// to fail first.
	ended := ctx.Err()
	if ended != nil {
		listingErr = errors.Join(ended, listingErr)
	}
	// One summary rather than every listing's error: a timeout fails them all
	// alike, and the terminal shows the message as is.
	summary := fmt.Errorf("%d of %d ModelScope file lists failed: %w", failed, failed+succeeded, listingErr)
	switch {
	case len(models) == 0:
		// A failed listing may have held the only model the filters would
		// keep, so an empty answer here is not a real one.
		return nil, summary
	case ended != nil:
		return models, fmt.Errorf("%w: %w", errCatalogPartial, summary)
	case float64(failed) > modelScopeFileListFailureRate*float64(failed+succeeded):
		return nil, summary
	}
	// Repository ids are left out: an exact-name search would log the
	// operator's query.
	slog.Warn("skipping ModelScope repositories whose file list is unavailable", "err", summary)
	return models, nil
}
