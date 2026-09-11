// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// newTable builds a focused table with the shell's shared styling. Views
// pass their columns and then drive rows/size via the returned model.
func newTable(cols []table.Column) table.Model {
	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
	)
	s := table.DefaultStyles()
	// The default Cell/Header styles add one space of padding to each side of
	// every cell, so rows render 2*n_columns wider than the budgeted column
	// widths; the table's viewport then hard-truncates rows at the terminal
	// width, silently clipping the rightmost column. Keep cells at their
	// exact budgeted widths instead.
	s.Cell = lipgloss.NewStyle()
	s.Header = lipgloss.NewStyle().
		Bold(true).
		Foreground(colorAccent).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true)
	s.Selected = s.Selected.
		Bold(true).
		Foreground(lipgloss.Color("0")).
		Background(colorAccent)
	t.SetStyles(s)
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
