// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	svcerrors "nvpair-shared/errors"

	"github.com/charmbracelet/bubbles/table"
)

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
			if got := rendered(tc.cols, tc.total); got != tc.total {
				t.Errorf("row consumes %d columns, terminal is %d", got, tc.total)
			}
		})
	}
}

// TestLayoutColumnsWeightedFlex checks a heavier column takes proportionally
// more of the surplus, so a message column can dominate a narrow id column.
func TestLayoutColumnsWeightedFlex(t *testing.T) {
	cols := []column{flexCol("ID", 10, 1), flexCol("MESSAGE", 10, 3)}
	got := layoutColumns(60, cols)

	// budget 60-4=56, minimums 20, surplus 36 split 1:3 -> +9 / +27.
	if got[0].Width != 19 {
		t.Errorf("ID width = %d, want 19", got[0].Width)
	}
	if got[1].Width != 37 {
		t.Errorf("MESSAGE width = %d, want 37", got[1].Width)
	}
}

// TestLayoutColumnsNarrowTerminal checks a terminal too narrow for the
// minimums degrades to those minimums rather than producing widths bubbles
// would render as zero-width or negative.
func TestLayoutColumnsNarrowTerminal(t *testing.T) {
	cols := []column{fixedCol("SEV", 9), flexCol("MESSAGE", 10, 1)}
	for _, total := range []int{0, 1, 10, 20} {
		got := layoutColumns(total, cols)
		if got[0].Width != 9 {
			t.Errorf("total=%d: SEV width = %d, want the declared 9", total, got[0].Width)
		}
		if got[1].Width != 10 {
			t.Errorf("total=%d: MESSAGE width = %d, want the 10 minimum", total, got[1].Width)
		}
	}
}

// TestLayoutColumnsHonoursMinimum checks a flex column never shrinks below its
// stated minimum even when fixed columns consume the whole width.
func TestLayoutColumnsHonoursMinimum(t *testing.T) {
	cols := []column{fixedCol("WIDE", 50), flexCol("REST", 12, 1)}
	got := layoutColumns(40, cols)
	if got[1].Width < 12 {
		t.Errorf("REST width = %d, want at least the 12 minimum", got[1].Width)
	}
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
		if len(cols) == 0 {
			t.Errorf("%s: no columns", name)
		}
		for _, c := range cols {
			if c.Width < minCellWidth {
				t.Errorf("%s: column %q width %d below minimum", name, c.Title, c.Width)
			}
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
	if len(rows) == 0 {
		t.Fatal("no rows to check; the fixture did not populate the table")
	}
	for i, row := range rows {
		if len(row) != cols {
			t.Errorf("row %d has %d cell(s), table has %d column(s)", i, len(row), cols)
		}
	}
}
