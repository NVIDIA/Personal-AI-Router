// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/applog"
	"nvpair-shared/engines"
	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"
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
	for _, w := range serviceWorkers {
		for _, e := range engines.All() {
			assert.NotEqual(t, e.ComponentName(), w, "the broker reports proxy crashes against %q", engines.ProxyComponent)
		}
	}
	require.Contains(t, serviceWorkers, engines.ProxyComponent, "a proxy crash must have a worker row")

	// End to end through the real matcher, with the id the broker builds.
	v := newServiceView(nil)
	v.localNodeUUID = "self-uuid"
	v.rebuildCrashes([]svcerrors.ServiceError{{
		ID:      crashPrefix + engines.ProxyComponent,
		Message: "proxy crashed",
		NodeID:  "self-uuid",
	}})
	require.Contains(t, v.crashed, engines.ProxyComponent, "a proxy crash did not register")
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

	require.Contains(t, v.crashed, "scanner", "local UUID-stamped crash should be surfaced, not filtered as remote")
	require.NotContains(t, v.crashed, "proxy", "a peer's crash must be filtered out of the local service view")
}

// TestWorkersAreUnknownUntilTheyCanBeKnown checks the table does not read "ok"
// before there is evidence for it. A worker is ok only by the absence of its
// crash from a snapshot read with this machine's UUID, so until the service has
// answered, a snapshot has arrived, and the UUID is known, every row is "?".
func TestWorkersAreUnknownUntilTheyCanBeKnown(t *testing.T) {
	statuses := func(v *serviceView) map[string]bool {
		seen := map[string]bool{}
		for _, r := range v.workers.Rows() {
			seen[r[1]] = true
		}
		return seen
	}

	v := newServiceView(nil)
	v.refreshWorkers()
	got := statuses(v)
	assert.NotContains(t, got, "ok", "before anything was known")
	assert.Contains(t, got, "?", "before anything was known")

	// A peer's crash arrives before this machine's UUID does. It must not be
	// taken for this machine's.
	v.Update(servicePingMsg{version: "1"})
	v.Update(serviceErrorsLoadedMsg{errs: []svcerrors.ServiceError{
		{ID: crashPrefix + "scanner", Message: "x", NodeID: "peer-uuid"},
	}})
	got = statuses(v)
	assert.NotContains(t, got, "ok", "before the UUID was known")
	assert.NotContains(t, got, "DOWN", "before the UUID was known")

	v.Update(serviceNodeIDMsg{nodeUUID: "self-uuid"})
	got = statuses(v)
	assert.Contains(t, got, "ok", "once everything was known; the crash was a peer's")
	assert.NotContains(t, got, "DOWN", "once everything was known; the crash was a peer's")
	assert.NotContains(t, got, "?", "once everything was known; the crash was a peer's")
}

// TestServiceShowsACrashFromBeforeItStarted is the regression guard for a
// worker that had already crashed reading "ok".
//
// errors:update fires only on change, so a tab that learns crashes from pushes
// alone knew nothing of one recorded before it subscribed.
func TestServiceShowsACrashFromBeforeItStarted(t *testing.T) {
	crash := svcerrors.ServiceError{ID: crashPrefix + "scanner", Message: "scanner crashed"}

	v := newServiceView(nil)
	v.localNodeUUID = "self-uuid"
	v.Update(serviceErrorsLoadedMsg{errs: []svcerrors.ServiceError{crash}})
	require.Contains(t, v.crashed, "scanner", "a crash in the initial errors snapshot was not shown")

	// A push is a full snapshot and newer than any read in flight, so a late
	// initial reply must not bring back a crash the push has since cleared.
	v = newServiceView(nil)
	v.localNodeUUID = "self-uuid"
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "errors:update", Params: []byte(`[]`)}})
	v.Update(serviceErrorsLoadedMsg{errs: []svcerrors.ServiceError{crash}})
	assert.NotContains(t, v.crashed, "scanner", "a late initial read overwrote a newer errors:update")
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
		assert.Contains(t, listed, w, "supervised worker is shown in the service table")
	}
	assert.Len(t, serviceWorkers, len(supervised), "table lists every supervised worker")
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
		assert.NotEmpty(t, row[2], "the errors worker reads ok with no explanation that it cannot report its own crash")
		return
	}
	require.FailNowf(t, "error sink not present in the worker list", "%q", errorSinkWorker)
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
	require.FailNow(t, "no choice row found")
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

	assert.Nil(t, press(v, "enter"), "enter applied a change immediately instead of opening the picker")
	require.True(t, v.choosing, "picker did not open")
	assert.True(t, v.CapturingInput(), "picker does not own the keyboard; a digit would jump tabs mid-choice")

	// Every option must be visible while choosing.
	row := v.choiceRow()
	for _, level := range logLevelNames() {
		assert.Contains(t, row, level, "picker row shows every option")
	}
}

// TestLogLevelPickerOpensOnCurrentValue checks the highlight starts on the level
// in force, not always at the first option.
func TestLogLevelPickerOpensOnCurrentValue(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = slog.LevelWarn

	press(v, "enter")
	assert.Equal(t, "warn", logLevelNames()[v.choiceIdx], "picker opens on current level")
}

// TestLogLevelPickerNavigationClamps checks the highlight moves on both axes and
// stops at the ends rather than wrapping.
func TestLogLevelPickerNavigationClamps(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = applog.Levels[0]
	press(v, "enter")

	names := logLevelNames()
	press(v, "h")
	assert.Equal(t, 0, v.choiceIdx, "clamp at first option")
	press(v, "l")
	assert.Equal(t, names[1], names[v.choiceIdx], "after one step right")
	// Walk past the end.
	for range names {
		press(v, "j")
	}
	assert.Equal(t, len(names)-1, v.choiceIdx, "clamp at last option")
}

// TestLogLevelPickerCancels checks esc closes without applying anything.
func TestLogLevelPickerCancels(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = slog.LevelInfo
	press(v, "enter")
	press(v, "l") // highlight a different level

	assert.Nil(t, press(v, "esc"), "esc issued a command")
	assert.False(t, v.choosing, "esc left the picker open")
	assert.Equal(t, slog.LevelInfo, v.logLevel, "level changed despite cancelling")
}

// TestLogLevelPickerAppliesSelection checks committing a different option issues
// the change, and that re-picking the current one does not.
func TestLogLevelPickerAppliesSelection(t *testing.T) {
	v := newServiceView(nil)
	logLevelRow(t, v)
	v.logLevel = slog.LevelInfo

	press(v, "enter")
	press(v, "l")
	cmd := press(v, "enter")
	assert.NotNil(t, cmd, "committing a different level issued no command")
	assert.False(t, v.choosing, "picker stayed open after applying")

	// Re-selecting the level already in force is a no-op, not a redundant RPC.
	press(v, "enter")
	assert.Nil(t, press(v, "enter"), "re-selecting the current level issued a command")
}

// resetBroker answers the one call a reset makes, engine:uninstall-managed,
// with the supplied result. An empty result stands in for a backend that
// rejected the request. Every method it is asked for is reported on the
// returned channel, so a test can assert what the reset actually sent.
func resetBroker(t *testing.T, result string) (*rpc.Client, <-chan string) {
	t.Helper()
	c1, c2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		c1.Close()
		c2.Close()
	})
	client := rpc.NewClient(c1, c1)
	go client.Run(ctx)

	called := make(chan string, 8)
	broker := rpc.NewCodec(c2, c2)
	go func() {
		for {
			req, err := broker.Read()
			if err != nil {
				return
			}
			called <- req.Method
			reply := &rpc.Message{JSONRPC: "2.0", ID: req.ID}
			if req.Method == "engine:uninstall-managed" && result == "" {
				reply.Error = &rpc.RPCError{Code: -32000, Message: "engine manager unavailable"}
			} else if req.Method == "engine:uninstall-managed" {
				reply.Result = json.RawMessage(result)
			} else {
				reply.Result = json.RawMessage(`{}`)
			}
			assert.NoError(t, broker.Write(reply))
		}
	}()
	return client, called
}

func armedResetView(t *testing.T, client *rpc.Client) *serviceView {
	t.Helper()
	v := newServiceView(client)
	for i, it := range v.items {
		if it.destructive {
			v.cursor = i
			v.activate()
			return v
		}
	}
	require.FailNow(t, "no destructive row found")
	return nil
}

// TestResetRemovesManagedEnginesBeforeWiping pins the order a reset works in:
// the engines go first, through the backend, while the broker is still up, and
// only then does the shell get the request to quit and delete the data
// directory. Deleting the directory alone would miss an engine a vendor
// installer put in the user's home, which is where LM Studio lands.
//
// One call, not a loop: engine-manager owns which installs are PAIR's.
func TestResetRemovesManagedEnginesBeforeWiping(t *testing.T) {
	client, called := resetBroker(t, `{"engines":[
		{"engine":"ollama","removed":true},
		{"engine":"llamacpp","removed":false}
	]}`)
	v := armedResetView(t, client)

	cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd, "confirmation produced no command")
	// An engine PAIR did not install is skipped, not failed, so it does not
	// stand in the way of the wipe.
	_, ok := cmd().(wipeDataMsg)
	require.True(t, ok, "reset did not end in a data wipe request")

	select {
	case method := <-called:
		assert.Equal(t, "engine:uninstall-managed", method, "reset request")
	default:
		require.FailNow(t, "reset sent no request")
	}
	select {
	case method := <-called:
		assert.Failf(t, "reset sent a second request; the backend selects the engines", "method: %q", method)
	default:
	}
}

// TestResetStopsWhenAnEngineCannotBeRemoved is the guard for orphaning an
// engine. The wipe deletes the record that an engine is PAIR's, so wiping after
// a failed removal left its files on disk with nothing allowed to remove them.
func TestResetStopsWhenAnEngineCannotBeRemoved(t *testing.T) {
	client, _ := resetBroker(t, `{"engines":[
		{"engine":"ollama","removed":true},
		{"engine":"lmstudio","removed":false,"error":"locked"}
	]}`)
	v := armedResetView(t, client)

	cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd, "confirmation produced no command")
	halted, ok := cmd().(resetHaltedMsg)
	require.True(t, ok, "a failed engine removal still went on to wipe the data")
	assert.Equal(t, "could not remove lmstudio", halted.reason, "names the engine that could not be removed")

	v.Update(halted)
	assert.False(t, v.resetting, "a stopped reset still holds the keyboard")
	assert.False(t, v.CapturingInput(), "a stopped reset still holds the keyboard")
	assert.Equal(t, toastError, v.status.kind, "status reports why the reset stopped")
	assert.Contains(t, v.status.text, "could not remove lmstudio", "status reports why the reset stopped")
}

// TestResetStopsWhenTheEngineManagerCannotBeReached covers the backend being
// unreachable or rejecting the call, when nothing is known about which engines
// are still on disk.
func TestResetStopsWhenTheEngineManagerCannotBeReached(t *testing.T) {
	client, _ := resetBroker(t, "")
	v := armedResetView(t, client)

	cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd, "confirmation produced no command")
	_, ok := cmd().(resetHaltedMsg)
	assert.True(t, ok, "the reset wiped the data without knowing whether the engines were removed")
}

// TestResetHoldsTheKeyboard is the guard on quitting halfway. The confirmation
// is disarmed before the work starts, so without the reset holding input the
// shell's own `q` exits with the data directory intact — engines gone, data
// kept, the opposite of what the row promises.
func TestResetHoldsTheKeyboard(t *testing.T) {
	client, _ := resetBroker(t, `{"engines":[]}`)
	v := armedResetView(t, client)
	require.False(t, v.resetting, "arming alone should not start the reset")

	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})

	require.True(t, v.resetting, "confirming did not mark the reset as running")
	assert.True(t, v.CapturingInput(), "a reset in flight does not hold the keyboard, so q would quit halfway")
	// A second confirmation must not start a parallel removal.
	v.cursor = 0
	for i, it := range v.items {
		if it.destructive {
			v.cursor = i
		}
	}
	assert.Nil(t, v.runAction(v.cursor), "a second confirmation started another reset")
}

// TestResetRequiresConfirmation is the guard on the one irreversible action in
// the TUI: activating the row must only arm it, and only the confirmation key
// may fire it.
func TestResetRequiresConfirmation(t *testing.T) {
	resetIdx := -1
	client, _ := resetBroker(t, `{"engines":[]}`)
	v := newServiceView(client)
	for i, it := range v.items {
		if it.destructive {
			resetIdx = i
			break
		}
	}
	require.GreaterOrEqual(t, resetIdx, 0, "no destructive row found")

	v.cursor = resetIdx
	assert.Nil(t, v.activate(), "activating the reset row acted immediately instead of asking to confirm")
	require.Equal(t, resetIdx, v.confirming)

	// Any other key cancels.
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	assert.Equal(t, -1, v.confirming, "a non-confirming key left the action armed")

	// Re-arm, then confirm.
	v.cursor = resetIdx
	v.activate()
	cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	require.NotNil(t, cmd, "confirmation produced no command")
	_, ok := cmd().(wipeDataMsg)
	assert.True(t, ok, "confirmation did not request a data wipe")
}

// TestWipeRequestQuitsAndRecordsIntent checks the shell records the request and
// exits, leaving the deletion to the caller once the broker has stopped.
func TestWipeRequestQuitsAndRecordsIntent(t *testing.T) {
	m := newTestModel(&stubView{title: "T", rows: 1})
	updated, cmd := m.Update(wipeDataMsg{})
	got, ok := updated.(Model)
	require.True(t, ok, "model type changed")
	assert.True(t, got.wipeOnExit, "wipe intent not recorded")
	require.NotNil(t, cmd, "no quit command issued")
	_, isQuit := cmd().(tea.QuitMsg)
	assert.True(t, isQuit, "wipe request did not quit the program")
}

// TestWorkerTableIsStatic is the guard for a highlight nothing can move.
//
// bubbles highlights its cursor row regardless of focus, so a read-only table
// built the normal way advertises a selection that does not exist — and the
// worker table has no per-worker operation to select for.
func TestWorkerTableIsStatic(t *testing.T) {
	v := newServiceView(nil)
	assert.False(t, v.workers.Focused(), "worker table is focused but its keys are never routed to it")

	rows := []table.Row{{"scanner", "ok", ""}, {"proxy", "ok", ""}, {"errors", "ok", ""}}

	// The behavioural claim: a static table's cursor cannot be moved, so the
	// row it sits on is not a selection the operator can act on.
	static := newStaticTable(serviceWorkerColumns(60))
	static.SetHeight(4)
	static.SetRows(rows)
	before := static.Cursor()
	static, _ = static.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	static, _ = static.Update(tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, before, static.Cursor(), "static table cursor must not move")

	// An interactive table does move, which is what makes the distinction real
	// rather than a property every table happens to have.
	interactive := newTable(serviceWorkerColumns(60))
	interactive.SetHeight(4)
	interactive.SetRows(rows)
	interactive, _ = interactive.Update(tea.KeyMsg{Type: tea.KeyDown})
	assert.NotZero(t, interactive.Cursor(), "interactive table did not move; the test proves nothing")
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
	require.GreaterOrEqual(t, len(indents), 2, "at least two data rows")
	for i, got := range indents[1:] {
		assert.Equal(t, indents[0], got, "row %d must align with row 0", i+1)
	}
}

// TestWorkerTableOrdersCrashesFirst is the guard for the clipping bug behind the
// highlight: the table cannot be scrolled, so on a short terminal the tail is
// unreachable. Failures must never be the rows that get cut.
func TestWorkerTableOrdersCrashesFirst(t *testing.T) {
	v := newServiceView(nil)
	v.pinged, v.errsKnown, v.localNodeUUID = true, true, "self-uuid"
	// Pick a worker deliberately late in the declared order.
	lastWorker := serviceWorkers[len(serviceWorkers)-1]
	v.rebuildCrashes([]svcerrors.ServiceError{
		{ID: crashPrefix + lastWorker, Message: "it died"},
	})

	rows := v.workers.Rows()
	require.Len(t, rows, len(serviceWorkers))
	// The row shows the display label; the crash lookup keys on the process name.
	assert.Equal(t, workerLabel(lastWorker), rows[0][0], "crashed worker first")
	assert.Equal(t, "DOWN", rows[0][1], "first row status")
	for _, r := range rows[1:] {
		assert.NotEqual(t, "DOWN", r[1], "a crashed worker (%q) sorted below a healthy one", r[0])
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
	assert.Zero(t, v.hiddenWorkers(), "tall terminal must not hide workers")
	assert.NotContains(t, v.View(), "not shown", "roomy layout still claims workers are hidden")

	// Cramped: too short for eleven workers plus the configuration list, so
	// some rows are unreachable and the view must admit it.
	v.SetSize(80, 15)
	_ = v.View() // the table is sized at render time
	require.NotZero(t, v.hiddenWorkers(), "terminal with %d worker rows reports nothing hidden despite %d workers", visibleTableRows(v.workers.Height()), len(serviceWorkers))
	assert.Contains(t, v.View(), "not shown", "short layout hides workers without saying so")
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
	assert.Zero(t, v.hiddenWorkers(), "room for all workers plus the header")

	// One row short: exactly one worker must be reported hidden.
	v.workers.SetHeight(len(serviceWorkers) + tableHeaderRows - 1)
	assert.Equal(t, 1, v.hiddenWorkers(), "one row short")

	// The old arithmetic ignored the header and so reported 0 here.
	v.workers.SetHeight(len(serviceWorkers))
	assert.Equal(t, tableHeaderRows, v.hiddenWorkers(), "height equal to worker count; header occupies %d rows", tableHeaderRows)
}

// TestVisibleTableRows pins the header accounting the layout budgets depend on.
func TestVisibleTableRows(t *testing.T) {
	test := func(name string, height, want int) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, visibleTableRows(height))
		})
	}
	test("zero height", 0, 0)
	test("header only", 1, 0)
	test("header and border only", 2, 0)
	test("one data row", 3, 1)
	test("three data rows", 5, 3)
	test("eleven data rows", 13, 11)
}

var _ View = (*serviceView)(nil)
var _ inputCapturer = (*serviceView)(nil)
