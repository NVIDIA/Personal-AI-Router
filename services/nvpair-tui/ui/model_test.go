// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strconv"
	"strings"
	"testing"

	svcerrors "nvpair-shared/errors"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// stubView renders a fixed block of rows, optionally far more (and far wider)
// than the size it was handed, standing in for a view whose own height
// accounting is wrong.
type stubView struct {
	title string
	rows  int
	width int
}

func (s *stubView) Title() string          { return s.title }
func (s *stubView) Init() tea.Cmd          { return nil }
func (s *stubView) SetSize(_, _ int)       {}
func (s *stubView) Update(tea.Msg) tea.Cmd { return nil }
func (s *stubView) Help() []key.Binding    { return nil }
func (s *stubView) View() string {
	line := strings.Repeat("x", s.width)
	lines := make([]string, s.rows)
	for i := range lines {
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func newTestModel(views ...View) Model {
	m := New(nil, nil, views)
	m.width, m.height = 80, 24
	return m
}

// TestViewFrameIsExactlyTerminalSized is the regression guard for the duplicated
// bottom rows after a resize. The shell must emit exactly as many rows and
// columns as the terminal has, whatever the active view renders: an over-tall
// frame scrolls the alt screen and leaves the previous frame's tail behind.
func TestViewFrameIsExactlyTerminalSized(t *testing.T) {
	cases := []struct {
		name string
		view *stubView
	}{
		{"view renders far too many rows", &stubView{title: "Over", rows: 200, width: 40}},
		{"view renders too few rows", &stubView{title: "Under", rows: 1, width: 40}},
		{"view renders lines wider than the terminal", &stubView{title: "Wide", rows: 5, width: 500}},
		{"view renders nothing", &stubView{title: "Empty", rows: 0, width: 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(tc.view)
			out := m.View()
			if got := lipgloss.Height(out); got != m.height {
				t.Errorf("frame is %d rows, terminal is %d", got, m.height)
			}
			if got := lipgloss.Width(out); got > m.width {
				t.Errorf("frame is %d columns wide, terminal is %d", got, m.width)
			}
		})
	}
}

// TestFrameStaysExactWithTheUpdateBanner is the arithmetic check for the notice
// row.
//
// The banner adds a row to the frame and takes one from the content budget, and
// those two have to cancel at every height. If they do not, the frame is either
// a row too tall — which scrolls the alt screen and leaves the previous frame's
// tail behind — or a row short of the terminal.
//
// Swept across every supported height rather than sampled, and over the real
// views, because the budget also depends on the active view's footer.
func TestFrameStaysExactWithTheUpdateBanner(t *testing.T) {
	withProductVersion(t, "0.91.7")

	for h := minTerminalHeight; h <= 44; h++ {
		for i := range defaultViews(nil) {
			m := newTestModel(defaultViews(nil)...)
			m.width, m.height = 80, h
			m.selectTab(i)

			plain := m.contentHeight()
			m = send(m, updateCheckMsg{latest: "0.92.0"})

			if got := lipgloss.Height(m.View()); got != h {
				t.Fatalf("height %d, tab %d: frame is %d rows with the banner up", h, i+1, got)
			}
			// One row taken, unless the budget had already bottomed out at its
			// floor of one — below that there is nothing left to give.
			if withBanner := m.contentHeight(); plain > 1 && withBanner != plain-1 {
				t.Fatalf("height %d, tab %d: budget %d -> %d, want one row taken",
					h, i+1, plain, withBanner)
			}
		}
	}
}

// TestViewFrameHeightAcrossTerminalSizes checks the budget holds at the small
// sizes where the header, tab bar, and footer alone can exceed the terminal.
func TestViewFrameHeightAcrossTerminalSizes(t *testing.T) {
	for _, h := range []int{4, 5, 10, 24, 60} {
		m := New(nil, nil, []View{&stubView{title: "T", rows: 100, width: 10}})
		m.width, m.height = 80, h
		if got := lipgloss.Height(m.View()); got != h {
			t.Errorf("height %d: frame is %d rows", h, got)
		}
	}
}

// TestViewBeforeFirstResize checks the shell renders a placeholder rather than a
// zero-sized frame before the terminal size arrives.
func TestViewBeforeFirstResize(t *testing.T) {
	m := New(nil, nil, []View{&stubView{title: "T", rows: 3, width: 10}})
	if out := m.View(); out != "starting..." {
		t.Errorf("pre-resize view = %q, want the placeholder", out)
	}
}

// TestJumpDigitsMatchTheTabsExactly checks the digit binding is neither short
// nor long.
//
// Short means a tab reachable only by tabbing to it, with nothing on screen to
// explain why its number did nothing. Long is what shipped: the footer read
// "1-9 go to tab" against five tabs, advertising four keys that do nothing —
// which is the more common failure, because the range is a string a reader has
// to remember to update.
func TestJumpDigitsMatchTheTabsExactly(t *testing.T) {
	views := defaultViews(nil)
	keys := newGlobalKeyMap(len(views)).JumpTab

	if got, want := len(keys.Keys()), len(views); got != want {
		t.Errorf("%d jump digits (%v) for %d tabs", got, keys.Keys(), want)
	}
	if got, want := keys.Help().Key, "1-"+strconv.Itoa(len(views)); got != want {
		t.Errorf("footer advertises %q, want %q", got, want)
	}
}

// TestJumpDigitsLabelDegenerateCounts covers the label at the edges, since it is
// assembled rather than written out.
func TestJumpDigitsLabelDegenerateCounts(t *testing.T) {
	cases := map[int]string{1: "1", 2: "1-2", 5: "1-5", 9: "1-9", 12: "1-9"}
	for tabs, want := range cases {
		if got := tabDigitsHelp(tabs); got != want {
			t.Errorf("%d tabs labelled %q, want %q", tabs, got, want)
		}
		// A twelfth tab must not produce a tenth key that cannot be typed.
		if got := len(tabDigits(tabs)); got > 9 {
			t.Errorf("%d tabs produced %d keys, want at most 9", tabs, got)
		}
	}
}

// TestDigitKeysSelectTabs pins the shortcut the numbered tab bar advertises.
func TestDigitKeysSelectTabs(t *testing.T) {
	m := newTestModel(
		&stubView{title: "One", rows: 1},
		&stubView{title: "Two", rows: 1},
		&stubView{title: "Three", rows: 1},
	)

	press := func(k string) {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		m = updated.(Model)
	}

	press("3")
	if m.active != 2 {
		t.Errorf("after '3', active = %d, want 2", m.active)
	}
	press("1")
	if m.active != 0 {
		t.Errorf("after '1', active = %d, want 0", m.active)
	}
	// Out of range for three tabs: the selection must not move.
	press("9")
	if m.active != 0 {
		t.Errorf("after out-of-range '9', active = %d, want 0", m.active)
	}
}

// TestTabWrapsBothDirections checks prev from the first tab lands on the last
// rather than going negative.
func TestTabWrapsBothDirections(t *testing.T) {
	m := newTestModel(
		&stubView{title: "One", rows: 1},
		&stubView{title: "Two", rows: 1},
	)
	m.selectTab(-1)
	if m.active != 1 {
		t.Errorf("selectTab(-1) = %d, want 1", m.active)
	}
	m.selectTab(2)
	if m.active != 0 {
		t.Errorf("selectTab(2) = %d, want 0", m.active)
	}
}

// TestErrorTabLabelCarriesTheCount checks the tab bar is the error indicator, so
// nothing extra is needed to notice a problem from another tab.
func TestErrorTabLabelCarriesTheCount(t *testing.T) {
	v := newErrorsView(nil)

	if got := v.Title(); got != "Errors" {
		t.Errorf("clean label = %q, want a bare title", got)
	}

	v.setErrors([]svcerrors.ServiceError{{ID: "a", Message: "boom", Severity: "error"}})
	if got := v.Title(); got != "Errors (1)" {
		t.Errorf("label = %q, want a count", got)
	}

	v.setErrors([]svcerrors.ServiceError{
		{ID: "a", Message: "boom", Severity: "error"},
		{ID: "b", Message: "meh", Severity: "warning"},
	})
	if got := v.Title(); got != "Errors (2)" {
		t.Errorf("label = %q, want the updated count", got)
	}

	// Clearing the last error takes the count away again.
	v.setErrors(nil)
	if got := v.Title(); got != "Errors" {
		t.Errorf("label = %q after clearing, want a bare title", got)
	}
}

// TestErrorsTabIsReachableLikeAnyOther checks it behaves as a plain tab: the
// digit selects it and nothing intercepts the keyboard.
func TestErrorsTabIsReachableLikeAnyOther(t *testing.T) {
	errors := newErrorsView(nil)
	m := newTestModel(
		&stubView{title: "One", rows: 1},
		&stubView{title: "Two", rows: 1},
		errors,
	)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	m = updated.(Model)
	if m.active != 2 {
		t.Fatalf("digit 3 selected tab %d, want the errors tab", m.active)
	}
	if m.activeView() != View(errors) {
		t.Error("active view is not the errors tab")
	}

	// And tabbing away works, unlike an overlay that had to be dismissed.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	m = updated.(Model)
	if m.active != 0 {
		t.Error("could not leave the errors tab with a digit")
	}
}

// TestErrorsTabFrameStaysBounded checks a long error list obeys the same frame
// budget as any other tab.
func TestErrorsTabFrameStaysBounded(t *testing.T) {
	errors := newErrorsView(nil)
	m := newTestModel(errors)
	m.resizeViews()

	errs := make([]svcerrors.ServiceError, 200)
	for i := range errs {
		errs[i] = svcerrors.ServiceError{ID: strconv.Itoa(i), Message: strings.Repeat("y", 300)}
	}
	errors.setErrors(errs)

	out := m.View()
	if got := lipgloss.Height(out); got != m.height {
		t.Errorf("frame is %d rows, terminal is %d", got, m.height)
	}
	if got := lipgloss.Width(out); got > m.width {
		t.Errorf("frame is %d columns, terminal is %d", got, m.width)
	}
}

func TestFitLines(t *testing.T) {
	if got := fitLines("a\nb\nc", 2); got != "a\nb" {
		t.Errorf("truncate: got %q", got)
	}
	if got := fitLines("a", 3); got != "a\n\n" {
		t.Errorf("pad: got %q", got)
	}
	if got := fitLines("a\nb", 2); got != "a\nb" {
		t.Errorf("exact: got %q", got)
	}
}
