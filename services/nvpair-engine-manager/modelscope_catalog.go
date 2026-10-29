// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ModelScope lists files separately, so keep the page small enough to check
// every repository's offered quantization within the catalog timeout.
const modelScopeCatalogLimit = 10

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
		return nil, fmt.Errorf("ModelScope catalog request failed: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ModelScope catalog returned HTTP %d", resp.StatusCode)
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
	var wg sync.WaitGroup
	for i, entry := range result.Data.Models {
		row := entry.hfModelRow
		row.LastModified, row.CreatedAt = entry.LastModified, entry.CreatedAt
		for j, tag := range row.Tags {
			if _, value, ok := strings.Cut(tag, ":"); ok {
				row.Tags[j] = value
			}
		}
		if len(entry.Tasks) > 0 {
			row.PipelineTag = entry.Tasks[0]
		}
		if row.Private || string(row.Gated) != "false" || !llamaCPPRepoPattern.MatchString(row.ID) || !hasTag(row.Tags, "gguf") || !isGenerative(row) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
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
			rows[i] = row
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	models := normalizeLlamaCPPRows(rows)
	for i := range models {
		repo, _, _ := strings.Cut(models[i].ID, ":")
		models[i].URL = modelScopeURL + "/models/" + repo
	}
	return models, nil
}
