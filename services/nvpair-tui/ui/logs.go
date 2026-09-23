// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// maxLogLines bounds the in-memory scrollback so a long-running session cannot
// grow without limit.
const maxLogLines = 5000

// logsView shows the service tree's stderr — the broker's own logs plus every
// worker's, prefixed — with a substring filter, a follow toggle, and a way to
// write the buffer out.
//
// Saving matters more here than in a desktop app. The graphical UI can open a
// log file in a file manager; over SSH there is no file manager, and the
// operator needs the lines somewhere they can be copied off the machine.
//
// The log level is not set here. It is a fleet-wide setting that belongs with
// the rest of the service configuration, and having it on a single keystroke
// next to the scroll keys made it far too easy to change the whole tree's
// verbosity by accident.
type logsView struct {
	client *rpc.Client
	vp     viewport.Model
	lines  []string

	filter      string
	filterInput textinput.Model
	editing     bool
	// follow pins the viewport to the newest line. Scrolling up is the normal
	// way to read back, so it releases automatically rather than fighting the
	// operator for control of the viewport.
	follow bool

	status toast
	ready  bool

	width, height int
}

type logsSavedMsg struct {
	path string
	err  error
}

var (
	logFilterKey = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter"))
	// t for tail, not f. The viewport binds f to page-down, and this is the one
	// view whose whole purpose is scrolling back through a buffer — so f threw
	// the operator to the bottom on the first press of the standard paging key.
	// tail is also the vocabulary anyone reaching for this already has.
	logFollowKey = key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "follow"))
	logSaveKey   = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "save to file"))
	logClearKey  = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "clear filter"))
)

func newLogsView(client *rpc.Client) *logsView {
	ti := textinput.New()
	ti.Placeholder = "substring to show (case-insensitive)"
	return &logsView{client: client, filterInput: ti, follow: true}
}

func (v *logsView) Title() string { return "Logs" }

func (v *logsView) Init() tea.Cmd { return nil }

func (v *logsView) SetSize(w, h int) {
	v.width, v.height = w, h
	// One row for the status/filter footer.
	vh := clampWidth(h-1, 1)
	if !v.ready {
		v.vp = viewport.New(w, vh)
		v.ready = true
	} else {
		v.vp.Width = w
		v.vp.Height = vh
	}
	v.render()
}

func (v *logsView) CapturingInput() bool { return v.editing }

func (v *logsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case LogLineMsg:
		v.lines = append(v.lines, msg.Line)
		if len(v.lines) > maxLogLines {
			v.lines = v.lines[len(v.lines)-maxLogLines:]
		}
		v.render()
		return nil

	case logsSavedMsg:
		if msg.err != nil {
			v.status.error("save failed: %s", msg.err)
		} else {
			v.status.ok("saved to %s", msg.path)
		}
		return nil

	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *logsView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.editing {
		switch msg.String() {
		case "enter":
			v.filter = strings.TrimSpace(v.filterInput.Value())
			v.editing = false
			v.filterInput.Blur()
			v.render()
			return nil
		case "esc":
			v.editing = false
			v.filterInput.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.filterInput, cmd = v.filterInput.Update(msg)
		return cmd
	}

	switch {
	case key.Matches(msg, logFilterKey):
		v.editing = true
		v.filterInput.SetValue(v.filter)
		v.filterInput.Focus()
		return textinput.Blink
	case key.Matches(msg, logFollowKey):
		v.follow = !v.follow
		if v.follow {
			v.vp.GotoBottom()
		}
		return nil
	case key.Matches(msg, logClearKey):
		v.filter = ""
		v.render()
		return nil
	case key.Matches(msg, logSaveKey):
		return v.saveCmd()
	}

	before := v.vp.YOffset
	var cmd tea.Cmd
	v.vp, cmd = v.vp.Update(msg)
	// Scrolling away from the bottom releases follow, so reading back does not
	// get yanked forward by the next log line.
	if v.vp.YOffset != before && !v.vp.AtBottom() {
		v.follow = false
	}
	return cmd
}

// visibleLines is the buffer after the filter, which is applied at render time
// so changing it re-reads the whole retained buffer rather than only new lines.
func (v *logsView) visibleLines() []string {
	if v.filter == "" {
		return v.lines
	}
	needle := strings.ToLower(v.filter)
	out := make([]string, 0, len(v.lines))
	for _, l := range v.lines {
		if strings.Contains(strings.ToLower(l), needle) {
			out = append(out, l)
		}
	}
	return out
}

func (v *logsView) render() {
	if !v.ready {
		return
	}
	v.vp.SetContent(strings.Join(v.visibleLines(), "\n"))
	if v.follow {
		v.vp.GotoBottom()
	}
}

// saveCmd writes the retained buffer to a timestamped file in the operator's
// home directory. The filter is deliberately not applied: a saved log is
// evidence, and silently omitting lines that did not match a transient filter
// would make it misleading.
func (v *logsView) saveCmd() tea.Cmd {
	lines := append([]string(nil), v.lines...)
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			return logsSavedMsg{err: err}
		}
		stamp := time.Now().Format("20060102-150405")
		body := strings.Join(lines, "\n") + "\n"

		// Never overwrite. The name is only precise to the second, and two saves
		// inside the same second is exactly what happens when an operator
		// captures evidence, changes a filter, and captures again — silently
		// destroying the first file is the opposite of what saving is for.
		// O_EXCL makes the check and the create one step, so a second save
		// cannot land between them.
		for attempt := 0; attempt < 100; attempt++ {
			name := fmt.Sprintf("nvpair-logs-%s.log", stamp)
			if attempt > 0 {
				name = fmt.Sprintf("nvpair-logs-%s-%d.log", stamp, attempt+1)
			}
			path := filepath.Join(home, name)
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			if err != nil {
				return logsSavedMsg{err: err}
			}
			_, writeErr := f.WriteString(body)
			closeErr := f.Close()
			if writeErr != nil {
				return logsSavedMsg{err: writeErr}
			}
			if closeErr != nil {
				return logsSavedMsg{err: closeErr}
			}
			return logsSavedMsg{path: path}
		}
		return logsSavedMsg{err: fmt.Errorf("could not find a free name for nvpair-logs-%s", stamp)}
	}
}

func (v *logsView) View() string {
	if !v.ready {
		return footerStyle.Render("starting...")
	}
	var b strings.Builder
	b.WriteString(v.vp.View())
	b.WriteByte('\n')

	if v.editing {
		b.WriteString("filter: " + v.filterInput.View())
		return b.String()
	}
	if s := v.status.render(); s != "" {
		b.WriteString(s)
		return b.String()
	}
	b.WriteString(footerStyle.Render(v.footerSummary()))
	return b.String()
}

func (v *logsView) footerSummary() string {
	follow := "off"
	if v.follow {
		follow = "on"
	}
	shown := len(v.visibleLines())
	if v.filter == "" {
		return fmt.Sprintf("%d lines   follow %s", shown, follow)
	}
	return fmt.Sprintf("%d of %d lines matching %q   follow %s",
		shown, len(v.lines), v.filter, follow)
}

func (v *logsView) Help() []key.Binding {
	if v.editing {
		return inputHelp("apply filter")
	}
	bindings := []key.Binding{logFilterKey, logFollowKey, logSaveKey}
	if v.filter != "" {
		bindings = append(bindings, logClearKey)
	}
	return bindings
}
