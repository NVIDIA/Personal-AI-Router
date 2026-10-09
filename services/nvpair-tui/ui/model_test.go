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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	const termWidth, termHeight = 80, 24
	test := func(name string, view *stubView) {
		t.Run(name, func(t *testing.T) {
			m := New(nil, nil, []View{view})
			m.width, m.height = termWidth, termHeight
			out := m.View()
			assert.Equal(t, termHeight, lipgloss.Height(out), "frame height")
			assert.LessOrEqual(t, lipgloss.Width(out), termWidth, "frame width")
		})
	}
	test("view renders far too many rows", &stubView{title: "Over", rows: 200, width: 40})
	test("view renders too few rows", &stubView{title: "Under", rows: 1, width: 40})
	test("view renders lines wider than the terminal", &stubView{title: "Wide", rows: 5, width: 500})
	test("view renders nothing", &stubView{title: "Empty", rows: 0, width: 0})
}

// TestFrameNeverOutgrowsTheTerminal pushes the view's budget as low as it goes
// and checks nothing overflows when it gets there.
//
// The budget has a floor of one row, and a floor is only safe if the frame
// never needs to go below it. It did: full help on a node's detail screen lists
// ten keys in one column, so at the twelve-row minimum the footer took ten,
// the budget hit the floor, and the frame came out thirteen rows tall. This
// sweeps every height from one row up, at widths below, at, and above the
// minimum, over every tab, the detail screen, full help, and the banner.
func TestFrameNeverOutgrowsTheTerminal(t *testing.T) {
	withReleaseVersion(t, "0.91.7")
	screens := func() []struct {
		name  string
		build func() Model
	} {
		type screen = struct {
			name  string
			build func() Model
		}
		out := make([]screen, 0, 8)
		for i, v := range defaultViews(nil) {
			out = append(out, screen{v.Title(), func() Model {
				m := New(nil, nil, defaultViews(nil))
				m.selectTab(i)
				return m
			}})
		}
		for _, pane := range []detailPane{detailEngines, detailModels} {
			out = append(out, screen{"node detail", func() Model {
				nodes := newNodesView(nil)
				nodes.feeds.discovered = []availableNode{{HostUUID: "u1", Name: "host", IPAddress: "10.0.0.1", Port: 1}}
				nodes.feeds.selfUUID = "u1"
				nodes.rebuild()
				m := New(nil, nil, []View{nodes})
				nodes.openDetail()
				nodes.detail.pane = pane
				return m
			}})
		}
		return out
	}

	for _, s := range screens() {
		for _, w := range []int{minTerminalWidth - 1, minTerminalWidth, 80, 160} {
			for h := 1; h <= 44; h++ {
				for _, fullHelp := range []bool{false, true} {
					for _, banner := range []bool{false, true} {
						m := s.build()
						m.width, m.height = w, h
						m.help.Width = w
						m.showFullHelp = fullHelp
						if banner {
							m = send(m, updateCheckMsg{latest: "0.92.0"})
						}
						m.resizeViews()
						out := m.View()
						require.Equal(t, h, lipgloss.Height(out), "%s at %dx%d (full help %v, banner %v)", s.name, w, h, fullHelp, banner)
						require.LessOrEqual(t, lipgloss.Width(out), w, "%s at %dx%d (full help %v, banner %v)", s.name, w, h, fullHelp, banner)
					}
				}
			}
		}
	}
}

// TestMinimumWidthKeepsNavigationVisible checks the shell still shows how to
// move between tabs and how to leave at the narrowest supported width.
//
// At forty columns the padded tab bar was forty-six wide and lost its last tab
// off the right, and the footer, truncated from the right, ended at "shift+tab
// prev" — with quit and help both cut off.
func TestMinimumWidthKeepsNavigationVisible(t *testing.T) {
	m := New(nil, nil, defaultViews(nil))
	updated, _ := m.Update(tea.WindowSizeMsg{Width: minTerminalWidth, Height: minTerminalHeight})
	m = updated.(Model)

	lines := strings.Split(m.View(), "\n")
	assert.Contains(t, lines[1], "Logs", "last tab is hidden at minimum width")
	footer := lines[len(lines)-1]
	assert.Contains(t, footer, "quit", "footer at minimum width")
	assert.Contains(t, footer, "help", "footer at minimum width")

	// With an error count the names no longer fit, and every tab must still be
	// reachable by the number the bar shows for it.
	for _, v := range m.views {
		if e, ok := v.(*errorsView); ok {
			e.setErrors([]svcerrors.ServiceError{{ID: "a", Message: "x", Severity: "error"}, {ID: "b", Message: "y", Severity: "error"}})
		}
	}
	bar := strings.Split(m.View(), "\n")[1]
	require.LessOrEqual(t, lipgloss.Width(bar), minTerminalWidth, "tab bar: %q", bar)
	for i := range m.views {
		assert.Contains(t, bar, strconv.Itoa(i+1), "tab number at minimum width")
	}
	assert.Contains(t, bar, m.activeView().Title(), "the active tab is named at minimum width")
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
	withReleaseVersion(t, "0.91.7")

	for h := minTerminalHeight; h <= 44; h++ {
		for i := range defaultViews(nil) {
			m := newTestModel(defaultViews(nil)...)
			m.width, m.height = 80, h
			m.selectTab(i)

			plain := m.contentHeight()
			m = send(m, updateCheckMsg{latest: "0.92.0"})

			require.Equal(t, h, lipgloss.Height(m.View()), "height %d, tab %d: frame with the banner up", h, i+1)
			// One row taken, unless the budget had already bottomed out at its
			// floor of one — below that there is nothing left to give.
			withBanner := m.contentHeight()
			if plain > 1 {
				require.Equal(t, plain-1, withBanner, "height %d, tab %d: one row taken from budget", h, i+1)
			}
		}
	}
}

// TestLeavingATabReturnsItToItsOwnFirstScreen checks a drill-down does not
// outlive the visit that opened it.
//
// The Nodes tab replaces itself with one machine's detail screen. Left open,
// switching to another tab and back put the operator inside that machine
// again rather than on the list they asked for — the list one more keypress
// away, with nothing on screen explaining why.
func TestLeavingATabReturnsItToItsOwnFirstScreen(t *testing.T) {
	nodes := newNodesView(nil)
	nodes.feeds.discovered = []availableNode{
		{HostUUID: "u1", Name: "this-host", IPAddress: "10.0.0.1", Port: 1},
	}
	nodes.rebuild()

	m := newTestModel(nodes, &stubView{title: "Jobs", rows: 1})
	nodes.openDetail()
	require.NotNil(t, nodes.detail, "the detail screen did not open")

	m.selectTab(1)
	assert.Nil(t, nodes.detail, "leaving the tab left the drill-down open")

	// And the tab still works normally on return: opening one again, then
	// coming back to it directly, also lands on the list.
	m.selectTab(0)
	nodes.openDetail()
	m.selectTab(0)
	assert.Nil(t, nodes.detail, "re-selecting the tab did not return to the list")
}

// TestViewFrameHeightAcrossTerminalSizes checks the budget holds at the small
// sizes where the header, tab bar, and footer alone can exceed the terminal.
func TestViewFrameHeightAcrossTerminalSizes(t *testing.T) {
	for _, h := range []int{4, 5, 10, 24, 60} {
		m := New(nil, nil, []View{&stubView{title: "T", rows: 100, width: 10}})
		m.width, m.height = 80, h
		assert.Equal(t, h, lipgloss.Height(m.View()), "frame height")
	}
}

// TestViewBeforeFirstResize checks the shell renders a placeholder rather than a
// zero-sized frame before the terminal size arrives.
func TestViewBeforeFirstResize(t *testing.T) {
	m := New(nil, nil, []View{&stubView{title: "T", rows: 3, width: 10}})
	assert.Equal(t, "starting...", m.View(), "pre-resize placeholder")
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

	assert.Len(t, keys.Keys(), len(views), "jump digits match tabs")
	assert.Equal(t, "1-"+strconv.Itoa(len(views)), keys.Help().Key, "footer digit range")
}

// TestJumpDigitsLabelDegenerateCounts covers the label at the edges, since it is
// assembled rather than written out.
func TestJumpDigitsLabelDegenerateCounts(t *testing.T) {
	test := func(name string, tabs int, want string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, tabDigitsHelp(tabs))
		})
	}
	test("single tab", 1, "1")
	test("two tabs", 2, "1-2")
	test("five tabs", 5, "1-5")
	test("all digit keys", 9, "1-9")
}

// closingView records being released.
type closingView struct {
	stubView
	closed bool
}

func (c *closingView) close() { c.closed = true }

// TestViewsAreReleasedWithoutTheFinalModel checks cleanup reaches the views
// themselves. After a panic Bubble Tea's Run returns no model to release them
// through, which left a running demo's dispatcher processes behind.
func TestViewsAreReleasedWithoutTheFinalModel(t *testing.T) {
	holding := &closingView{stubView: stubView{title: "Jobs"}}
	closeViews([]View{&stubView{title: "Nodes"}, holding})
	assert.True(t, holding.closed, "a view holding children was not released")
}

// TestMoreTabsThanDigitsIsRefused checks a tenth tab fails loudly instead of
// quietly having no key. The tab set is fixed at build time, so this is a
// mistake in how the shell was put together, found on the first run.
func TestMoreTabsThanDigitsIsRefused(t *testing.T) {
	defer func() {
		assert.NotNil(t, recover(), "a tenth tab was accepted without a key to select it")
	}()
	newGlobalKeyMap(maxTabs + 1)
}

// TestTheShellFitsItsDigitKeys keeps the real tab set inside that limit, so the
// panic above is never what finds it.
func TestTheShellFitsItsDigitKeys(t *testing.T) {
	assert.LessOrEqual(t, len(defaultViews(nil)), maxTabs, "tabs have digit keys")
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
	assert.Equal(t, 2, m.active, "after '3'")
	press("1")
	assert.Equal(t, 0, m.active, "after '1'")
	// Out of range for three tabs: the selection must not move.
	press("9")
	assert.Equal(t, 0, m.active, "after out-of-range '9'")
}

// TestTabWrapsBothDirections checks prev from the first tab lands on the last
// rather than going negative.
func TestTabWrapsBothDirections(t *testing.T) {
	m := newTestModel(
		&stubView{title: "One", rows: 1},
		&stubView{title: "Two", rows: 1},
	)
	m.selectTab(-1)
	assert.Equal(t, 1, m.active, "selectTab(-1)")
	m.selectTab(2)
	assert.Equal(t, 0, m.active, "selectTab(2)")
}

// TestErrorTabLabelCarriesTheCount checks the tab bar is the error indicator, so
// nothing extra is needed to notice a problem from another tab.
func TestErrorTabLabelCarriesTheCount(t *testing.T) {
	v := newErrorsView(nil)

	assert.Equal(t, "Errors", v.Title(), "clean label")

	v.setErrors([]svcerrors.ServiceError{{ID: "a", Message: "boom", Severity: "error"}})
	assert.Equal(t, "Errors (1)", v.Title(), "label carries count")

	v.setErrors([]svcerrors.ServiceError{
		{ID: "a", Message: "boom", Severity: "error"},
		{ID: "b", Message: "meh", Severity: "warning"},
	})
	assert.Equal(t, "Errors (2)", v.Title(), "label carries updated count")

	// Clearing the last error takes the count away again.
	v.setErrors(nil)
	assert.Equal(t, "Errors", v.Title(), "bare title after clearing")
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
	require.Equal(t, 2, m.active, "digit 3 selects the errors tab")
	assert.Same(t, errors, m.activeView(), "active view is not the errors tab")

	// And tabbing away works, unlike an overlay that had to be dismissed.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	m = updated.(Model)
	assert.Equal(t, 0, m.active, "could not leave the errors tab with a digit")
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
	assert.Equal(t, m.height, lipgloss.Height(out), "frame height")
	assert.LessOrEqual(t, lipgloss.Width(out), m.width, "frame width")
}

func TestFitLines(t *testing.T) {
	assert.Equal(t, "a\nb", fitLines("a\nb\nc", 2), "truncate")
	assert.Equal(t, "a\n\n", fitLines("a", 3), "pad")
	assert.Equal(t, "a\nb", fitLines("a\nb", 2), "exact")
}
