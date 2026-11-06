// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// The vLLM catalogue: Hugging Face repos that ship safetensors weights and
// answer chat or completion requests. The download name is the repo id itself,
// which is what vLLM serves and what the Hugging Face cache stores.
const vllmListingLimit = 50

// vllmPublishers are the Hugging Face accounts whose repos make up the browse
// list: publishers of widely used open-weight models that vLLM serves directly.
// Gated repos are dropped, so a publisher that gates most of its models
// contributes only its open ones.
var vllmPublishers = []string{"Qwen", "nvidia", "mistralai", "microsoft", "ibm-granite"}

// vllmPipelines are the pipeline tags listed. Vision-language chat models carry
// image-text-to-text and are served by vLLM like any other chat model.
var vllmPipelines = []string{"text-generation", "image-text-to-text"}

func (c *catalogService) vllmCatalog(ctx context.Context, query string) ([]CatalogModel, time.Time, error) {
	if query == "" {
		return c.vllm.get(ctx)
	}
	return c.searchCache("vllm:"+strings.ToLower(query), "vllm search", func(ctx context.Context) ([]CatalogModel, error) {
		rows, err := c.fetchVLLMListings(ctx, "search", []string{query})
		if err != nil {
			return nil, err
		}
		return normalizeVLLMRows(rows), nil
	}).get(ctx)
}

func (c *catalogService) fetchVLLM(ctx context.Context) ([]CatalogModel, error) {
	rows, err := c.fetchVLLMListings(ctx, "author", vllmPublishers)
	if err != nil {
		return nil, err
	}
	return normalizeVLLMRows(rows), nil
}

// fetchVLLMListings reads one listing per value and pipeline. Any request
// failing fails the whole fetch, so a refresh never replaces the cached list
// with one silently missing a publisher.
func (c *catalogService) fetchVLLMListings(ctx context.Context, field string, values []string) ([]hfModelRow, error) {
	type job struct{ value, pipeline string }
	var jobs []job
	for _, v := range values {
		for _, p := range vllmPipelines {
			jobs = append(jobs, job{v, p})
		}
	}
	batches := make([][]hfModelRow, len(jobs))
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batches[i], errs[i] = c.fetchHF(ctx, vllmListing(field, j.value, j.pipeline))
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var rows []hfModelRow
	for _, b := range batches {
		rows = append(rows, b...)
	}
	return rows, nil
}

// vllmListing is a listing request narrowed by one field, author or search, and
// one pipeline. expand asks for exactly the fields used, including the tensor
// parameter counts that give a model's size.
func vllmListing(field, value, pipeline string) url.Values {
	q := url.Values{}
	q.Set(field, value)
	q.Set("pipeline_tag", pipeline)
	q.Set("filter", "safetensors")
	q.Set("sort", "downloads")
	q.Set("direction", "-1")
	q.Set("limit", fmt.Sprint(vllmListingLimit))
	for _, f := range []string{"safetensors", "downloads", "likes", "lastModified", "createdAt", "tags", "gated", "private", "pipeline_tag"} {
		q.Add("expand[]", f)
	}
	return q
}

func normalizeVLLMRows(rows []hfModelRow) []CatalogModel {
	seen := make(map[string]int, len(rows))
	out := make([]CatalogModel, 0, len(rows))
	for _, r := range rows {
		model, ok := vllmModel(r)
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

// vllmModel turns one listing entry into a download, or reports it is not one.
// Gated and private repos fail to download without credentials, MLX is Apple's
// framework, and a listing can still include embedding models the chat endpoint
// cannot serve.
func vllmModel(r hfModelRow) (CatalogModel, bool) {
	repo := strings.TrimSpace(r.ID)
	if repo == "" {
		repo = strings.TrimSpace(r.ModelID)
	}
	if !llamaCPPRepoPattern.MatchString(repo) {
		return CatalogModel{}, false
	}
	if r.Private || string(bytes.TrimSpace(r.Gated)) != "false" {
		return CatalogModel{}, false
	}
	if !hasTag(r.Tags, "safetensors") || !isGenerative(r) || isMLX(repo, r.Tags) {
		return CatalogModel{}, false
	}
	updated := r.LastModified
	if updated == "" {
		updated = r.CreatedAt
	}
	if _, err := time.Parse(time.RFC3339, updated); err != nil {
		return CatalogModel{}, false
	}
	return CatalogModel{
		ID:            repo,
		Name:          repo,
		Author:        repo[:strings.Index(repo, "/")],
		URL:           "https://huggingface.co/" + repo,
		Size:          safetensorsBytes(r.Safetensors.Parameters),
		Downloads:     max(r.Downloads, 0),
		Likes:         max(r.Likes, 0),
		UpdatedAt:     updated,
		Tags:          r.Tags,
		ParameterSize: parameterLabel(r.Safetensors.Total),
	}, true
}

// tensorBytes is the storage size of a safetensors element type. A type not
// listed makes the total unknown, which is reported as no size at all.
var tensorBytes = map[string]uint64{
	"F64": 8, "F32": 4, "BF16": 2, "F16": 2, "I64": 8, "I32": 4, "I16": 2,
	"I8": 1, "U8": 1, "F8_E4M3": 1, "F8_E5M2": 1, "BOOL": 1,
}

func safetensorsBytes(parameters map[string]int64) uint64 {
	var total uint64
	for kind, count := range parameters {
		width, ok := tensorBytes[kind]
		if !ok || count < 0 {
			return 0
		}
		total += uint64(count) * width
	}
	return total
}

// parameterLabel renders a parameter count the way model names do: 600M, 8B.
func parameterLabel(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e9), ".0") + "B"
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", (n+500_000)/1_000_000)
	default:
		return ""
	}
}
