// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// errorsView is the Errors tab: the service errors active across the cluster,
// by severity, age, and the machine that reported them.
//
// The usual path: the tab's label carries the count, so an error is noticed
// from any tab; open it, select an error to read its full message and the
// engine, operation, and model it concerns, and fix the cause. c clears this
// machine's own errors — a peer's is cleared on the machine that reported it —
// and an error still true is reported again by its producer.
//
// The rows are the broker's error datastore: the errors:get-initial snapshot,
// then each full-snapshot errors:update push.
type errorsView struct {
	client *rpc.Client
	table  table.Model
	errs   []svcerrors.ServiceError
	// pushed is whether an errors:update has arrived. Each is a full snapshot,
	// so once one has, the initial read is older than what is shown and must
	// not replace it.
	pushed bool
	// namer resolves the node UUID the broker stamps on each report. Without it
	// the NODE column showed the same random-looking id the jobs list did.
	namer         *nodeNamer
	status        toast
	width, height int
}

// errorsIdentityMsg carries this machine's identity for the NODE column.
type errorsIdentityMsg struct {
	id  clusterIdentity
	err error
}

// errorsLoadedMsg carries the result of errors:get-initial.
type errorsLoadedMsg struct {
	errs []svcerrors.ServiceError
	err  error
}

// errorsClearedMsg reports the outcome of an errors:clear request. The
// refreshed list arrives separately via an errors:update push.
type errorsClearedMsg struct {
	err error
}

var clearKey = key.NewBinding(
	key.WithKeys("c"),
	key.WithHelp("c", "clear selected"),
)

func newErrorsView(client *rpc.Client) *errorsView {
	v := &errorsView{client: client, width: defaultTableWidth, namer: newNodeNamer()}
	v.table = newTable(v.columns())
	return v
}

// Title carries the active error count, so the tab bar itself is the indicator
// and no separate badge is needed anywhere. Title is re-read every render, so
// the count follows the errors:update stream without extra plumbing.
func (v *errorsView) Title() string {
	if n := v.count(); n > 0 {
		return fmt.Sprintf("Errors (%d)", n)
	}
	return "Errors"
}

func (v *errorsView) Init() tea.Cmd {
	return tea.Batch(
		call(v.client, "errors:get-initial", nil, func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return errorsLoadedMsg{err: err}
			}
			var errs []svcerrors.ServiceError
			decodeOrLog("errors:get-initial", msg.Result, &errs)
			return errorsLoadedMsg{errs: errs}
		}),
		nodeIdentityCmd(v.client, func(id clusterIdentity, err error) tea.Msg {
			return errorsIdentityMsg{id: id, err: err}
		}),
	)
}

// SetSize records the budget and fixes the table's width. Its height is set in
// View, from the chrome actually being rendered — see fitTable.
func (v *errorsView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.table.SetWidth(w)
	v.table.SetColumns(v.columns())
}

// columns lays the table out so its minimums fit the narrowest supported
// terminal. The message is why the tab exists, so it takes most of any width
// beyond that; a node name gives way first. SEV fits the longest severity the
// producers send, "warning".
func (v *errorsView) columns() []table.Column {
	return layoutColumns(v.width, []column{
		fixedCol("SEV", 7),
		fixedCol("AGE", 6),
		flexCol("NODE", 8, 1),
		flexCol("MESSAGE", 10, 4),
	})
}

func (v *errorsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case errorsLoadedMsg:
		if msg.err != nil {
			v.status.error("failed to load errors: %s", msg.err)
			return nil
		}
		// The reply and the pushes reach this loop by different paths, so an
		// update can land first. It is the newer full snapshot, and the reply
		// would otherwise overwrite it with an older one.
		if !v.pushed {
			v.setErrors(msg.errs)
		}
		return nil

	case errorsClearedMsg:
		if msg.err != nil {
			v.status.error("clear failed: %s", msg.err)
		} else {
			// The row disappearing is the confirmation; the id is an internal
			// supervisor identifier and means nothing to a reader.
			v.status.ok("cleared")
		}
		return nil

	case errorsIdentityMsg:
		if msg.err == nil {
			v.namer.setSelf(msg.id)
			v.repaint()
		}
		return nil

	case TickMsg:
		// AGE is relative and errors:update only fires on change, so without
		// this a sticky error shows the age it had when first reported for the
		// rest of the session — reading as a brand-new failure.
		v.repaint()
		return nil

	case NotificationMsg:
		switch msg.Msg.Method {
		case "errors:update":
			var errs []svcerrors.ServiceError
			decodeOrLog(msg.Msg.Method, msg.Msg.Params, &errs)
			v.pushed = true
			v.setErrors(errs)
		case "discovery:nodes-changed":
			// The only source of the UUID-to-name mapping for the NODE column.
			var nodes []availableNode
			decodeOrLog(msg.Msg.Method, msg.Msg.Params, &nodes)
			v.namer.learnDiscovered(nodes)
			v.repaint()
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
	if len(v.errs) == 0 {
		v.status.info("no error selected")
		return nil
	}
	row := v.table.SelectedRow()
	if row == nil {
		return nil
	}
	idx := v.table.Cursor()
	if idx < 0 || idx >= len(v.errs) {
		return nil
	}
	e := v.errs[idx]

	// Clearing is delete-by-id on this node only. Cross-node propagation is
	// designed but not built — shared/errors documents ClearedBy as stamped for
	// it and ignored "for now" — so a peer's error is deleted here and restored
	// by the next sync from the node that owns it. Refusing beats reporting a
	// success that undoes itself a second later; the operator's real option is
	// to clear it where it came from.
	if v.namer.selfUUID == "" {
		v.status.info("still identifying this machine - try again in a moment")
		return nil
	}
	if !v.clearable(e) {
		v.status.error("%s reported this - clear it there; clearing here would not stick",
			v.namer.name(e.NodeID))
		return nil
	}

	return call(v.client, "errors:clear", svcerrors.ClearParams{ID: e.ID}, func(_ *rpc.Message, err error) tea.Msg {
		return errorsClearedMsg{err: err}
	})
}

// clearable reports whether clearing this entry will stick.
//
// Nothing is clearable until this machine's identity has arrived, because
// which errors are this machine's is exactly what is not yet known — and
// guessing "all of them" let a peer's error be cleared here and restored by
// the next sync. The read lands within the first moment of a session.
//
// An error with no node id predates attribution or comes from a producer that
// does not stamp one; it is treated as local, which is where it almost
// certainly came from and keeps the key working rather than refusing on a
// missing field.
func (v *errorsView) clearable(e svcerrors.ServiceError) bool {
	if v.namer.selfUUID == "" {
		return false
	}
	return e.NodeID == "" || e.NodeID == v.namer.selfUUID
}

// repaint rebuilds the rows from the errors already held, for a change in how
// they render rather than in what they are: a node's name learned, the clock
// moving on.
func (v *errorsView) repaint() { v.setErrors(v.errs) }

func (v *errorsView) setErrors(errs []svcerrors.ServiceError) {
	selected := v.selectedErrorID()
	v.errs = errs
	rows := make([]table.Row, 0, len(errs))
	for _, e := range errs {
		rows = append(rows, table.Row{
			severityLabel(e.Severity),
			ageLabel(e.Timestamp),
			v.namer.name(e.NodeID),
			e.Message,
		})
	}
	// Keep the highlight on the same error across a refresh. errors:update is a
	// full snapshot sorted by id, so a new error that sorts earlier shifts every
	// row below it — and c would then clear a different entry than the one on
	// screen. The cursor restore additionally covers startup, where the initial
	// list is empty and would otherwise leave c dead.
	v.table.SetRows(rows)
	v.restoreSelection(selected)
	restoreCursor(&v.table, len(rows))
}

// selectedErrorID identifies the highlighted error across a refresh.
func (v *errorsView) selectedErrorID() string {
	i := v.table.Cursor()
	if i < 0 || i >= len(v.errs) {
		return ""
	}
	return v.errs[i].ID
}

// restoreSelection puts the cursor back on an error after the list changed. One
// that has been cleared or resolved leaves the cursor where bubbles clamped it.
func (v *errorsView) restoreSelection(id string) {
	if id == "" {
		return
	}
	for i, e := range v.errs {
		if e.ID == id {
			v.table.SetCursor(i)
			return
		}
	}
}

func (v *errorsView) View() string {
	empty := ""
	context := ""
	if len(v.errs) == 0 {
		empty = statusOKStyle.Render("No active errors.")
	} else if detail := v.selectedContext(); detail != "" {
		context = footerStyle.Render(detail)
	}

	// The table takes what the context and status lines leave, so a selected
	// error with full context cannot push the status line off the frame.
	status := v.status.render()
	body := empty
	if len(v.errs) > 0 {
		if fitTable(&v.table, v.height, context, status) {
			body = v.table.View()
		} else {
			body = footerStyle.Render(fmt.Sprintf(
				"  (too little room to list %d errors)", len(v.errs)))
			// The context is the only thing here whose height comes from the
			// data rather than the layout: a long enough message wraps past the
			// whole budget. Bounded to what is left once the one-line body and
			// the status have taken theirs.
			context = clampLines(context, v.height-countLines(body)-countLines(status))
		}
	}
	return joinLines(body, context, status)
}

// selectedContext describes the highlighted error in more detail than its row.
//
// The producer stamps which engine, operation, and model a failure came from,
// and what it suggests doing about it, but only the message reached the screen —
// so "install failed" arrived with no way to tell which engine it referred to on
// a node running two. Shown for the selected row rather than as columns because
// the table has to stay readable on an 80-column terminal.
func (v *errorsView) selectedContext() string {
	i := v.table.Cursor()
	if i < 0 || i >= len(v.errs) {
		return ""
	}
	e := v.errs[i]

	// The message first, in full. The table hard-truncates its MESSAGE cell to
	// whatever width is left — about forty characters at eighty columns — and
	// this tab exists to show that message, so there has to be somewhere it can
	// be read in its entirety.
	lines := []string{indentWrap(e.Message, v.width)}

	parts := make([]string, 0, 4)
	if e.EngineType != "" {
		parts = append(parts, "engine "+engineDisplayName(e.EngineType))
	}
	if e.Operation != "" {
		parts = append(parts, "during "+e.Operation)
	}
	if e.ModelName != "" {
		parts = append(parts, "model "+e.ModelName)
	}
	// "none" is the producer saying there is nothing to do, which is not worth a
	// line; anything else is a hint the operator can act on.
	if e.Action != "" && e.Action != "none" {
		parts = append(parts, "suggested: "+e.Action)
	}
	if len(parts) > 0 {
		// Wrapped like the message. A model id plus an engine plus an operation
		// runs past eighty columns routinely, and the field that fell off the
		// end was the suggested action — the one thing here that tells the
		// operator what to do.
		lines = append(lines, indentWrap(strings.Join(parts, "   "), v.width))
	}
	return strings.Join(lines, "\n")
}

// clampLines truncates a rendered block to at most n rows, marking the cut.
func clampLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	lines = lines[:n]
	lines[n-1] = footerStyle.Render("  ...")
	return strings.Join(lines, "\n")
}

// indentWrap folds s onto lines no wider than width, indented by two spaces so
// it reads as detail belonging to the row above.
//
// Measured in display cells, not bytes, and it breaks a token that cannot fit
// on a line of its own. Both matter for what actually lands here: engine errors
// quote model ids, blob paths, and URLs, none of which contain a space, and a
// token longer than the terminal is exactly the case that defeated the purpose
// of showing the message in full — the shell truncates an over-wide line rather
// than wrapping it, so the tail was lost either way.
func indentWrap(s string, width int) string {
	const indent = "  "
	limit := width - lipgloss.Width(indent)
	if limit < 8 {
		limit = 8
	}

	var out []string
	line := ""
	flush := func() {
		if line != "" {
			out = append(out, indent+line)
			line = ""
		}
	}
	for _, word := range strings.Fields(s) {
		for lipgloss.Width(word) > limit {
			// Longer than a whole line: split it rather than emit an over-wide
			// row. The head goes out on its own so the break is visible.
			flush()
			head, rest := splitCells(word, limit)
			out = append(out, indent+head)
			word = rest
		}
		switch {
		case line == "":
			line = word
		case lipgloss.Width(line)+1+lipgloss.Width(word) <= limit:
			line += " " + word
		default:
			flush()
			line = word
		}
	}
	flush()
	return strings.Join(out, "\n")
}

// splitCells cuts s at the first n display cells, returning the head and the
// remainder. Cuts on rune boundaries so a multi-byte character is never halved.
func splitCells(s string, n int) (head, rest string) {
	used := 0
	for i, r := range s {
		w := lipgloss.Width(string(r))
		if used+w > n {
			if i == 0 {
				// A single rune wider than the whole allowance. Emit it anyway:
				// returning an empty head makes no progress, and the caller
				// loops until the word is consumed.
				_, size := utf8.DecodeRuneInString(s)
				return s[:size], s[size:]
			}
			return s[:i], s[i:]
		}
		used += w
	}
	return s, ""
}

func (v *errorsView) Help() []key.Binding {
	// Withdrawn on a peer's error rather than offered and refused. The footer is
	// the promise; a key that only ever answers "you cannot do that here" should
	// not be in it.
	if i := v.table.Cursor(); i >= 0 && i < len(v.errs) && !v.clearable(v.errs[i]) {
		return nil
	}
	return []key.Binding{clearKey}
}

// count is how many active errors the service is reporting, for the tab label.
func (v *errorsView) count() int { return len(v.errs) }

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
