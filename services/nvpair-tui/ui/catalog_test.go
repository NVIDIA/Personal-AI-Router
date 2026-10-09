// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// catalogBroker is a broker that answers engine:catalog, recording the params
// of each call and answering a query with one model named after it.
func catalogBroker(t *testing.T) (*rpc.Client, <-chan map[string]string) {
	t.Helper()
	c1, c2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		c1.Close()
		c2.Close()
	})
	client := rpc.NewClient(c1, c1)
	go client.Run(ctx)

	calls := make(chan map[string]string, 8)
	broker := rpc.NewCodec(c2, c2)
	go func() {
		for {
			req, err := broker.Read()
			if err != nil {
				return
			}
			var params map[string]string
			assert.NoError(t, json.Unmarshal(req.Params, &params))
			calls <- params
			result := `{"models":[{"name":"ggml-org/browse-GGUF:Q4_K_M"}],"searchable":true}`
			if q := params["query"]; q != "" {
				result = `{"models":[{"name":"found/` + q + `-GGUF:Q4_K_M"}],"searchable":true,"query":"` + q + `"}`
			}
			assert.NoError(t, broker.Write(&rpc.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(result)}))
		}
	}()
	return client, calls
}

// typeSearch opens the search field, types text, and submits it.
func typeSearch(b *catalogBrowser, text string) tea.Cmd {
	b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	b.input.SetValue(text)
	cmd, _, _ := b.update(tea.KeyMsg{Type: tea.KeyEnter})
	return cmd
}

// TestSearchableCatalogSearchesUpstream checks a source that can search is
// searched rather than filtered. llama.cpp's browse list is a few publishers'
// repos, and filtering it locally could never find a model outside them.
func TestSearchableCatalogSearchesUpstream(t *testing.T) {
	client, calls := catalogBroker(t)
	b := newCatalogBrowser(client, "llamacpp", "llama.cpp", "this-host", false)
	b.SetSize(100, 24)
	b.update(b.Init()())
	params := <-calls
	require.Empty(t, params["query"], "browse list query")
	require.Equal(t, "llamacpp", params["engine"], "browse list engine")

	cmd := typeSearch(b, "gemma")
	require.NotNil(t, cmd, "a search on a searchable source did not go upstream")
	require.True(t, b.loading, "a search on a searchable source did not start loading")
	assert.Contains(t, b.View(), `Searching for "gemma"`, "the search in flight is not shown")
	b.update(cmd())
	assert.Equal(t, "gemma", (<-calls)["query"], "search query")
	require.Len(t, b.shown, 1, "search result")
	require.Equal(t, "found/gemma-GGUF:Q4_K_M", b.shown[0].Name, "search result")
	assert.Contains(t, b.summary(), `1 results for "gemma"`, "summary must say this is a search")

	cmd, _, _ = b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	assert.Nil(t, cmd, "clearing a search asked the backend again for a list it already had")
	assert.Empty(t, b.query, "clearing the search must clear the query")
	if assert.Len(t, b.shown, 1, "clearing the search must restore the browse list") {
		assert.Equal(t, "ggml-org/browse-GGUF:Q4_K_M", b.shown[0].Name)
	}
}

// TestSupersededSearchIsDropped checks a reply to a search the operator has
// since replaced or cleared does not overwrite what is on screen.
func TestSupersededSearchIsDropped(t *testing.T) {
	b := newCatalogBrowser(nil, "llamacpp", "llama.cpp", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{engine: "llamacpp", gen: b.gen, searchable: true,
		models: []catalogModel{{Name: "browse"}}})

	typeSearch(b, "first")
	first := b.gen
	typeSearch(b, "second")
	b.update(catalogLoadedMsg{engine: "llamacpp", gen: first, searchable: true,
		models: []catalogModel{{Name: "stale"}}})
	assert.True(t, b.loading, "the reply to a replaced search stopped loading")
	assert.False(t, len(b.shown) == 1 && b.shown[0].Name == "stale", "the reply to a replaced search was shown")

	b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	b.update(catalogLoadedMsg{engine: "llamacpp", gen: first + 1, searchable: true,
		models: []catalogModel{{Name: "late"}}})
	assert.False(t, b.loading, "a cleared search's late reply restarted loading")
	if assert.Len(t, b.shown, 1, "a cleared search's late reply replaced the browse list") {
		assert.Equal(t, "browse", b.shown[0].Name)
	}
}

// searchingBrowser has the browse list's one model selected, then starts a
// search whose reply has not arrived.
func searchingBrowser() *catalogBrowser {
	b := newCatalogBrowser(nil, "llamacpp", "llama.cpp", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{engine: "llamacpp", gen: b.gen, searchable: true,
		models: []catalogModel{{Name: "gemma"}}})
	typeSearch(b, "qwen")
	return b
}

// TestEnterDuringASearchDownloadsNothing is the guard for downloading a model
// the operator can no longer see. While a search is in flight the list is
// replaced by "Searching for ...", and enter used to download the row selected
// before it.
func TestEnterDuringASearchDownloadsNothing(t *testing.T) {
	b := searchingBrowser()
	picked, open := browserKey(b, "enter")
	assert.Empty(t, picked, "enter during a search must select nothing")
	assert.True(t, open, "enter during a search must keep the browser open")
}

// TestEnterAfterAFailedSearchDownloadsNothing checks a failed search leaves no
// earlier row to download behind its explanation.
func TestEnterAfterAFailedSearchDownloadsNothing(t *testing.T) {
	b := searchingBrowser()
	b.update(catalogLoadedMsg{engine: "llamacpp", gen: b.gen, err: errFake{}})
	picked, open := browserKey(b, "enter")
	assert.Empty(t, picked, "enter after a failed search must select nothing")
	assert.True(t, open, "enter after a failed search must keep the browser open")
}

// TestUnsearchableCatalogStillFiltersLocally checks a source that cannot search
// keeps the local filter, which issues no request.
func TestUnsearchableCatalogStillFiltersLocally(t *testing.T) {
	b := loadedBrowser()
	assert.Nil(t, typeSearch(b, "qwen"), "filtering a source that cannot search sent a request")
	assert.Empty(t, b.query, "local filtering must not send a query")
	assert.Len(t, b.shown, 1, "local filter to one model")
}

func loadedBrowser() *catalogBrowser {
	b := newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{
		engine:    "ollama",
		fetchedAt: "2026-07-21T02:07:47Z",
		models: []catalogModel{
			{ID: "llama3.2:8b", Name: "llama3.2:8b", Size: 5 << 30, Family: "llama", ParameterSize: "8B"},
			{ID: "qwen3:4b", Name: "qwen3:4b", Size: 2 << 30, Family: "qwen", ParameterSize: "4B"},
			{ID: "phi4:latest", Name: "phi4:latest", Size: 9 << 30, Family: "phi", ParameterSize: "14B"},
		},
	})
	return b
}

func browserKey(b *catalogBrowser, k string) (string, bool) {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	_, picked, open := b.update(msg)
	return picked, open
}

// TestCatalogListsModels checks a loaded catalogue reaches the table.
func TestCatalogListsModels(t *testing.T) {
	b := loadedBrowser()
	assert.False(t, b.loading, "still loading after the reply landed")
	require.Len(t, b.table.Rows(), 3)
	assert.Contains(t, b.View(), "llama3.2:8b", "view omits a model name")
	// The catalogue's age matters for judging staleness.
	assert.Contains(t, b.View(), "2026-07-21", "view omits the catalog date")
}

// TestCatalogFilterNarrowsLocally is the point of the browser: search over the
// whole list without another request, since there is no server-side search.
func TestCatalogFilterNarrowsLocally(t *testing.T) {
	b := loadedBrowser()

	b.filter = "qwen"
	b.refresh()
	require.Len(t, b.shown, 1, "filtering by name")
	require.Equal(t, "qwen3:4b", b.shown[0].Name)

	// Parameter size and family are searched too, so "8b" narrows usefully.
	b.filter = "8b"
	b.refresh()
	if assert.Len(t, b.shown, 1, "filtering by parameter size") {
		assert.Equal(t, "llama3.2:8b", b.shown[0].Name)
	}

	b.filter = "nothing-matches-this"
	b.refresh()
	assert.Empty(t, b.shown, "bogus filter kept rows")
	assert.Contains(t, b.View(), "Nothing matches", "empty result set gives no explanation")

	b.filter = ""
	b.refresh()
	assert.Len(t, b.shown, 3, "clearing the filter must restore every row")
}

// TestCatalogSortCyclesAndOrders checks the sort key cycles and that size sorts
// largest-first.
func TestCatalogSortCyclesAndOrders(t *testing.T) {
	b := loadedBrowser()
	require.Equal(t, catalogSortDefault, b.sortBy, "did not open on the backend's order")

	browserKey(b, "o")
	require.Equal(t, catalogSortName, b.sortBy, "first sort")
	assert.Equal(t, "llama3.2:8b", b.shown[0].Name, "name sort")

	browserKey(b, "o")
	require.Equal(t, catalogSortSize, b.sortBy, "second sort")
	assert.Equal(t, "phi4:latest", b.shown[0].Name, "size sort must put the largest first")

	browserKey(b, "o")
	assert.Equal(t, catalogSortDefault, b.sortBy, "sort did not cycle back round")
}

// TestCatalogEnterReturnsPullReadyName checks selecting a model closes the
// browser and hands back the name the engine's download action accepts.
func TestCatalogEnterReturnsPullReadyName(t *testing.T) {
	b := loadedBrowser()
	b.table.SetCursor(1)

	picked, open := browserKey(b, "enter")
	assert.False(t, open, "browser stayed open after a selection")
	assert.Equal(t, "qwen3:4b", picked, "highlighted model's pull name")
}

// TestCatalogEscapeSelectsNothing checks backing out downloads nothing.
func TestCatalogEscapeSelectsNothing(t *testing.T) {
	b := loadedBrowser()
	picked, open := browserKey(b, "esc")
	assert.False(t, open, "esc left the browser open")
	assert.Empty(t, picked, "esc must select nothing")
}

// TestCatalogSearchCapturesKeys checks the filter field owns the keyboard, so
// typing a model name cannot trigger the sort or selection keys.
//
// The shell-level guarantee is asserted separately, through the detail screen
// that owns the browser: an earlier version of this test called a
// CapturingInput method on the browser that nothing in production consulted, so
// it proved a path that never ran.
func TestCatalogSearchCapturesKeys(t *testing.T) {
	b := loadedBrowser()
	browserKey(b, "/")
	require.True(t, b.searching, "search did not take the keyboard")

	// 'o' is the sort key outside the field; inside it is a character.
	before := b.sortBy
	browserKey(b, "o")
	assert.Equal(t, before, b.sortBy, "a keystroke typed into the filter triggered the sort")

	browserKey(b, "esc")
	assert.False(t, b.searching, "esc did not leave the filter")
}

// TestCatalogLoadFailureExplainsItself checks a failed load says so instead of
// showing an empty list that reads as "this engine has nothing".
func TestCatalogLoadFailureExplainsItself(t *testing.T) {
	b := newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{engine: "ollama", err: errFake{}})

	assert.False(t, b.loading, "still loading after a failure")
	assert.NotEmpty(t, b.status.render(), "failure produced no message")
}

// TestCatalogLoadFailureCanBeRetried is the regression guard for a failed load
// with no way forward but closing the browser and opening it again.
func TestCatalogLoadFailureCanBeRetried(t *testing.T) {
	b := newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false)
	b.SetSize(100, 24)
	cmd, _, _ := b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	assert.Nil(t, cmd, "r reloaded a catalog that had not failed")

	b.update(catalogLoadedMsg{engine: "ollama", err: errFake{}})
	assert.Contains(t, b.View(), "Press r to try again", "a failed load did not offer a retry")
	cmd, _, open := b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	require.NotNil(t, cmd, "r did not ask for the catalog again")
	require.True(t, open, "r must keep the browser open")
	assert.True(t, b.loading, "the retry did not show the catalog as loading")
	assert.False(t, b.failed, "the retry must clear the failure")

	loaded := loadedBrowser()
	assert.False(t, loaded.failed, "a successful load was counted as a failure")
}

// TestCatalogIgnoresOtherEnginesReply checks a late reply for an engine the
// operator has moved on from does not populate this browser.
func TestCatalogIgnoresOtherEnginesReply(t *testing.T) {
	b := newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{
		engine: "lmstudio",
		models: []catalogModel{{ID: "x", Name: "x"}},
	})
	assert.Empty(t, b.all, "accepted a catalog for a different engine")
}

// TestCatalogEmptyCatalogIsDistinctFromNoMatch checks the two empty states read
// differently, because the fixes are different.
func TestCatalogEmptyCatalogIsDistinctFromNoMatch(t *testing.T) {
	b := newCatalogBrowser(nil, "vllm", "vLLM", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{engine: "vllm", models: nil})

	assert.Contains(t, b.View(), "No catalog available")
}

// TestPeerCatalogNamesTheMachineItWasFilteredFor checks a peer's list states
// both halves of what it was filtered for. The operating system alone does not
// decide what installs: an Intel Mac and an Apple Silicon one are offered
// different LM Studio lists.
func TestPeerCatalogNamesTheMachineItWasFilteredFor(t *testing.T) {
	b := newCatalogBrowser(nil, "lmstudio", "LM Studio", "peer-host", true)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{
		engine: "lmstudio",
		target: "darwin/amd64",
		models: []catalogModel{{ID: "x", Name: "x"}},
	})
	assert.Contains(t, b.summary(), "filtered for darwin/amd64", "summary must say which machine the list applies to")

	// This machine's own list needs no caveat: it is filtered for itself.
	local := loadedBrowser()
	local.target = "darwin/arm64"
	assert.NotContains(t, local.summary(), "filtered for", "local summary carries a caveat meant for peers")
}

func TestShortDate(t *testing.T) {
	assert.Equal(t, "2026-07-21", shortDate("2026-07-21T02:07:47.321Z"))
	assert.Equal(t, "short", shortDate("short"))
}

// errFake is a minimal error for the failure path.
type errFake struct{}

func (errFake) Error() string { return "catalog unavailable" }
