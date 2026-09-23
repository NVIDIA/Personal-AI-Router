// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

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
