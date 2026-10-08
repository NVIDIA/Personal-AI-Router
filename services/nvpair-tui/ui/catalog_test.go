// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
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
			_ = json.Unmarshal(req.Params, &params)
			calls <- params
			result := `{"models":[{"name":"ggml-org/browse-GGUF:Q4_K_M"}],"searchable":true}`
			if q := params["query"]; q != "" {
				result = `{"models":[{"name":"found/` + q + `-GGUF:Q4_K_M"}],"searchable":true,"query":"` + q + `"}`
			}
			_ = broker.Write(&rpc.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(result)})
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
	if got := <-calls; got["query"] != "" || got["engine"] != "llamacpp" {
		t.Fatalf("the browse list was asked for with %v", got)
	}

	cmd := typeSearch(b, "gemma")
	if cmd == nil || !b.loading {
		t.Fatal("a search on a searchable source did not go upstream")
	}
	if !strings.Contains(b.View(), `Searching for "gemma"`) {
		t.Errorf("the search in flight is not shown: %q", b.View())
	}
	b.update(cmd())
	if got := <-calls; got["query"] != "gemma" {
		t.Errorf("the search was sent as %v", got)
	}
	if len(b.shown) != 1 || b.shown[0].Name != "found/gemma-GGUF:Q4_K_M" {
		t.Fatalf("showing %v, want the search's result", b.shown)
	}
	if !strings.Contains(b.summary(), `1 results for "gemma"`) {
		t.Errorf("summary %q does not say this is a search", b.summary())
	}

	if cmd, _, _ := b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")}); cmd != nil {
		t.Error("clearing a search asked the backend again for a list it already had")
	}
	if b.query != "" || len(b.shown) != 1 || b.shown[0].Name != "ggml-org/browse-GGUF:Q4_K_M" {
		t.Errorf("clearing the search showed %v, want the browse list back", b.shown)
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
	if !b.loading || len(b.shown) == 1 && b.shown[0].Name == "stale" {
		t.Error("the reply to a replaced search was shown")
	}

	b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	b.update(catalogLoadedMsg{engine: "llamacpp", gen: first + 1, searchable: true,
		models: []catalogModel{{Name: "late"}}})
	if b.loading || len(b.shown) != 1 || b.shown[0].Name != "browse" {
		t.Errorf("a cleared search's late reply replaced the browse list: %v", b.shown)
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
	if picked, open := browserKey(b, "enter"); picked != "" || !open {
		t.Errorf("enter during a search picked %q, open %v; want nothing picked and the browser open", picked, open)
	}
}

// TestEnterAfterAFailedSearchDownloadsNothing checks a failed search leaves no
// earlier row to download behind its explanation.
func TestEnterAfterAFailedSearchDownloadsNothing(t *testing.T) {
	b := searchingBrowser()
	b.update(catalogLoadedMsg{engine: "llamacpp", gen: b.gen, err: errFake{}})
	if picked, open := browserKey(b, "enter"); picked != "" || !open {
		t.Errorf("enter after a failed search picked %q, open %v; want nothing picked and the browser open", picked, open)
	}
}

// TestUnsearchableCatalogStillFiltersLocally checks a source that cannot search
// keeps the local filter, which issues no request.
func TestUnsearchableCatalogStillFiltersLocally(t *testing.T) {
	b := loadedBrowser()
	if cmd := typeSearch(b, "qwen"); cmd != nil {
		t.Error("filtering a source that cannot search sent a request")
	}
	if b.query != "" || len(b.shown) != 1 {
		t.Errorf("query %q, %d shown; want a local filter to one model", b.query, len(b.shown))
	}
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
	if b.loading {
		t.Error("still loading after the reply landed")
	}
	if got := len(b.table.Rows()); got != 3 {
		t.Fatalf("table has %d rows, want 3", got)
	}
	if !strings.Contains(b.View(), "llama3.2:8b") {
		t.Error("view omits a model name")
	}
	// The catalogue's age matters for judging staleness.
	if !strings.Contains(b.View(), "2026-07-21") {
		t.Errorf("view omits the catalog date: %q", b.summary())
	}
}

// TestCatalogFilterNarrowsLocally is the point of the browser: search over the
// whole list without another request, since there is no server-side search.
func TestCatalogFilterNarrowsLocally(t *testing.T) {
	b := loadedBrowser()

	b.filter = "qwen"
	b.refresh()
	if len(b.shown) != 1 || b.shown[0].Name != "qwen3:4b" {
		t.Fatalf("filtering by name gave %d rows", len(b.shown))
	}

	// Parameter size and family are searched too, so "8b" narrows usefully.
	b.filter = "8b"
	b.refresh()
	if len(b.shown) != 1 || b.shown[0].Name != "llama3.2:8b" {
		t.Errorf("filtering by parameter size gave %d rows", len(b.shown))
	}

	b.filter = "nothing-matches-this"
	b.refresh()
	if len(b.shown) != 0 {
		t.Errorf("bogus filter kept %d rows", len(b.shown))
	}
	if !strings.Contains(b.View(), "Nothing matches") {
		t.Error("empty result set gives no explanation")
	}

	b.filter = ""
	b.refresh()
	if len(b.shown) != 3 {
		t.Errorf("clearing the filter left %d rows", len(b.shown))
	}
}

// TestCatalogSortCyclesAndOrders checks the sort key cycles and that size sorts
// largest-first.
func TestCatalogSortCyclesAndOrders(t *testing.T) {
	b := loadedBrowser()
	if b.sortBy != catalogSortDefault {
		t.Fatal("did not open on the backend's order")
	}

	browserKey(b, "o")
	if b.sortBy != catalogSortName {
		t.Fatalf("first sort = %v", b.sortBy)
	}
	if b.shown[0].Name != "llama3.2:8b" {
		t.Errorf("name sort leads with %q", b.shown[0].Name)
	}

	browserKey(b, "o")
	if b.sortBy != catalogSortSize {
		t.Fatalf("second sort = %v", b.sortBy)
	}
	if b.shown[0].Name != "phi4:latest" {
		t.Errorf("size sort leads with %q, want the largest", b.shown[0].Name)
	}

	browserKey(b, "o")
	if b.sortBy != catalogSortDefault {
		t.Error("sort did not cycle back round")
	}
}

// TestCatalogEnterReturnsPullReadyName checks selecting a model closes the
// browser and hands back the name the engine's download action accepts.
func TestCatalogEnterReturnsPullReadyName(t *testing.T) {
	b := loadedBrowser()
	b.table.SetCursor(1)

	picked, open := browserKey(b, "enter")
	if open {
		t.Error("browser stayed open after a selection")
	}
	if picked != "qwen3:4b" {
		t.Errorf("picked %q, want the highlighted model's pull name", picked)
	}
}

// TestCatalogEscapeSelectsNothing checks backing out downloads nothing.
func TestCatalogEscapeSelectsNothing(t *testing.T) {
	b := loadedBrowser()
	picked, open := browserKey(b, "esc")
	if open {
		t.Error("esc left the browser open")
	}
	if picked != "" {
		t.Errorf("esc picked %q", picked)
	}
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
	if !b.searching {
		t.Fatal("search did not take the keyboard")
	}

	// 'o' is the sort key outside the field; inside it is a character.
	before := b.sortBy
	browserKey(b, "o")
	if b.sortBy != before {
		t.Error("a keystroke typed into the filter triggered the sort")
	}

	browserKey(b, "esc")
	if b.searching {
		t.Error("esc did not leave the filter")
	}
}

// TestCatalogLoadFailureExplainsItself checks a failed load says so instead of
// showing an empty list that reads as "this engine has nothing".
func TestCatalogLoadFailureExplainsItself(t *testing.T) {
	b := newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{engine: "ollama", err: errFake{}})

	if b.loading {
		t.Error("still loading after a failure")
	}
	if b.status.render() == "" {
		t.Error("failure produced no message")
	}
}

// TestCatalogLoadFailureCanBeRetried is the regression guard for a failed load
// with no way forward but closing the browser and opening it again.
func TestCatalogLoadFailureCanBeRetried(t *testing.T) {
	b := newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false)
	b.SetSize(100, 24)
	if cmd, _, _ := b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd != nil {
		t.Error("r reloaded a catalog that had not failed")
	}

	b.update(catalogLoadedMsg{engine: "ollama", err: errFake{}})
	if !strings.Contains(b.View(), "Press r to try again") {
		t.Errorf("a failed load did not offer a retry: %q", b.View())
	}
	cmd, _, open := b.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil || !open {
		t.Fatal("r did not ask for the catalog again")
	}
	if !b.loading || b.failed {
		t.Error("the retry did not show the catalog as loading")
	}

	loaded := loadedBrowser()
	if loaded.failed {
		t.Error("a successful load was counted as a failure")
	}
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
	if len(b.all) != 0 {
		t.Error("accepted a catalog for a different engine")
	}
}

// TestCatalogEmptyCatalogIsDistinctFromNoMatch checks the two empty states read
// differently, because the fixes are different.
func TestCatalogEmptyCatalogIsDistinctFromNoMatch(t *testing.T) {
	b := newCatalogBrowser(nil, "vllm", "vLLM", "this-host", false)
	b.SetSize(100, 24)
	b.update(catalogLoadedMsg{engine: "vllm", models: nil})

	if !strings.Contains(b.View(), "No catalog available") {
		t.Errorf("view = %q", b.View())
	}
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
	if got := b.summary(); !strings.Contains(got, "filtered for darwin/amd64") {
		t.Errorf("summary %q does not say which machine the list applies to", got)
	}

	// This machine's own list needs no caveat: it is filtered for itself.
	local := loadedBrowser()
	local.target = "darwin/arm64"
	if got := local.summary(); strings.Contains(got, "filtered for") {
		t.Errorf("local summary %q carries a caveat meant for peers", got)
	}
}

func TestShortDate(t *testing.T) {
	if got := shortDate("2026-07-21T02:07:47.321Z"); got != "2026-07-21" {
		t.Errorf("shortDate = %q", got)
	}
	if got := shortDate("short"); got != "short" {
		t.Errorf("shortDate passed through as %q", got)
	}
}

// errFake is a minimal error for the failure path.
type errFake struct{}

func (errFake) Error() string { return "catalog unavailable" }
