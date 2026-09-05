// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"time"

	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// errorsView shows the broker's service-error datastore: the initial
// snapshot from errors:get-initial plus every full-snapshot errors:update
// push. The user can clear the selected entry; note clears are in-memory
// in nvpair-errors and a fresh producer emit resurrects the entry.
type errorsView struct {
	client        *rpc.Client
	table         table.Model
	errs          []svcerrors.ServiceError
	status        string
	width, height int
}

// errorsLoadedMsg carries the result of errors:get-initial.
type errorsLoadedMsg struct {
	errs []svcerrors.ServiceError
	err  error
}

// errorsClearedMsg reports the outcome of an errors:clear request. The
// refreshed list arrives separately via an errors:update push.
type errorsClearedMsg struct {
	id  string
	err error
}

var clearKey = key.NewBinding(
	key.WithKeys("c"),
	key.WithHelp("c", "clear selected"),
)

func newErrorsView(client *rpc.Client) *errorsView {
	v := &errorsView{client: client}
	// Seed real columns up front (not nil): errors:get-initial can return
	// before the first tea.WindowSizeMsg reaches SetSize, and setErrors ->
	// table.SetRows on a zero-column table panics (index out of range) as
	// soon as the broker already has >=1 stored error at startup.
	v.table = newTable(v.columns())
	return v
}

func (v *errorsView) Title() string { return "Errors" }

func (v *errorsView) Init() tea.Cmd {
	return call(v.client, "errors:get-initial", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return errorsLoadedMsg{err: err}
		}
		var errs []svcerrors.ServiceError
		_ = decodeParams(msg.Result, &errs)
		return errorsLoadedMsg{errs: errs}
	})
}

func (v *errorsView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-1, 1))
	v.table.SetColumns(v.columns())
}

func (v *errorsView) columns() []table.Column {
	// Fixed columns first; Message takes whatever width is left.
	const sev, age, node = 9, 6, 16
	msg := clampWidth(v.width-sev-age-node-2, 10)
	return []table.Column{
		{Title: "SEV", Width: sev},
		{Title: "AGE", Width: age},
		{Title: "NODE", Width: node},
		{Title: "MESSAGE", Width: msg},
	}
}

func (v *errorsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case errorsLoadedMsg:
		if msg.err != nil {
			v.status = "failed to load errors: " + msg.err.Error()
			return nil
		}
		v.setErrors(msg.errs)
		return nil

	case errorsClearedMsg:
		if msg.err != nil {
			v.status = "clear failed: " + msg.err.Error()
		} else {
			v.status = "cleared " + msg.id
		}
		return nil

	case NotificationMsg:
		if msg.Msg.Method == "errors:update" {
			var errs []svcerrors.ServiceError
			_ = decodeParams(msg.Msg.Params, &errs)
			v.setErrors(errs)
		}
		return nil

	case tea.KeyMsg:
		if key.Matches(msg, clearKey) {
			return v.clearSelected()
		}
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	return nil
}

func (v *errorsView) clearSelected() tea.Cmd {
	row := v.table.SelectedRow()
	if row == nil {
		return nil
	}
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.errs) {
		return nil
	}
	id := v.errs[idx].ID
	return call(v.client, "errors:clear", svcerrors.ClearParams{ID: id}, func(_ *rpc.Message, err error) tea.Msg {
		return errorsClearedMsg{id: id, err: err}
	})
}

func (v *errorsView) setErrors(errs []svcerrors.ServiceError) {
	v.errs = errs
	rows := make([]table.Row, 0, len(errs))
	for _, e := range errs {
		rows = append(rows, table.Row{
			severityLabel(e.Severity),
			ageLabel(e.Timestamp),
			truncate(e.NodeID, 16),
			e.Message,
		})
	}
	v.table.SetRows(rows)
}

func (v *errorsView) View() string {
	if len(v.errs) == 0 {
		body := statusOKStyle.Render("No active errors.")
		if v.status != "" {
			body += "\n" + footerStyle.Render(v.status)
		}
		return body
	}
	out := v.table.View()
	if v.status != "" {
		out += "\n" + footerStyle.Render(v.status)
	}
	return out
}

func (v *errorsView) Help() []key.Binding {
	return []key.Binding{clearKey}
}

func severityLabel(s string) string {
	if s == "" {
		return "info"
	}
	return s
}

// ageLabel renders a millisecond epoch timestamp as a compact relative
// age (e.g. "12s", "5m", "3h").
func ageLabel(tsMillis int64) string {
	if tsMillis == 0 {
		return "-"
	}
	d := time.Since(time.UnixMilli(tsMillis))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
