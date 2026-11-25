// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mtplxPackFile is MTPLX's published pack list as the publisher serves it: one
// entry per pack keyed by its Hugging Face repo id, plus a `not_yet_published`
// section that names packs no download can resolve yet.
const mtplxPackFile = `{
  "schema": 1,
  "generated_at": "2026-09-24T07:44:40Z",
  "models": {
    "Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Quality": {
      "revision": "470d66a5c53dd9f93e94f84e33b6227fb7959335",
      "min_engine_version": "2.12.0",
      "note": "Higher-fidelity Flash-Next build."
    },
    "Youssofal/Qwen3.6-35B-A3B-MTPLX-Optimized-Balance-FP16": {
      "revision": "0d0a99ba6c91169609572b225c5c4eef03693bb3",
      "min_engine_version": "2.12.0",
      "note": "Balance build: Q8_0 layers and MTP head."
    },
    "Youssofal/Qwen3.8-27B-MTPLX-Optimized-Speed-FP16": {
      "revision": "a6a715ff95a3c0a55629b1a465920e597798a111",
      "min_engine_version": "2.12.0"
    }
  },
  "not_yet_published": [
    { "model": "Youssofal/Unreleased-32B", "size_bytes": 31000000000 }
  ]
}`

// catalogServingMTPLXPacks serves body as MTPLX's pack file and counts the
// requests it receives, which is how the tests below tell one catalogue read from
// a read per keystroke.
func catalogServingMTPLXPacks(t *testing.T, status int, body string) (*catalogService, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, body)
		assert.NoError(t, err, "the fixture must reach the decoder")
	}))
	t.Cleanup(srv.Close)

	c := newCatalogService()
	c.mtplxURL = srv.URL + "/releases/models.json"
	return c, &hits
}

func TestMTPLXCatalog_ListsEveryPublishedPackInRepoIDOrder(t *testing.T) {
	c, hits := catalogServingMTPLXPacks(t, http.StatusOK, mtplxPackFile)

	res, err := c.Catalog(t.Context(), "mtplx", "darwin", "arm64", "", "")

	require.NoError(t, err)
	var got []string
	for _, m := range res.Models {
		got = append(got, m.ID)
	}
	// Sorted, because a decoded Go map has no order and this list barely changes
	// between publishes: the same file must render the same way every time.
	assert.Equal(t, []string{
		"Youssofal/Qwen3.6-35B-A3B-MTPLX-Optimized-Balance-FP16",
		"Youssofal/Qwen3.8-27B-MTPLX-Optimized-Speed-FP16",
		"Youssofal/Qwen3.8-Flash-Next-MTPLX-Optimized-Quality",
	}, got)
	assert.Equal(t, int64(1), hits.Load(), "one catalogue read serves the whole refresh")

	first := res.Models[0]
	// A pack is pulled and served by its repo id, so that is its download name,
	// and its page is the Hugging Face repo that id names.
	assert.Equal(t, "Youssofal/Qwen3.6-35B-A3B-MTPLX-Optimized-Balance-FP16", first.Name)
	assert.Equal(t, "Youssofal", first.Author)
	assert.Equal(t,
		"https://huggingface.co/Youssofal/Qwen3.6-35B-A3B-MTPLX-Optimized-Balance-FP16", first.URL)
	assert.True(t, first.AppleOnly, "every MTPLX pack runs only on Apple Silicon")
	assert.Empty(t, first.UpdatedAt, "the publisher gives no per-pack date")

	assert.False(t, res.Searchable, "the client filters this list; there is nothing to search")
	assert.Empty(t, res.Query)
	assert.Equal(t, mtplxCatalogURL, res.Source)
	assert.Equal(t, "darwin", res.Platform)
	assert.Equal(t, "arm64", res.Arch)
	assert.NotEmpty(t, res.FetchedAt)
}

func TestMTPLXCatalog_AnswersAQueryFromThePacksAlreadyListed(t *testing.T) {
	c, hits := catalogServingMTPLXPacks(t, http.StatusOK, mtplxPackFile)

	browse, err := c.Catalog(t.Context(), "mtplx", "darwin", "arm64", "", "")
	require.NoError(t, err)
	require.Len(t, browse.Models, 3, "the pack list has to be read before a query answers itself")

	// How a source that cannot search is queried: the same whole list comes back
	// and the client filters it, so the publisher is not read again per keystroke.
	res, err := c.Catalog(t.Context(), "mtplx", "darwin", "arm64", "flash", "")

	require.NoError(t, err)
	assert.Equal(t, browse.Models, res.Models, "a query must not narrow a list the client filters")
	assert.False(t, res.Searchable)
	assert.Empty(t, res.Query, "nothing was searched, so there is no query to echo")
	assert.Equal(t, int64(1), hits.Load(), "the second call is served from the cached list")
}

func TestMTPLXCatalog_DropsEntriesThatAreNotDownloadableRepoIDs(t *testing.T) {
	body := `{"schema": 1, "models": {
		"Youssofal/Real-Pack": {"revision": "abc"},
		"NoSlash": {},
		"../escape": {},
		"-flaglike": {},
		"": {}
	}}`
	c, _ := catalogServingMTPLXPacks(t, http.StatusOK, body)

	res, err := c.Catalog(t.Context(), "mtplx", "darwin", "arm64", "", "")

	require.NoError(t, err)
	// Only a `<owner>/<repo>` id can be handed to `mtplx pull`. A key led by a
	// path separator or a dash would read as a path or a flag, and one with no
	// slash names no repo.
	require.Len(t, res.Models, 1)
	assert.Equal(t, "Youssofal/Real-Pack", res.Models[0].ID)
}

func TestMTPLXCatalog_LeavesAPackOutOfAHostThatCannotRunIt(t *testing.T) {
	c, _ := catalogServingMTPLXPacks(t, http.StatusOK, mtplxPackFile)

	res, err := c.Catalog(t.Context(), "mtplx", "linux", "x64", "", "")

	require.NoError(t, err)
	assert.Empty(t, res.Models,
		"an Apple Silicon pack is not offered to a machine that cannot load it")
	assert.Equal(t, "linux", res.Platform)
	assert.Equal(t, "x64", res.Arch)
}

func TestMTPLXCatalog_ReportsAPublisherThatGivesNoList(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{
			name:    "a server error",
			status:  http.StatusBadGateway,
			body:    "",
			wantErr: "mtplx catalog returned 502 Bad Gateway",
		},
		{
			name:    "an empty pack list",
			status:  http.StatusOK,
			body:    `{"schema": 1, "models": {}}`,
			wantErr: "mtplx catalog returned no models",
		},
		{
			name:    "something that is not the pack file",
			status:  http.StatusOK,
			body:    `<html>sign in first</html>`,
			wantErr: "decode mtplx catalog",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := catalogServingMTPLXPacks(t, tc.status, tc.body)

			_, err := c.Catalog(t.Context(), "mtplx", "darwin", "arm64", "", "")

			// No list is a failure rather than an empty catalogue: the model
			// browser says the source could not be read, not that MTPLX has no
			// packs. An empty answer also keeps its retry backoff, so a dead
			// publisher is not re-read on every call.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestMTPLXCatalog_ParsesAPackEntryWhoseFieldsItDoesNotRead(t *testing.T) {
	// The revision, minimum engine version and note decide nothing, so the entry
	// payload is skipped whole: a publisher adding a field cannot break the parse,
	// and none of that text reaches the wire.
	body := `{"schema": 1, "models": {"Youssofal/Pack": {
		"revision": "abc", "min_engine_version": "2.12.0", "note": "text",
		"brand_new_field": {"nested": true}
	}}}`
	c, _ := catalogServingMTPLXPacks(t, http.StatusOK, body)

	res, err := c.Catalog(t.Context(), "mtplx", "darwin", "arm64", "", "")

	require.NoError(t, err)
	require.Len(t, res.Models, 1)
	encoded, err := json.Marshal(res.Models[0])
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"id": "Youssofal/Pack",
		"name": "Youssofal/Pack",
		"author": "Youssofal",
		"url": "https://huggingface.co/Youssofal/Pack",
		"downloads": 0,
		"likes": 0,
		"updatedAt": "",
		"tags": null,
		"appleOnly": true
	}`, string(encoded))
}
