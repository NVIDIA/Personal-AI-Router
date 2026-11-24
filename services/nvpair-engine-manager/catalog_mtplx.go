// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// The MTPLX catalogue: the pack list MTPLX publishes for Apple Silicon.
//
// This is the one catalogue here that is not a search of Hugging Face, and the
// reason is what MTPLX will actually load. It runs a pack rebuilt for its own MTP
// decode path, not arbitrary safetensors: a repo whose weights were written for
// llama.cpp or vLLM fails to load rather than falling back to a slower path. So a
// general Hugging Face browse would offer mostly packs this engine cannot serve,
// and the curated list is the only place its runnable packs are named.
//
// The download name is the pack's Hugging Face repo id, which is also what
// `mtplx models` reports and what the engine serves, so listing, pulling and
// loading all key off the same string.
const mtplxCatalogURL = "https://mtplx.com/releases/models.json"

// mtplxCatalogFile is the whole published list. Its `models` entry is a map
// keyed by repo id, unlike every other source here, which returns an array. The
// value carries a revision pin, a minimum engine version and a note; none of them
// decide what is offered, so the payload is kept unread. The revision is not
// needed because the download resolves the pack by id, and the minimum engine
// version is not filtered on: a pack newer than the installed engine fails when
// it is loaded, which names the cause, rather than vanishing from the list
// without explanation. A fourth key, `not_yet_published`, is not decoded at all
// because those packs cannot be pulled yet, so listing them would offer downloads
// that fail.
type mtplxCatalogFile struct {
	Schema      int                        `json:"schema"`
	GeneratedAt string                     `json:"generated_at"`
	Models      map[string]json.RawMessage `json:"models"`
}

// mtplxCatalogSource is the list to fetch. catalogService.mtplxURL overrides it
// in tests, where there is no live publisher to read.
func (c *catalogService) mtplxCatalogSource() string {
	if c.mtplxURL != "" {
		return c.mtplxURL
	}
	return mtplxCatalogURL
}

// fetchMTPLX reads the published pack list.
//
// It is not searched: the list is a couple dozen entries, so a query is answered
// from it by the client rather than by a fresh fetch per keystroke, and
// Searchable stays false in the reply.
func (c *catalogService) fetchMTPLX(ctx context.Context) ([]CatalogModel, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.mtplxCatalogSource(), nil)
	if err != nil {
		return nil, fmt.Errorf("build mtplx catalog request: %w", withoutURL(err))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PAIR/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mtplx catalog request failed: %w", withoutURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mtplx catalog returned %s", resp.Status)
	}

	var file mtplxCatalogFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBody)).Decode(&file); err != nil {
		return nil, fmt.Errorf("decode mtplx catalog: %w", err)
	}
	return normalizeMTPLXPacks(file.Models), nil
}

// normalizeMTPLXPacks turns the published map into catalog entries.
//
// A decoded Go map has no order, so the entries come out sorted by repo id: the
// list is near-static between publishes, and without this the same file renders
// differently on each refresh. Keys that are not a Hugging Face repo id are
// dropped rather than listed, because Name has to stay a string the engine's
// download accepts, and an id carrying a path separator or a leading dash reads
// as a flag or a path.
func normalizeMTPLXPacks(packs map[string]json.RawMessage) []CatalogModel {
	out := make([]CatalogModel, 0, len(packs))
	for repo := range packs {
		repo = strings.TrimSpace(repo)
		if !hfRepoPattern.MatchString(repo) {
			continue
		}
		owner, _, _ := strings.Cut(repo, "/")
		out = append(out, CatalogModel{
			ID:     repo,
			Name:   repo,
			Author: owner,
			URL:    "https://huggingface.co/" + repo,
			// Apple Silicon only, which is why this catalogue is still filtered
			// for the target instead of only shown on a Mac: a client driving a
			// peer that is not one must not be offered packs that peer cannot
			// load.
			AppleOnly: true,
		})
	}
	slices.SortFunc(out, func(a, b CatalogModel) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}
