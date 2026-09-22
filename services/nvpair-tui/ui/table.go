// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// cellPadding is the horizontal padding bubbles' table adds to every cell.
// Its default Header and Cell styles both carry Padding(0, 1), and the content
// is rendered with Width(col.Width) *inside* that padding, so a column occupies
// col.Width+2 terminal columns. Layout that reserves less overflows the table
// and the terminal clips the rightmost columns — which is why a 7-wide PORT
// column used to render its header as "po".
const cellPadding = 2

// minCellWidth keeps a column wide enough to show something on a very narrow
// terminal. Below this, content is unreadable anyway and clipping is preferable
// to a zero/negative width bubbles would panic on.
const minCellWidth = 3

// countLines is how many terminal rows a rendered fragment occupies. An empty
// fragment occupies none, which is what lets a caller pass optional chrome
// straight through without branching on whether it is present.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// fitTable sizes a table to whatever is left of a row budget once the given
// chrome has taken its share, and reports whether it fits at all.
//
// Sizing happens here, at render time, rather than in SetSize, because half of
// a view's chrome is conditional: a status toast, an inline editor, a filter
// note, a warning. Sizing against a fixed guess of how many of those are
// present means the view renders more rows than the shell allotted whenever the
// guess is low, and the shell's frame clamp then deletes the last line — which
// is always the newest, most urgent one, since the optional rows are the
// messages. Deriving the height from the chrome actually being rendered makes
// that overflow impossible rather than merely unlikely.
//
// This measures and nothing else. It deliberately does not assemble the view:
// an earlier version returned the non-empty chrome for the caller to splice the
// table into by index, and because it dropped the empty entries, an index the
// caller had counted for could point past the end — which panicked and took the
// whole program down. Callers now build their own line list in their own order,
// where that order is written out in the code and no index is kept in step.
//
// A false return means the terminal is too short for both. The chrome wins: it
// is the status message and the reason the list looks the way it does, while a
// table clamped to a header and no rows conveys nothing. Losing the table is a
// visible, explicable outcome; losing the bottom line is a silent one.
func fitTable(t *table.Model, budget int, chrome ...string) bool {
	used := 0
	for _, c := range chrome {
		used += countLines(c)
	}

	// A header with no data rows is not a table, so that is the floor.
	const minTable = 1 + tableHeaderRows
	if budget-used < minTable {
		return false
	}
	t.SetHeight(budget - used)
	return true
}

// joinLines renders the non-empty fragments as consecutive rows.
//
// Empty fragments are the absent optional chrome. Dropping them here, at the
// point of assembly where the order is written out, is what lets a view list
// every possible line unconditionally and still render only what applies.
func joinLines(parts ...string) string {
	present := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			present = append(present, p)
		}
	}
	return strings.Join(present, "\n")
}

// restoreCursor puts a table's cursor back in range after its contents changed.
//
// bubbles clamps a cursor that has run past the end, but it clamps to len-1 —
// so a table momentarily handed zero rows lands on -1 and stays there, because
// refilling it never moves a cursor already below the range. Every action keyed
// off the highlighted row then reports nothing selected, for the life of the
// screen, until an arrow key happens to rescue it. Views are handed an empty
// list routinely: before the first reply lands, or while a peer is unreachable.
func restoreCursor(t *table.Model, rows int) {
	if rows > 0 && t.Cursor() < 0 {
		t.SetCursor(0)
	}
}

// tableHeaderRows is how much of a bubbles table's height goes to its header:
// the titles plus the border rule beneath them. SetHeight sets the height of the
// whole table, so only height-tableHeaderRows data rows are visible — a caller
// that treats the height it *passed in* as a row count over-counts by exactly
// this much.
//
// Note the asymmetry, which is easy to get wrong in both directions: SetHeight
// takes a total and subtracts the header itself, while Height returns the
// remainder, i.e. the data rows. Convert a budget with visibleTableRows before
// comparing it to a row count; never apply it to Height, which is already
// converted.
const tableHeaderRows = 2

// visibleTableRows is how many data rows a table of total height h can show.
func visibleTableRows(h int) int {
	if h <= tableHeaderRows {
		return 0
	}
	return h - tableHeaderRows
}

// defaultTableWidth is the nominal width a view lays its table out at before the
// first WindowSizeMsg arrives.
//
// Every table must have its columns from construction. The broker replays a
// baseline snapshot as soon as a view subscribes, which can land before the
// terminal size does, and bubbles' renderRow indexes its column slice per row
// cell — so rows against a zero-column table panic rather than render empty.
const defaultTableWidth = 80

// column is a caller's request for one table column. Fixed columns keep their
// width; flex columns share whatever is left over in proportion to their weight
// and never shrink below width, which acts as their minimum.
type column struct {
	title string
	width int
	flex  int
}

// fixedCol is a column that always renders at exactly w content columns.
func fixedCol(title string, w int) column {
	return column{title: title, width: w}
}

// flexCol is a column that absorbs leftover width, never going below min.
// Weight distributes the remainder when several columns flex: two weight-1
// columns split it evenly, weight 2 against weight 1 takes two thirds.
func flexCol(title string, min, weight int) column {
	if weight < 1 {
		weight = 1
	}
	return column{title: title, width: min, flex: weight}
}

// layoutColumns fits cols into total terminal columns, accounting for the
// per-cell padding bubbles adds. Fixed columns are honoured first; whatever
// remains is shared among the flex columns by weight. When even the minimums do
// not fit, every column falls back to its minimum and the table clips — the
// terminal is simply too narrow, and a readable left edge beats evenly
// unreadable columns.
func layoutColumns(total int, cols []column) []table.Column {
	out := make([]table.Column, len(cols))
	for i, c := range cols {
		out[i] = table.Column{Title: c.title, Width: clampWidth(c.width, minCellWidth)}
	}

	budget := total - cellPadding*len(cols)
	var fixed, flexMin, weight int
	for _, c := range cols {
		if c.flex > 0 {
			flexMin += clampWidth(c.width, minCellWidth)
			weight += c.flex
		} else {
			fixed += clampWidth(c.width, minCellWidth)
		}
	}
	if weight == 0 || budget <= fixed+flexMin {
		return out
	}

	// Hand out the surplus by weight, giving the last flex column the
	// rounding remainder so the row fills the width exactly.
	surplus := budget - fixed - flexMin
	granted, lastFlex := 0, -1
	for i, c := range cols {
		if c.flex == 0 {
			continue
		}
		lastFlex = i
		share := surplus * c.flex / weight
		out[i].Width += share
		granted += share
	}
	if lastFlex >= 0 {
		out[lastFlex].Width += surplus - granted
	}
	return out
}

// tableStyles is the shell's shared table styling.
func tableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		Bold(true).
		Foreground(colorAccent).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true)
	s.Selected = s.Selected.
		Bold(true).
		Foreground(lipgloss.Color("0")).
		Background(colorAccent)
	return s
}

// newTable builds a focused table with the shell's shared styling. Views
// pass their columns and then drive rows/size via the returned model.
func newTable(cols []table.Column) table.Model {
	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
	)
	t.SetStyles(tableStyles())
	return t
}

// newStaticTable builds a table for rows nothing can be done to: no highlighted
// row, and no key handling.
//
// Blurring alone is not enough. bubbles highlights whatever row its cursor sits
// on regardless of focus — focus only gates Update — so a read-only table built
// with newTable renders a selection the operator cannot move and that means
// nothing. Neutralising the selected style is the only way to make a table look
// as inert as it is.
func newStaticTable(cols []table.Column) table.Model {
	t := table.New(table.WithColumns(cols))
	s := tableStyles()
	// An empty style, not the cell style. bubbles applies Selected to the
	// already-assembled row, so a style carrying padding indents the cursor row
	// by one column relative to every other row. Only a style that renders its
	// input unchanged leaves the row truly identical to its neighbours.
	s.Selected = lipgloss.NewStyle()
	t.SetStyles(s)
	t.Blur()
	return t
}

// clampWidth returns w bounded to at least min, so a narrow terminal never
// produces negative/zero column widths.
func clampWidth(w, min int) int {
	if w < min {
		return min
	}
	return w
}
