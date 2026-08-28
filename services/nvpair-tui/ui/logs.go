// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"

	"nvpair-shared/applog"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// maxLogLines bounds the in-memory scrollback so a long-running session
// can't grow without limit.
const maxLogLines = 5000

// logsView shows the broker's (and its workers') stderr in a scrollable
// pane and lets the operator change the fleet-wide log level live via
// log/set-level, which the broker fans out to every worker.
type logsView struct {
	client *rpc.Client
	vp     viewport.Model
	lines  []string
	status string
	ready  bool
}

type logLevelSetMsg struct {
	level string
	err   error
}

var (
	logDebugKey = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "debug"))
	logInfoKey  = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "info"))
	logWarnKey  = key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "warn"))
	logErrorKey = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "error"))
)

func newLogsView(client *rpc.Client) *logsView {
	return &logsView{client: client}
}

func (v *logsView) Title() string { return "Logs" }

func (v *logsView) Init() tea.Cmd { return nil }

func (v *logsView) SetSize(w, h int) {
	// Reserve one line for the status/level footer.
	vh := clampWidth(h-1, 1)
	if !v.ready {
		v.vp = viewport.New(w, vh)
		v.ready = true
	} else {
		v.vp.Width = w
		v.vp.Height = vh
	}
	v.vp.SetContent(strings.Join(v.lines, "\n"))
}

func (v *logsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case LogLineMsg:
		atBottom := v.vp.AtBottom()
		v.lines = append(v.lines, msg.Line)
		if len(v.lines) > maxLogLines {
			v.lines = v.lines[len(v.lines)-maxLogLines:]
		}
		v.vp.SetContent(strings.Join(v.lines, "\n"))
		if atBottom {
			v.vp.GotoBottom()
		}
		return nil

	case logLevelSetMsg:
		if msg.err != nil {
			v.status = "set-level failed: " + msg.err.Error()
		} else {
			v.status = "log level set to " + msg.level
		}
		return nil

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, logDebugKey):
			return v.setLevel("debug")
		case key.Matches(msg, logInfoKey):
			return v.setLevel("info")
		case key.Matches(msg, logWarnKey):
			return v.setLevel("warn")
		case key.Matches(msg, logErrorKey):
			return v.setLevel("error")
		}
		var cmd tea.Cmd
		v.vp, cmd = v.vp.Update(msg)
		return cmd
	}
	return nil
}

func (v *logsView) setLevel(level string) tea.Cmd {
	return call(v.client, applog.SetLevelMethod, applog.SetLevelParams{Level: level}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return logLevelSetMsg{err: err}
		}
		var r struct {
			Level string `json:"level"`
		}
		_ = decodeParams(msg.Result, &r)
		return logLevelSetMsg{level: r.Level}
	})
}

func (v *logsView) View() string {
	footer := footerStyle.Render("set fleet log level:  d debug  i info  w warn  e error")
	if v.status != "" {
		footer = footerStyle.Render(v.status) + "   " + footer
	}
	if !v.ready {
		return footer
	}
	return v.vp.View() + "\n" + footer
}

func (v *logsView) Help() []key.Binding {
	return []key.Binding{logDebugKey, logInfoKey, logWarnKey, logErrorKey}
}
