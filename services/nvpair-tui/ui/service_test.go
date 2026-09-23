// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"nvpair-shared/engines"
	svcerrors "nvpair-shared/errors"
)

// The Service tab's proxy row must key on the identity the broker actually
// stamps on a proxy crash.
//
// One nvpair-proxy process hosts every engine's facade under one supervisor, so
// the broker reports one crash for the process. Keying on the per-engine
// ComponentName is silent in both directions: the real crash entry matches no
// row, so a dead proxy is invisible in the one view meant to show worker
// liveness, and the per-engine rows can never leave "ok". Nothing else in the
// build catches that — the mismatch compiles and every other test passes — so
// this assertion is what makes the identity rule in nvpair-shared/engines
// enforceable rather than advisory.
func TestServiceProxyRowMatchesTheBrokerCrashIdentity(t *testing.T) {
	found := false
	for _, w := range serviceWorkers {
		if w == engines.ProxyComponent {
			found = true
		}
		for _, e := range engines.All() {
			if w == e.ComponentName() {
				t.Errorf("worker row %q keys on a per-facade identity; the broker reports proxy crashes against %q",
					w, engines.ProxyComponent)
			}
		}
	}
	if !found {
		t.Fatalf("no worker row keys on %q, so a proxy crash would have no row at all: %v",
			engines.ProxyComponent, serviceWorkers)
	}

	// End to end through the real matcher, with the id the broker builds.
	v := newServiceView(nil)
	v.localNodeUUID = "self-uuid"
	v.rebuildCrashes([]svcerrors.ServiceError{{
		ID:      crashPrefix + engines.ProxyComponent,
		Message: "proxy crashed",
		NodeID:  "self-uuid",
	}})
	if _, down := v.crashed[engines.ProxyComponent]; !down {
		t.Fatalf("a proxy crash did not register: %v", v.crashed)
	}
}

// TestRebuildCrashesFiltersByUUID: the worker table keeps only local-origin
// crashes, keyed on this host's stable UUID (the value the broker stamps on
// local reports). A peer's crash must be dropped, and a local UUID-stamped
// crash must NOT be misclassified as remote.
func TestRebuildCrashesFiltersByUUID(t *testing.T) {
	v := newServiceView(nil)
	v.localNodeUUID = "self-uuid"

	crash := func(worker, nodeID string) svcerrors.ServiceError {
		return svcerrors.ServiceError{ID: crashPrefix + worker, Message: worker + " crashed", NodeID: nodeID}
	}
	v.rebuildCrashes([]svcerrors.ServiceError{
		crash("scanner", "self-uuid"), // local crash — keep
		crash("proxy", "peer-uuid"),   // a peer's crash — drop
	})

	if _, down := v.crashed["scanner"]; !down {
		t.Fatal("local UUID-stamped crash should be surfaced, not filtered as remote")
	}
	if _, down := v.crashed["proxy"]; down {
		t.Fatal("a peer's crash must be filtered out of the local service view")
	}
}

// TestRebuildCrashesBeforeIdentity: before the local UUID resolves, all crashes
// are kept (fail-open) so the view isn't blank during startup.
func TestRebuildCrashesBeforeIdentity(t *testing.T) {
	v := newServiceView(nil)
	v.rebuildCrashes([]svcerrors.ServiceError{
		{ID: crashPrefix + "scanner", Message: "x", NodeID: "whatever-uuid"},
	})
	if _, down := v.crashed["scanner"]; !down {
		t.Fatal("crashes should be kept until the local UUID is known")
	}
}

// TestServiceListsEverySupervisedWorker is the regression guard for the reported
// gap: the table was missing the errors worker, and the scheduler too.
//
// The list is written out rather than derived from serviceWorkers, which would
// only compare that slice with itself. It is the second opinion: a worker the
// broker supervises and this table forgot is exactly the failure an operator
// cannot see, so the names are restated here deliberately.
func TestServiceListsEverySupervisedWorker(t *testing.T) {
	// The broker's supervisor names, which its crash ids are built from. The
	// proxy appears once, by process name: one nvpair-proxy hosts every
	// engine's facade, so there is one supervisor entry and one crash id.
	supervised := []string{
		"scanner", "node-info", engines.ProxyComponent, "workload-manager",
		"engine-manager", "manual-nodes", "settings", "cluster-manager",
		"scheduler", "errors",
	}
	listed := make(map[string]bool, len(serviceWorkers))
	for _, w := range serviceWorkers {
		listed[w] = true
	}
	for _, w := range supervised {
		if !listed[w] {
			t.Errorf("supervised worker %q is not shown in the service table", w)
		}
	}
	if len(serviceWorkers) != len(supervised) {
		t.Errorf("table lists %d workers, broker supervises %d", len(serviceWorkers), len(supervised))
	}
}

// TestErrorSinkRowExplainsItself checks the errors worker carries a caveat, so
// its unconditional "ok" is not read as confirmed liveness.
func TestErrorSinkRowExplainsItself(t *testing.T) {
	v := newServiceView(nil)
	v.refreshWorkers()

	for i, w := range serviceWorkers {
		if w != errorSinkWorker {
			continue
		}
		row := v.workers.Rows()[i]
		if row[2] == "" {
			t.Error("the errors worker reads ok with no explanation that it cannot report its own crash")
		}
		return
	}
	t.Fatalf("%q not present in the worker list", errorSinkWorker)
}

// logLevelRow finds the log level row and puts the cursor on it.
func logLevelRow(t *testing.T, v *serviceView) int {
	t.Helper()
	for i, it := range v.items {
		if it.kind == itemChoice {
			v.cursor = i
			return i
		}
	}
	t.Fatal("no choice row found")
	return -1
}

func press(v *serviceView, k string) tea.Cmd {
	if k == "enter" {
		return v.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if k == "esc" {
		return v.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	}
	return v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
}

// TestLogLevelOpensPicker checks enter presents the options rather than silently
// stepping to the next one.
func TestLogLevelOpensPicker(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)

	if cmd := press(v, "enter"); cmd != nil {
		t.Error("enter applied a change immediately instead of opening the picker")
	}
	if !v.choosing {
		t.Fatal("picker did not open")
	}
	if !v.CapturingInput() {
		t.Error("picker does not own the keyboard; a digit would jump tabs mid-choice")
	}

	// Every option must be visible while choosing.
	row := v.choiceRow()
	for _, level := range logLevels {
		if !contains(row, level) {
			t.Errorf("picker row %q omits %q", row, level)
		}
	}
}

// TestLogLevelPickerOpensOnCurrentValue checks the highlight starts on the level
// in force, not always at the first option.
func TestLogLevelPickerOpensOnCurrentValue(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = "warn"

	press(v, "enter")
	if got := logLevels[v.choiceIdx]; got != "warn" {
		t.Errorf("picker opened on %q, want the current level warn", got)
	}
}

// TestLogLevelPickerNavigationClamps checks the highlight moves on both axes and
// stops at the ends rather than wrapping.
func TestLogLevelPickerNavigationClamps(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = logLevels[0]
	press(v, "enter")

	press(v, "h")
	if v.choiceIdx != 0 {
		t.Errorf("moved before the first option to %d", v.choiceIdx)
	}
	press(v, "l")
	if got := logLevels[v.choiceIdx]; got != logLevels[1] {
		t.Errorf("after one step right = %q, want %q", got, logLevels[1])
	}
	// Walk past the end.
	for range logLevels {
		press(v, "j")
	}
	if v.choiceIdx != len(logLevels)-1 {
		t.Errorf("walked past the last option to %d", v.choiceIdx)
	}
}

// TestLogLevelPickerCancels checks esc closes without applying anything.
func TestLogLevelPickerCancels(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = "info"
	press(v, "enter")
	press(v, "l") // highlight a different level

	if cmd := press(v, "esc"); cmd != nil {
		t.Error("esc issued a command")
	}
	if v.choosing {
		t.Error("esc left the picker open")
	}
	if v.logLevel != "info" {
		t.Errorf("level changed to %q despite cancelling", v.logLevel)
	}
}

// TestLogLevelPickerAppliesSelection checks committing a different option issues
// the change, and that re-picking the current one does not.
func TestLogLevelPickerAppliesSelection(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = "info"

	press(v, "enter")
	press(v, "l")
	cmd := press(v, "enter")
	if cmd == nil {
		t.Error("committing a different level issued no command")
	}
	if v.choosing {
		t.Error("picker stayed open after applying")
	}

	// Re-selecting the level already in force is a no-op, not a redundant RPC.
	press(v, "enter")
	if cmd := press(v, "enter"); cmd != nil {
		t.Error("re-selecting the current level issued a command")
	}
}

// TestResetRequiresConfirmation is the guard on the one irreversible action in
// the TUI: activating the row must only arm it, and only the confirmation key
// may fire it.
func TestResetRequiresConfirmation(t *testing.T) {
	resetIdx := -1
	v := newServiceView(nil)
	for i, it := range v.items {
		if it.destructive {
			resetIdx = i
			break
		}
	}
	if resetIdx < 0 {
		t.Fatal("no destructive row found")
	}

	v.cursor = resetIdx
	if cmd := v.activate(); cmd != nil {
		t.Error("activating the reset row acted immediately instead of asking to confirm")
	}
	if v.confirming != resetIdx {
		t.Fatalf("confirming = %d, want %d", v.confirming, resetIdx)
	}

	// Any other key cancels.
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if v.confirming != -1 {
		t.Error("a non-confirming key left the action armed")
	}

	// Re-arm, then confirm.
	v.cursor = resetIdx
	v.activate()
	cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if cmd == nil {
		t.Fatal("confirmation produced no command")
	}
	if _, ok := cmd().(wipeDataMsg); !ok {
		t.Error("confirmation did not request a data wipe")
	}
}

// TestWipeRequestQuitsAndRecordsIntent checks the shell records the request and
// exits, leaving the deletion to the caller once the broker has stopped.
func TestWipeRequestQuitsAndRecordsIntent(t *testing.T) {
	m := newTestModel(&stubView{title: "T", rows: 1})
	updated, cmd := m.Update(wipeDataMsg{})
	got, ok := updated.(Model)
	if !ok {
		t.Fatal("model type changed")
	}
	if !got.wipeOnExit {
		t.Error("wipe intent not recorded")
	}
	if cmd == nil {
		t.Fatal("no quit command issued")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Error("wipe request did not quit the program")
	}
}

// TestWorkerTableIsStatic is the guard for a highlight nothing can move.
//
// bubbles highlights its cursor row regardless of focus, so a read-only table
// built the normal way advertises a selection that does not exist — and the
// worker table has no per-worker operation to select for.
func TestWorkerTableIsStatic(t *testing.T) {
	v := newServiceView(nil)
	if v.workers.Focused() {
		t.Error("worker table is focused but its keys are never routed to it")
	}

	rows := []table.Row{{"scanner", "ok", ""}, {"proxy", "ok", ""}, {"errors", "ok", ""}}

	// The behavioural claim: a static table's cursor cannot be moved, so the
	// row it sits on is not a selection the operator can act on.
	static := newStaticTable(serviceWorkerColumns(60))
	static.SetHeight(4)
	static.SetRows(rows)
	before := static.Cursor()
	static, _ = static.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	static, _ = static.Update(tea.KeyMsg{Type: tea.KeyDown})
	if static.Cursor() != before {
		t.Errorf("static table cursor moved from %d to %d", before, static.Cursor())
	}

	// An interactive table does move, which is what makes the distinction real
	// rather than a property every table happens to have.
	interactive := newTable(serviceWorkerColumns(60))
	interactive.SetHeight(4)
	interactive.SetRows(rows)
	interactive, _ = interactive.Update(tea.KeyMsg{Type: tea.KeyDown})
	if interactive.Cursor() == 0 {
		t.Error("interactive table did not move; the test proves nothing")
	}
}

// TestStaticTableRowsAlignExactly is the guard for the cursor row sitting one
// column right of the others.
//
// bubbles applies the Selected style to the already-assembled row, so giving it
// anything with padding indents row 0 relative to its neighbours — which looked
// like the first worker being mysteriously offset.
func TestStaticTableRowsAlignExactly(t *testing.T) {
	tbl := newStaticTable(serviceWorkerColumns(70))
	tbl.SetHeight(4)
	tbl.SetRows([]table.Row{
		{"scanner", "ok", ""},
		{"node-info", "ok", ""},
		{"proxy", "ok", ""},
	})

	var indents []int
	for _, line := range strings.Split(tbl.View(), "\n") {
		// Data rows only: skip the header and its border rule.
		if !strings.Contains(line, "ok") {
			continue
		}
		indents = append(indents, len(line)-len(strings.TrimLeft(line, " ")))
	}
	if len(indents) < 2 {
		t.Fatalf("found %d data rows, want at least 2", len(indents))
	}
	for i, got := range indents[1:] {
		if got != indents[0] {
			t.Errorf("row %d is indented %d columns, row 0 is indented %d; rows must align",
				i+1, got, indents[0])
		}
	}
}

// TestWorkerTableOrdersCrashesFirst is the guard for the clipping bug behind the
// highlight: the table cannot be scrolled, so on a short terminal the tail is
// unreachable. Failures must never be the rows that get cut.
func TestWorkerTableOrdersCrashesFirst(t *testing.T) {
	v := newServiceView(nil)
	// Pick a worker deliberately late in the declared order.
	lastWorker := serviceWorkers[len(serviceWorkers)-1]
	v.rebuildCrashes([]svcerrors.ServiceError{
		{ID: crashPrefix + lastWorker, Message: "it died"},
	})

	rows := v.workers.Rows()
	if len(rows) != len(serviceWorkers) {
		t.Fatalf("table has %d rows, want %d", len(rows), len(serviceWorkers))
	}
	// The row shows the display label; the crash lookup keys on the process name.
	if rows[0][0] != workerLabel(lastWorker) {
		t.Errorf("first row is %q, want the crashed %q", rows[0][0], workerLabel(lastWorker))
	}
	if rows[0][1] != "DOWN" {
		t.Errorf("first row status = %q", rows[0][1])
	}
	for _, r := range rows[1:] {
		if r[1] == "DOWN" {
			t.Errorf("a crashed worker (%q) sorted below a healthy one", r[0])
		}
	}
}

// TestWorkerTableReportsHiddenRows checks a terminal too short to show every
// worker says so, rather than silently dropping the tail of a table that cannot
// be scrolled.
func TestWorkerTableReportsHiddenRows(t *testing.T) {
	v := newServiceView(nil)
	v.refreshWorkers()

	// Roomy: nothing hidden, no note.
	v.SetSize(80, 40)
	if got := v.hiddenWorkers(); got != 0 {
		t.Errorf("tall terminal hides %d workers", got)
	}
	if strings.Contains(v.View(), "not shown") {
		t.Error("roomy layout still claims workers are hidden")
	}

	// Cramped: too short for eleven workers plus the configuration list, so
	// some rows are unreachable and the view must admit it.
	v.SetSize(80, 15)
	_ = v.View() // the table is sized at render time
	if v.hiddenWorkers() == 0 {
		t.Fatalf("terminal with %d worker rows reports nothing hidden despite %d workers",
			visibleTableRows(v.workers.Height()), len(serviceWorkers))
	}
	if !strings.Contains(v.View(), "not shown") {
		t.Errorf("short layout hides workers without saying so:\n%s", v.View())
	}
}

// TestHiddenWorkersAccountsForTheHeader is the regression guard for a count that
// read zero while rows were being clipped.
//
// bubbles' SetHeight sets the height of the whole table, header included, so
// only height-2 data rows are visible. Treating the height as a row count made
// the view claim everything fit while two workers were cut off a table that
// cannot be scrolled.
func TestHiddenWorkersAccountsForTheHeader(t *testing.T) {
	v := newServiceView(nil)
	v.refreshWorkers()

	// Exactly enough total height for the header plus every worker.
	v.workers.SetHeight(len(serviceWorkers) + tableHeaderRows)
	if got := v.hiddenWorkers(); got != 0 {
		t.Errorf("with room for all %d workers plus the header, hidden = %d",
			len(serviceWorkers), got)
	}

	// One row short: exactly one worker must be reported hidden.
	v.workers.SetHeight(len(serviceWorkers) + tableHeaderRows - 1)
	if got := v.hiddenWorkers(); got != 1 {
		t.Errorf("one row short reports %d hidden, want 1", got)
	}

	// The old arithmetic ignored the header and so reported 0 here.
	v.workers.SetHeight(len(serviceWorkers))
	if got := v.hiddenWorkers(); got != tableHeaderRows {
		t.Errorf("height equal to the worker count reports %d hidden, want %d "+
			"(the header occupies %d rows)", got, tableHeaderRows, tableHeaderRows)
	}
}

// TestVisibleTableRows pins the header accounting the layout budgets depend on.
func TestVisibleTableRows(t *testing.T) {
	cases := map[int]int{0: 0, 1: 0, 2: 0, 3: 1, 5: 3, 13: 11}
	for h, want := range cases {
		if got := visibleTableRows(h); got != want {
			t.Errorf("visibleTableRows(%d) = %d, want %d", h, got, want)
		}
	}
}

var _ View = (*serviceView)(nil)
var _ inputCapturer = (*serviceView)(nil)
