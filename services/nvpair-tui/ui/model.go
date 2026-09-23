// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strconv"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// headerHeight and tabBarHeight are the fixed single-row bands above the
// content area. The footer's height is variable, so it is measured at render
// time rather than declared here.
const (
	headerHeight = 1
	tabBarHeight = 1
)

// Model is the root Bubble Tea model: a tab bar over a set of Views, a
// header showing broker status, and a footer of contextual help. It owns
// the broker notification loop and routes messages to the views.
type Model struct {
	client *rpc.Client
	logCh  <-chan string
	keys   globalKeyMap
	help   help.Model

	views  []View
	active int

	width, height int

	ready         bool
	brokerVersion string
	disconnected  bool
	showFullHelp  bool

	// updateLatest is a published release newer than this build, and
	// updateDismissed records the operator saying they have seen it.
	//
	// Shell state rather than a view's, because the notice is on every tab: an
	// operator who lives on Nodes or Jobs would never see it on the one screen
	// they have no reason to open.
	updateLatest    string
	updateDismissed bool

	// wipeOnExit records a confirmed reset request. The deletion itself happens
	// in the caller after the broker has stopped, because the workers hold those
	// files while it runs.
	wipeOnExit bool
}

// closer is a view holding something that outlives the update loop and has to
// be released on the way out — a spawned child, a file handle.
type closer interface{ close() }

// close releases every view that holds one. Called once the program loop has
// finished, so nothing can still be scheduled.
func (m Model) close() {
	for _, v := range m.views {
		if c, ok := v.(closer); ok {
			c.close()
		}
	}
}

// Outcome reports what the operator asked for on the way out, for work that can
// only be done once the service tree is down.
type Outcome struct {
	WipeData bool
}

// New builds the root model over a connected broker client, the broker's
// captured stderr line channel, and the set of views (tabs) to present,
// in tab order.
func New(client *rpc.Client, logCh <-chan string, views []View) Model {
	h := help.New()
	styleHelp(&h)
	return Model{
		client: client,
		logCh:  logCh,
		keys:   newGlobalKeyMap(len(views)),
		help:   h,
		views:  views,
	}
}

// Init starts each view and arms the broker notification + log loops and the
// shared render tick.
func (m Model) Init() tea.Cmd {
	// Both are nil when the check is disabled or the build is unstamped, and
	// tea.Batch drops nils, so this needs no guard.
	cmds := []tea.Cmd{waitForNotification(m.client), waitForLog(m.logCh), uiTick(),
		checkUpdateCmd(), updateCheckTickCmd()}
	for _, v := range m.views {
		if c := v.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		m.resizeViews()
		return m, nil

	case tea.KeyMsg:
		// Interrupt is never captured. Everything else may be, but a terminal
		// program that cannot be stopped with ctrl+c is broken, and a text field
		// has no business consuming it — "q" is a legitimate character to type
		// into a filter, ctrl+c is not.
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		// A view editing a text field (e.g. a port or PIN entry) captures
		// all keys, so global bindings like tab/q don't steal characters
		// mid-input.
		if v := m.activeView(); v != nil {
			if ic, ok := v.(inputCapturer); ok && ic.CapturingInput() {
				return m, v.Update(msg)
			}
		}
		switch {
		case key.Matches(msg, m.keys.Dismiss):
			// Only meaningful while the banner is up. Swallowed either way,
			// which is fine: no view binds it.
			if m.banner() != "" {
				m.updateDismissed = true
				m.resizeViews()
			}
			return m, nil
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Help):
			m.showFullHelp = !m.showFullHelp
			m.resizeViews()
			return m, nil
		case key.Matches(msg, m.keys.NextTab):
			m.selectTab(m.active + 1)
			return m, nil
		case key.Matches(msg, m.keys.PrevTab):
			m.selectTab(m.active - 1)
			return m, nil
		case key.Matches(msg, m.keys.JumpTab):
			// The binding only carries digits, so this parse cannot fail.
			n, err := strconv.Atoi(msg.String())
			if err == nil && n >= 1 && n <= len(m.views) {
				m.selectTab(n - 1)
			}
			return m, nil
		}
		// Anything else is for the active view only.
		if v := m.activeView(); v != nil {
			return m, v.Update(msg)
		}
		return m, nil

	case NotificationMsg:
		if msg.Msg.Method == "app:ready" {
			m.ready = true
			m.brokerVersion = readyVersion(msg.Msg)
		}
		cmds := m.broadcast(msg)
		cmds = append(cmds, waitForNotification(m.client))
		return m, tea.Batch(cmds...)

	case TickMsg:
		// The redraw is the point: views rendering relative ages or a
		// transient status refresh without holding any tick state.
		cmds := m.broadcast(msg)
		cmds = append(cmds, uiTick())
		return m, tea.Batch(cmds...)

	case updateCheckMsg:
		// A failure is dropped, not reported. Someone on a network that cannot
		// reach the feed does not need telling every six hours, and this is the
		// least important thing on the screen.
		if msg.err == nil && newerVersion(ProductVersion, msg.latest) {
			// A newer release than the one already announced un-dismisses the
			// banner: the operator acknowledged the previous version, not this
			// one, and a long-running session would otherwise never mention it.
			if msg.latest != m.updateLatest {
				m.updateLatest = msg.latest
				m.updateDismissed = false
				m.resizeViews()
			}
		}
		return m, nil

	case updateCheckDueMsg:
		return m, tea.Batch(checkUpdateCmd(), updateCheckTickCmd())

	case wipeDataMsg:
		m.wipeOnExit = true
		return m, tea.Quit

	case DisconnectedMsg:
		m.disconnected = true
		return m, nil

	case LogLineMsg:
		cmds := m.broadcast(msg)
		cmds = append(cmds, waitForLog(m.logCh))
		return m, tea.Batch(cmds...)

	case LogClosedMsg:
		return m, nil

	default:
		// Background work (RPC results, ticks, spinner frames) goes to
		// every view; each ignores messages it doesn't own.
		return m, tea.Batch(m.broadcast(msg)...)
	}
}

// View composes a frame of exactly m.height rows and m.width columns. The
// content region is forced to its allotted height and the whole frame is
// clamped to the terminal width, so a view that renders more rows than it was
// given — or a status line longer than the terminal — cannot push the footer
// off screen or leave the previous frame's tail behind after a resize.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting..."
	}
	if m.width < minTerminalWidth || m.height < minTerminalHeight {
		return m.tooSmallView()
	}
	// The content budget depends on the footer, and the footer's height is the
	// active view's own help — which changes with in-view state, not only with
	// the events resizeViews runs on. Switching the detail's pane in full-help
	// mode adds two bindings, so the budget shrank under a view still sized for
	// the old one and the shell then deleted the difference.
	//
	// Sizing here, from the same measurement the frame is built with, is what
	// makes the two agree by construction rather than by remembering to re-size
	// on every state change that might affect the footer.
	budget := m.contentHeight()
	body := ""
	if v := m.activeView(); v != nil {
		v.SetSize(m.width, budget)
		body = v.View()
	}
	// joinLines rather than a fixed slice: the banner is usually absent, and an
	// empty entry in a Join is still a blank row. It sits below the tab bar so
	// it reads as belonging to the whole window rather than to the active tab,
	// and above the content so it cannot be mistaken for a view's own status.
	frame := joinLines(
		m.headerView(),
		m.tabBarView(),
		m.banner(),
		fitLines(body, budget),
		m.footerView(),
	)
	return lipgloss.NewStyle().MaxWidth(m.width).Render(frame)
}

// The smallest terminal any tab is designed for. Below this there is no honest
// layout: the header, tab bar, and footer alone claim four rows, and a view
// still has a summary line, a heading, and a table to place in what is left.
//
// Saying so once, here, is better than making every view degrade separately.
// A view contorting itself into five rows produces something unreadable that
// still looks like it is working, and it puts a size nobody uses in the way of
// every layout decision. This is a single, legible answer instead.
const (
	minTerminalWidth  = 40
	minTerminalHeight = 12
)

// tooSmallView replaces the whole frame when the terminal cannot hold a tab.
func (m Model) tooSmallView() string {
	msg := fmt.Sprintf("Terminal too small - %dx%d needed, this one is %dx%d.",
		minTerminalWidth, minTerminalHeight, m.width, m.height)
	// No keypress needed: the resize itself repaints.
	frame := fitLines(statusErrStyle.Render(msg)+"\nResize the window to continue.", m.height)
	return lipgloss.NewStyle().MaxWidth(m.width).Render(frame)
}

// fitLines forces s to exactly n lines, dropping any excess and padding when
// short.
func fitLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	for len(lines) < n {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// selectTab moves to idx, wrapping at both ends, and re-sizes the views: the
// footer's height depends on the active view's bindings, so the content region
// can change size when the tab does.
func (m *Model) selectTab(idx int) {
	if len(m.views) == 0 {
		return
	}
	m.active = ((idx % len(m.views)) + len(m.views)) % len(m.views)
	m.resizeViews()
}

func (m Model) activeView() View {
	if m.active < 0 || m.active >= len(m.views) {
		return nil
	}
	return m.views[m.active]
}

// broadcast delivers a non-key message to every view, so background tabs stay
// current — and their labels with them — while another tab is on screen.
func (m *Model) broadcast(msg tea.Msg) []tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(m.views))
	for _, v := range m.views {
		if c := v.Update(msg); c != nil {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

// contentHeight is the number of rows left for the active view once the header,
// tab bar, and footer have taken theirs. The footer is measured rather than
// estimated: its height varies with the active view's bindings and with the
// full-help toggle, and guessing it was what let the frame overflow.
func (m Model) contentHeight() int {
	h := m.height - headerHeight - tabBarHeight - lipgloss.Height(m.footerView())
	// The banner is chrome like the rest, so the views have to be told about it
	// — a row added to the frame without coming out of the budget is a row the
	// shell then deletes from the bottom of whichever view is showing, and the
	// bottom is where every view keeps its messages.
	//
	// Measured, not assumed one: lipgloss.Height("") is 1, so an absent banner
	// would otherwise cost a row it never draws.
	if b := m.banner(); b != "" {
		h -= lipgloss.Height(b)
	}
	if h < 1 {
		h = 1
	}
	return h
}

// banner is the shell-wide notice row, or empty when there is nothing to say.
//
// On every tab, until dismissed, because the operator this is for is the one who
// never opens the Service tab. It names the versions and where to get the
// release, and offers no key to install it — this client cannot, see
// updatecheck.go.
func (m Model) banner() string {
	if m.updateLatest == "" || m.updateDismissed {
		return ""
	}

	const dismiss = "   ctrl+x to dismiss"
	// Assembled longest-first against the real width rather than written out
	// once. The frame is clamped to the terminal, so an over-long line loses its
	// tail — and the tail is the dismiss hint, the one part that has to survive,
	// being the only way to get rid of the banner. At 118 columns the full
	// sentence already lost its last character, and the URL alone is 52.
	//
	// The shortest option fits minTerminalWidth, so one of these always fits.
	for _, text := range []string{
		fmt.Sprintf(" PAIR %s is available (you have %s) - %s",
			m.updateLatest, ProductVersion, updateReleasesPage),
		fmt.Sprintf(" PAIR %s is available (you have %s)", m.updateLatest, ProductVersion),
		fmt.Sprintf(" PAIR %s is available", m.updateLatest),
		" Update available",
	} {
		if lipgloss.Width(text+dismiss) <= m.width {
			return statusOKStyle.Render(text + dismiss)
		}
	}
	return statusOKStyle.Render(dismiss)
}

func (m *Model) resizeViews() {
	contentH := m.contentHeight()
	for _, v := range m.views {
		v.SetSize(m.width, contentH)
	}
}

func (m Model) headerView() string {
	left := titleStyle.Render("NVPAIR")
	var status string
	switch {
	case m.disconnected:
		status = statusErrStyle.Render("service disconnected")
	case m.ready:
		status = statusOKStyle.Render(fmt.Sprintf("service ready  v%s", m.brokerVersion))
	default:
		status = footerStyle.Render("starting service...")
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(status)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + status
}

func (m Model) tabBarView() string {
	cells := make([]string, len(m.views))
	for i, v := range m.views {
		label := fmt.Sprintf("%d %s", i+1, v.Title())
		if i == m.active {
			cells[i] = tabActiveStyle.Render(label)
		} else {
			cells[i] = tabInactiveStyle.Render(label)
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

func (m Model) footerView() string {
	// JumpTab is included so the numbers on the tab bar are documented
	// somewhere; the bar promises a shortcut and nothing else mentioned it.
	global := []key.Binding{m.keys.NextTab, m.keys.PrevTab, m.keys.JumpTab, m.keys.Help, m.keys.Quit}

	// None of them while a view owns the keyboard. Each view narrows its own
	// help to enter and esc in that state, and the footer used to prepend the
	// globals anyway — so the composed line advertised five keys that no longer
	// reached the shell. In a port field the digits are what you are meant to
	// type, and pressing q put a q in the field rather than quitting.
	//
	// ctrl+c is deliberately not listed here or anywhere: it is handled ahead of
	// the capture check and always works, which is what makes withdrawing q safe.
	if v, ok := m.activeView().(inputCapturer); ok && v.CapturingInput() {
		global = nil
	}
	var viewKeys []key.Binding
	if v := m.activeView(); v != nil {
		viewKeys = v.Help()
	}
	if m.showFullHelp {
		return m.help.FullHelpView([][]key.Binding{global, viewKeys})
	}
	// Globals first. bubbles truncates the short help from the right once it
	// exceeds the terminal width, so whatever is last is what disappears — and
	// with the view's own verbs first, an eighty-column terminal dropped "q
	// quit" on every tab and "? help" on most. Losing a verb is recoverable
	// because "?" lists them all; losing the way out, and the key that would
	// have revealed it, is the one truncation that traps someone.
	return m.help.ShortHelpView(append(global, viewKeys...))
}

func readyVersion(msg *rpc.Message) string {
	var p struct {
		Version string `json:"version"`
	}
	_ = decodeParams(msg.Params, &p)
	return p.Version
}
