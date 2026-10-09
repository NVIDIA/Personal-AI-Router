// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"
	"unicode/utf8"

	svcerrors "nvpair-shared/errors"

	"github.com/charmbracelet/bubbles/table"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTruncate covers the cut and its edges. It counts runes, not bytes: GPU
// and CPU model names reach it, a byte slice can land inside a multi-byte
// sequence, and its callers pad with %-Ns, which counts runes too.
func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits", "abc", 5, "abc"},
		{"exactly fits", "abcde", 5, "abcde"},
		{"cut with an ellipsis", "abcdef", 5, "abcd…"},
		{"multi-byte cut stays on a rune boundary", "ααααααααα™", 5, "αααα…"},
		{"multi-byte that fits is untouched", "ααα", 5, "ααα"},
		{"one column is only the ellipsis", "abc", 1, "…"},
		{"zero columns is empty", "abc", 0, ""},
		{"negative columns is empty", "abc", -1, ""},
		{"empty stays empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.in, tc.max)
			assert.Equal(t, tc.want, got)
			assert.True(t, utf8.ValidString(got), "truncate must produce valid UTF-8")
		})
	}
}

// rendered is the terminal width a laid-out row actually consumes: every column
// costs its declared width plus the padding bubbles wraps each cell in.
func rendered(cols []column, total int) int {
	got := layoutColumns(total, cols)
	sum := 0
	for _, c := range got {
		sum += c.Width + cellPadding
	}
	return sum
}

// TestLayoutColumnsFillsWidthExactly is the regression guard for the clipped
// headers ("po" for PORT, "SEE" for SEEN): a laid-out row must consume the
// width it was given, never more. Sizing that ignores cellPadding overflows and
// the terminal drops the rightmost columns.
func TestLayoutColumnsFillsWidthExactly(t *testing.T) {
	cases := []struct {
		name  string
		total int
		cols  []column
	}{
		{
			name:  "proxies upstream table",
			total: 80,
			cols: []column{
				flexCol("ID", 10, 1),
				flexCol("HOST", 10, 1),
				fixedCol("PORT", 7),
			},
		},
		{
			name:  "nodes table",
			total: 100,
			cols: []column{
				flexCol("NAME", 10, 1),
				flexCol("ADDRESS", 10, 1),
				fixedCol("PORT", 7),
				fixedCol("LAST SEEN", 10),
				fixedCol("STATUS", 11),
			},
		},
		{
			name:  "single flex column takes the remainder",
			total: 60,
			cols: []column{
				fixedCol("SEV", 9),
				flexCol("MESSAGE", 10, 1),
			},
		},
		{
			name:  "uneven division leaves no gap",
			total: 77,
			cols: []column{
				flexCol("A", 5, 1),
				flexCol("B", 5, 1),
				flexCol("C", 5, 1),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.total, rendered(tc.cols, tc.total), "row must consume the terminal width")
		})
	}
}

// TestLayoutColumnsWeightedFlex checks a heavier column takes proportionally
// more of the surplus, so a message column can dominate a narrow id column.
func TestLayoutColumnsWeightedFlex(t *testing.T) {
	cols := []column{flexCol("ID", 10, 1), flexCol("MESSAGE", 10, 3)}
	got := layoutColumns(60, cols)

	// budget 60-4=56, minimums 20, surplus 36 split 1:3 -> +9 / +27.
	assert.Equal(t, 19, got[0].Width)
	assert.Equal(t, 37, got[1].Width)
}

// TestLayoutColumnsNarrowTerminal checks a terminal too narrow for the
// minimums degrades to those minimums rather than producing widths bubbles
// would render as zero-width or negative.
func TestLayoutColumnsNarrowTerminal(t *testing.T) {
	cols := []column{fixedCol("SEV", 9), flexCol("MESSAGE", 10, 1)}
	for _, total := range []int{0, 1, 10, 20} {
		got := layoutColumns(total, cols)
		assert.Equal(t, 9, got[0].Width, "total=%d: SEV must retain its declared width", total)
		assert.Equal(t, 10, got[1].Width, "total=%d: MESSAGE must retain its minimum width", total)
	}
}

// TestLayoutColumnsHonoursMinimum checks a flex column never shrinks below its
// stated minimum even when fixed columns consume the whole width.
func TestLayoutColumnsHonoursMinimum(t *testing.T) {
	cols := []column{fixedCol("WIDE", 50), flexCol("REST", 12, 1)}
	got := layoutColumns(40, cols)
	assert.GreaterOrEqual(t, got[1].Width, 12, "REST must retain its minimum width")
}

// TestViewsAcceptRowsBeforeResize guards a panic every table view was exposed
// to: the broker replays a baseline snapshot as soon as a view subscribes, which
// can arrive before the first WindowSizeMsg. bubbles' renderRow indexes its
// column slice per row cell, so a table built with no columns panics on the
// first row rather than rendering empty.
func TestViewsAcceptRowsBeforeResize(t *testing.T) {
	t.Run("nodes from discovery", func(t *testing.T) {
		v := newNodesView(nil)
		v.feeds.discovered = []availableNode{{HostUUID: "u", Name: "n", IPAddress: "10.0.0.1", Port: 1}}
		v.rebuild()
	})
	t.Run("nodes from cluster roster", func(t *testing.T) {
		v := newNodesView(nil)
		v.feeds.members = []clusterNode{{ID: "id", NodeUUID: "u", Name: "n", State: "joined", Port: 1}}
		v.rebuild()
	})
	t.Run("nodes from manual list", func(t *testing.T) {
		v := newNodesView(nil)
		v.feeds.manual = []manualNode{{ID: "id", Name: "n", Address: "10.0.0.1"}}
		v.rebuild()
	})
	t.Run("jobs", func(t *testing.T) {
		newJobsView(nil).upsert(workload{ID: "w", Model: "m", Engine: "ollama", State: "running"})
	})
	t.Run("node detail engines and models", func(t *testing.T) {
		d := newNodeDetail(nil, nodeRow{
			key:            "u",
			name:           "n",
			self:           true,
			modelsByEngine: map[string][]string{"ollama": {"llama3.2"}},
			loadedByEngine: map[string][]string{"ollama": {"llama3.2"}},
		})
		d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
		d.refreshEngines()
		d.refreshModels()
	})
	t.Run("errors", func(t *testing.T) {
		newErrorsView(nil).setErrors([]svcerrors.ServiceError{{ID: "e", Message: "boom"}})
	})
	t.Run("service workers", func(t *testing.T) {
		newServiceView(nil).refreshWorkers()
	})
}

// TestEveryTableViewHasColumnsAtConstruction is the direct invariant behind the
// panic above, stated per view so a new view cannot regress it silently.
func TestEveryTableViewHasColumnsAtConstruction(t *testing.T) {
	widths := map[string][]table.Column{
		"nodes":                 nodesColumns(defaultTableWidth),
		"jobs":                  workloadColumns(defaultTableWidth),
		"detail engines local":  detailEngineColumns(defaultTableWidth, false),
		"detail engines remote": detailEngineColumns(defaultTableWidth, true),
		"detail models":         detailModelColumns(defaultTableWidth),
		"service workers":       serviceWorkerColumns(defaultTableWidth),
	}
	for name, cols := range widths {
		assert.NotEmpty(t, cols, "%s: no columns", name)
		for _, c := range cols {
			assert.GreaterOrEqual(t, c.Width, minCellWidth, "%s: column %q", name, c.Title)
		}
	}
}

// TestRowsMatchTheirColumns pins the two halves of a table together.
//
// bubbles' renderRow walks the column slice and indexes the row per column, so a
// row with fewer cells than columns panics and one with more silently drops the
// extras. Nothing else catches it: both halves compile independently, and a view
// that builds its rows in one function and its columns in another can lose a
// cell without a single type error — which is exactly the shape of the edit that
// removed the node table's timestamp column.
func TestRowsMatchTheirColumns(t *testing.T) {
	t.Run("nodes", func(t *testing.T) {
		v := newNodesView(nil)
		v.feeds.discovered = []availableNode{
			{HostUUID: "u", Name: "n", IPAddress: "10.0.0.1", Port: 1},
		}
		v.rebuild()
		assertRowWidths(t, v.table)
	})
	t.Run("jobs", func(t *testing.T) {
		v := newJobsView(nil)
		v.upsert(workload{ID: "w", Model: "m", Engine: "ollama", State: "running"})
		assertRowWidths(t, v.table)
	})
	t.Run("errors", func(t *testing.T) {
		v := newErrorsView(nil)
		v.setErrors([]svcerrors.ServiceError{{ID: "e", Message: "boom"}})
		assertRowWidths(t, v.table)
	})
	t.Run("service workers", func(t *testing.T) {
		v := newServiceView(nil)
		v.refreshWorkers()
		assertRowWidths(t, v.workers)
	})
	t.Run("node detail engines and models", func(t *testing.T) {
		for _, remote := range []bool{false, true} {
			d := newNodeDetail(nil, nodeRow{
				key:            "u",
				name:           "n",
				self:           !remote,
				modelsByEngine: map[string][]string{"ollama": {"llama3.2"}},
				loadedByEngine: map[string][]string{"ollama": {"llama3.2"}},
			})
			d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
			d.refreshEngines()
			d.refreshModels()
			assertRowWidths(t, d.engineTable)
			assertRowWidths(t, d.modelTable)
		}
	})
}

func assertRowWidths(t *testing.T, m table.Model) {
	t.Helper()
	cols := len(m.Columns())
	rows := m.Rows()
	require.NotEmpty(t, rows, "no rows to check; the fixture did not populate the table")
	for i, row := range rows {
		assert.Len(t, row, cols, "row %d must match the table's columns", i)
	}
}
