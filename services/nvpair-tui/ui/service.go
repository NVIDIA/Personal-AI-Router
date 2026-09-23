// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/engines"
	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// crashPrefix is the id prefix the broker stamps on its sticky "subprocess X
// exited unexpectedly" errors. Per-worker liveness is derived from the presence
// of these in the errors:update snapshot, because the broker exposes no
// dedicated worker-status RPC.
const crashPrefix = "supervisor:subprocess-crashed:"

// servicePollInterval is how often the broker is re-pinged for liveness+uptime.
const servicePollInterval = 5 * time.Second

// serviceWorkers are the workers the broker supervises, in the order the table
// lists them. The names are the supervisor names the broker builds its crash
// ids from, so every entry here must match one.
//
// The proxy appears once, as engines.ProxyComponent, because one nvpair-proxy
// process hosts every engine's facade under one supervisor — the broker reports
// one crash for the process, not one per engine. Keying a row on the per-engine
// ComponentName would be silent in both directions: the real crash entry would
// match no row, and the per-engine rows could never leave "ok". See the identity
// split in nvpair-shared/engines, and the test that enforces it below.
//
// "errors" is listed even though its own crash cannot be observed this way:
// nvpair-errors is the sink the crash reports are written to, so it cannot
// report its own death. It reads "ok" whether alive or dead, and the row says
// so — better than omitting a worker the operator never sees at all.
var serviceWorkers = []string{
	"scanner",
	"node-info",
	engines.ProxyComponent,
	"workload-manager",
	"engine-manager",
	"manual-nodes",
	"settings",
	"cluster-manager",
	"scheduler",
	"errors",
}

// errorSinkWorker is the one worker the crash feed cannot describe, called out
// in the table so "ok" is not read as confirmed liveness.
const errorSinkWorker = "errors"

// logLevels are the fleet-wide log levels, ordered least to most severe.
var logLevels = []string{"debug", "info", "warn", "error"}

// serviceItemKind is what activating a row does.
type serviceItemKind int

const (
	// itemText opens an inline editor.
	itemText serviceItemKind = iota
	// itemChoice opens a picker over a fixed set of values.
	itemChoice
	// itemAction runs a command, after a confirmation when destructive.
	itemAction
)

// serviceItem is one row of the configuration list.
type serviceItem struct {
	kind  serviceItemKind
	label string
	help  string
	// suffix is the settings/get-*/set-* method pair for a persisted node
	// setting. Empty for rows backed by something else.
	suffix string
	// destructive rows require an explicit confirmation keystroke.
	destructive bool
	// options are the values an itemChoice row offers, in the order the picker
	// presents them.
	options []string

	strV string
}

// serviceView is the service-wide surface: is the service healthy, where are its
// endpoints, and the settings and maintenance actions that apply to the whole
// node.
//
// It absorbs the former Overview, Proxies port control, and Settings tabs. Those
// were three tabs describing one thing — the state of the service on this
// machine, and splitting them meant worker health lived nowhere near the log
// level you would raise to diagnose it.
//
// Proxy ports are the exception: they moved on to each machine's node detail
// screen, beside the engine each one fronts.
type serviceView struct {
	client *rpc.Client

	workers table.Model
	items   []serviceItem
	cursor  int

	brokerVersion string
	uptime        time.Duration
	pingErr       error
	logLevel      string

	crashed map[string]svcerrors.ServiceError
	// localNodeUUID is this host's stable UUID. The broker stamps local-origin
	// reports with it, so a crash entry carrying a different node belongs to a
	// peer and must be ignored — errors:update is the full cross-node snapshot
	// when nvpair-errors runs with --peer-sync.
	localNodeUUID string
	// lastErrs is the most recent snapshot, retained so the crash table can be
	// re-filtered once the local UUID resolves.
	lastErrs []svcerrors.ServiceError

	input   textinput.Model
	editing bool
	// choosing is set while a picker is open over an itemChoice row, with
	// choiceIdx the highlighted option. A picker rather than a cycle: cycling
	// makes the operator guess what comes next, gives no way to back out once
	// started, and hides the full set of values from someone who has not
	// memorised it.
	choosing  bool
	choiceIdx int
	// confirming is the index of a destructive row awaiting its confirmation
	// keystroke, or -1.
	confirming int
	status     toast

	width, height int
}

type serviceTickMsg struct{}

type servicePingMsg struct {
	version string
	uptime  time.Duration
	err     error
}

type serviceNodeIDMsg struct {
	nodeUUID string
	err      error
}

type settingLoadedMsg struct {
	idx  int
	strV string
	err  error
}

type settingSavedMsg struct {
	idx int
	err error
}

type logLevelSetMsg struct {
	level string
	err   error
}

// wipeDataMsg asks the shell to quit and delete the per-user data directory once
// the service tree is down. The deletion cannot happen here: the workers still
// hold those files, and one shutting down afterwards would recreate what was
// just removed.
type wipeDataMsg struct{}

var (
	serviceUpKey       = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("up/k", "up"))
	serviceDownKey     = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("down/j", "down"))
	serviceActivateKey = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "change"))
	serviceConfirmKey  = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm"))

	// The picker is laid out horizontally, so both axes move the highlight —
	// whichever the operator reaches for.
	choicePrevKey   = key.NewBinding(key.WithKeys("left", "h", "up", "k"), key.WithHelp("←/→", "choose"))
	choiceNextKey   = key.NewBinding(key.WithKeys("right", "l", "down", "j"), key.WithHelp("→", "next"))
	choiceApplyKey  = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply"))
	choiceCancelKey = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel"))
)

func newServiceView(client *rpc.Client) *serviceView {
	v := &serviceView{
		client:     client,
		crashed:    map[string]svcerrors.ServiceError{},
		input:      textinput.New(),
		confirming: -1,
		logLevel:   "info",
		items: []serviceItem{
			// The proxy ports are deliberately not here. They belong
			// beside the engine each one fronts, on that machine's node detail
			// screen — keeping them here meant the two ports an operator has to
			// tell apart were configured in different places.
			{kind: itemChoice, label: "Service log level", options: logLevels,
				help: "applies to the whole service fleet"},
			// force-ports and cluster-auto-sync are deliberately absent. Both
			// are persisted by nvpair-node-settings but nothing currently acts
			// on them — the desktop app does not read them either — so offering
			// them would imply an effect they do not have.
			// Says "this machine" because it is stored per node and never
			// propagates: nvpair-node-settings holds it as display-only sugar
			// with no push and no peer sync, so each machine keeps its own
			// label. Sitting unqualified under a setting that announces it
			// "applies to the whole service fleet", it read as cluster-wide and
			// silently was not.
			{kind: itemText, label: "Cluster name", suffix: "cluster-friendly-name",
				help: "this machine's own label for the cluster - not shared with peers"},
			{kind: itemAction, label: "Reset all data and quit", destructive: true,
				help: "deletes settings, cluster identity, and pairing"},
		},
	}
	// Static: there is no per-worker operation, so a movable highlight would
	// promise a selection that does nothing.
	v.workers = newStaticTable(serviceWorkerColumns(defaultTableWidth))
	return v
}

// serviceWorkerColumns is the worker table's layout, shared by construction and
// resize so the two cannot drift.
func serviceWorkerColumns(w int) []table.Column {
	return layoutColumns(w, []column{
		fixedCol("WORKER", 18),
		fixedCol("STATUS", 6),
		flexCol("DETAIL", 10, 1),
	})
}

func (v *serviceView) Title() string { return "Service" }

func (v *serviceView) Init() tea.Cmd {
	cmds := []tea.Cmd{v.pingCmd(), v.tickCmd(), v.nodeIDCmd()}
	for i := range v.items {
		if c := v.loadCmd(i); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

func (v *serviceView) pingCmd() tea.Cmd {
	return call(v.client, "ping", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return servicePingMsg{err: err}
		}
		var r struct {
			Version  string `json:"version"`
			UptimeMS int64  `json:"uptime_ms"`
		}
		_ = decodeParams(msg.Result, &r)
		return servicePingMsg{version: r.Version, uptime: time.Duration(r.UptimeMS) * time.Millisecond}
	})
}

// nodeIDCmd resolves this host's UUID so the crash table can drop peer-origin
// entries from the cross-node errors:update snapshot.
func (v *serviceView) nodeIDCmd() tea.Cmd {
	return call(v.client, "cluster:get-node-id", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return serviceNodeIDMsg{err: err}
		}
		var id clusterIdentity
		_ = decodeParams(msg.Result, &id)
		return serviceNodeIDMsg{nodeUUID: id.NodeUUID}
	})
}

func (v *serviceView) tickCmd() tea.Cmd {
	return tea.Tick(servicePollInterval, func(time.Time) tea.Msg { return serviceTickMsg{} })
}

// logLevelCmd reads the current level when level is empty, otherwise sets it.
// The broker fans a set out to every worker.
func (v *serviceView) logLevelCmd(level string) tea.Cmd {
	if level == "" {
		// There is no getter in the applog contract, so the displayed level is
		// whatever this session last set, seeded from the service default.
		return nil
	}
	return call(v.client, applog.SetLevelMethod, applog.SetLevelParams{Level: level},
		func(msg *rpc.Message, err error) tea.Msg {
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

func (v *serviceView) loadCmd(idx int) tea.Cmd {
	it := v.items[idx]
	if it.suffix == "" {
		return nil
	}
	return call(v.client, "settings/get-"+it.suffix, nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return settingLoadedMsg{idx: idx, err: err}
		}
		var r struct {
			Value string `json:"value"`
		}
		_ = decodeParams(msg.Result, &r)
		return settingLoadedMsg{idx: idx, strV: r.Value}
	})
}

// SetSize records the budget and fixes the table's width. Its height is set in
// View, from the chrome actually being rendered — see fitTable.
func (v *serviceView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.workers.SetColumns(serviceWorkerColumns(w))
	v.workers.SetWidth(w)
}

// CapturingInput covers the picker and an armed confirmation as well as the text
// editor. The picker navigates with keys the shell also uses (the digits jump
// tabs), and an armed reset must answer the next key rather than let a tab
// switch leave it armed behind a prompt that is no longer on screen.
func (v *serviceView) CapturingInput() bool {
	return v.editing || v.choosing || v.confirming >= 0
}

func (v *serviceView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case serviceTickMsg:
		return tea.Batch(v.pingCmd(), v.tickCmd())

	case servicePingMsg:
		// Worker status is derived from reachability, so the table has to be
		// rebuilt whenever that changes in either direction.
		if (v.pingErr == nil) != (msg.err == nil) {
			defer v.refreshWorkers()
		}
		v.pingErr = msg.err
		if msg.err == nil {
			v.brokerVersion = msg.version
			v.uptime = msg.uptime
		}
		return nil

	case serviceNodeIDMsg:
		if msg.err == nil && msg.nodeUUID != "" {
			v.localNodeUUID = msg.nodeUUID
			v.rebuildCrashes(v.lastErrs)
		}
		return nil

	case settingLoadedMsg:
		if msg.err == nil {
			v.items[msg.idx].strV = msg.strV
		}
		return nil

	case settingSavedMsg:
		if msg.err != nil {
			v.status.error("save failed: %s", msg.err)
			return nil
		}
		v.status.ok("%s saved", v.items[msg.idx].label)
		return v.loadCmd(msg.idx)

	case logLevelSetMsg:
		if msg.err != nil {
			v.status.error("log level change failed: %s", msg.err)
			return nil
		}
		v.logLevel = msg.level
		v.status.ok("log level set to %s for every service", msg.level)
		return nil

	case NotificationMsg:
		if msg.Msg.Method == "errors:update" {
			var errs []svcerrors.ServiceError
			_ = decodeParams(msg.Msg.Params, &errs)
			v.rebuildCrashes(errs)
		}
		return nil

	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *serviceView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.editing {
		switch msg.String() {
		case "enter":
			return v.submitEdit()
		case "esc":
			v.editing = false
			v.input.Blur()
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}

	if v.choosing {
		switch {
		case key.Matches(msg, choicePrevKey):
			v.moveChoice(-1)
		case key.Matches(msg, choiceNextKey):
			v.moveChoice(1)
		case key.Matches(msg, choiceApplyKey):
			return v.commitChoice()
		case key.Matches(msg, choiceCancelKey):
			v.closeChoice()
		}
		return nil
	}

	// A destructive row asks for one more keystroke. Anything other than the
	// confirmation cancels, so a stray key never triggers it.
	if v.confirming >= 0 {
		idx := v.confirming
		v.confirming = -1
		if key.Matches(msg, serviceConfirmKey) {
			return v.runAction(idx)
		}
		v.status.info("cancelled")
		return nil
	}

	switch {
	case key.Matches(msg, serviceUpKey):
		if v.cursor > 0 {
			v.cursor--
		}
	case key.Matches(msg, serviceDownKey):
		if v.cursor < len(v.items)-1 {
			v.cursor++
		}
	case key.Matches(msg, serviceActivateKey):
		return v.activate()
	}
	return nil
}

func (v *serviceView) activate() tea.Cmd {
	it := &v.items[v.cursor]
	switch it.kind {
	case itemChoice:
		v.openChoice(it)
		return nil

	case itemText:
		v.beginEdit(it.strV, it.label, 0)
		return textinput.Blink

	case itemAction:
		if it.destructive {
			v.confirming = v.cursor
			v.status.arm("%s - press y to confirm, any other key to cancel", it.label)
			return nil
		}
		return v.runAction(v.cursor)
	}
	return nil
}

func (v *serviceView) beginEdit(value, placeholder string, limit int) {
	v.editing = true
	v.input.SetValue(value)
	v.input.Placeholder = placeholder
	v.input.CharLimit = limit
	v.input.Focus()
}

func (v *serviceView) submitEdit() tea.Cmd {
	v.editing = false
	v.input.Blur()
	idx := v.cursor
	it := v.items[idx]
	val := strings.TrimSpace(v.input.Value())

	return call(v.client, "settings/set-"+it.suffix, map[string]string{"value": val},
		func(_ *rpc.Message, err error) tea.Msg {
			return settingSavedMsg{idx: idx, err: err}
		})
}

// runAction performs a confirmed action row.
func (v *serviceView) runAction(idx int) tea.Cmd {
	if v.items[idx].destructive {
		return func() tea.Msg { return wipeDataMsg{} }
	}
	return nil
}

// openChoice opens the picker on the row's current value, so the highlighted
// option is the one in force rather than always the first.
func (v *serviceView) openChoice(it *serviceItem) {
	if len(it.options) == 0 {
		return
	}
	v.choosing = true
	v.choiceIdx = indexOf(it.options, v.itemValue(*it))
	v.SetSize(v.width, v.height)
}

func (v *serviceView) closeChoice() {
	v.choosing = false
	v.SetSize(v.width, v.height)
}

// commitChoice applies the highlighted option. Only the log level is a choice
// row today, and it is applied through the broker's fleet-wide fan-out.
func (v *serviceView) commitChoice() tea.Cmd {
	it := v.items[v.cursor]
	v.closeChoice()
	if v.choiceIdx < 0 || v.choiceIdx >= len(it.options) {
		return nil
	}
	chosen := it.options[v.choiceIdx]
	if chosen == v.itemValue(it) {
		v.status.info("%s unchanged", it.label)
		return nil
	}
	return v.logLevelCmd(chosen)
}

// moveChoice steps the highlight by delta, clamping rather than wrapping so the
// ends of the list are felt.
func (v *serviceView) moveChoice(delta int) {
	next := v.choiceIdx + delta
	if next < 0 || next >= len(v.items[v.cursor].options) {
		return
	}
	v.choiceIdx = next
}

// indexOf finds value in options, returning 0 when it is absent so the picker
// always opens on a valid row.
func indexOf(options []string, value string) int {
	for i, o := range options {
		if o == value {
			return i
		}
	}
	return 0
}

func (v *serviceView) rebuildCrashes(errs []svcerrors.ServiceError) {
	v.lastErrs = errs
	crashed := map[string]svcerrors.ServiceError{}
	for _, e := range errs {
		// errors:update is the full cross-node snapshot in a cluster, so a
		// peer's crashed worker would otherwise paint this host's same-named
		// worker DOWN. An empty NodeID is treated as local.
		if v.localNodeUUID != "" && e.NodeID != "" && e.NodeID != v.localNodeUUID {
			continue
		}
		if strings.HasPrefix(e.ID, crashPrefix) {
			crashed[strings.TrimPrefix(e.ID, crashPrefix)] = e
		}
	}
	v.crashed = crashed
	v.refreshWorkers()
}

// refreshWorkers rebuilds the worker table, crashed workers first.
//
// The order matters because the table cannot be scrolled: on a short terminal
// the tail is clipped and unreachable. Putting failures at the top means the
// rows that are ever hidden are the ones reading "ok", which carry no
// information anyway.
func (v *serviceView) refreshWorkers() {
	down := make([]table.Row, 0, len(v.crashed))
	up := make([]table.Row, 0, len(serviceWorkers))
	for _, w := range serviceWorkers {
		if e, crashed := v.crashed[w]; crashed {
			down = append(down, table.Row{workerLabel(w), "DOWN", e.Message})
			continue
		}
		// A worker is only known good because the crash stream says nothing
		// about it — and that stream comes through the broker. With the broker
		// unreachable there is no such evidence, so claiming "ok" would be a
		// green wall directly under a red "service not responding", which reads
		// as the answer and is worse than admitting ignorance.
		if v.pingErr != nil {
			up = append(up, table.Row{workerLabel(w), "?", "unknown - the service is not responding"})
			continue
		}
		detail := ""
		if w == errorSinkWorker {
			detail = "reports other crashes; cannot report its own"
		}
		up = append(up, table.Row{workerLabel(w), "ok", detail})
	}
	v.workers.SetRows(append(down, up...))
}

// workerLabel is a worker's name in the vocabulary the rest of the interface
// uses.
//
// The identifiers are supervisor process names, and they collided with how the
// same components are named elsewhere: the proxy process serves the endpoints
// the Jobs tab calls "Ollama" and "LM Studio", so an operator reading
// "nvpair-proxy DOWN" here had no way to connect it to the endpoint they had
// just seen reported down there. The crash lookup still keys on the process
// name; only the display changes.
//
// One row, named for what its death costs, because one process hosts every
// engine's facade: when it dies, every endpoint goes with it.
func workerLabel(worker string) string {
	switch worker {
	case "scanner":
		return "node discovery"
	case "node-info":
		return "node telemetry"
	case engines.ProxyComponent:
		return "engine endpoints"
	case "workload-manager":
		return "job tracking"
	case "engine-manager":
		return "engines"
	case "manual-nodes":
		return "manual nodes"
	case "settings":
		return "node settings"
	case "cluster-manager":
		return "cluster"
	case "scheduler":
		return "scheduling"
	case errorSinkWorker:
		return "error reporting"
	}
	return worker
}

// hiddenWorkers is how many worker rows do not fit, for a note under the table.
//
// Measured against the visible data rows, not the table's total height: the
// header occupies two of them, so treating the height as a row count reported
// zero hidden while two workers were clipped off a table that cannot scroll.
func (v *serviceView) hiddenWorkers() int {
	// bubbles is asymmetric here: SetHeight takes the table's total height and
	// subtracts the header internally, while Height returns what is left — the
	// data rows. So this compares against Height directly; running it through
	// visibleTableRows would subtract the header a second time.
	hidden := len(v.workers.Rows()) - v.workers.Height()
	if hidden < 0 {
		return 0
	}
	return hidden
}

func (v *serviceView) View() string {
	summary := statusOKStyle.Render(fmt.Sprintf("service v%s  up %s",
		v.brokerVersion, v.uptime.Round(time.Second)))
	if v.pingErr != nil {
		summary = statusErrStyle.Render(
			"service not responding: " + v.pingErr.Error() +
				" - press 5 for Logs, or q to quit and restart nvpair")
	}

	if len(v.workers.Rows()) == 0 {
		v.refreshWorkers()
	}

	editor := ""
	if v.editing {
		editor = v.items[v.cursor].label + ": " + v.input.View()
	}
	if v.choosing {
		editor = v.choiceRow()
	}
	heading := titleStyle.Render("Configuration")
	items := v.itemList()
	status := v.status.render()

	// Sized like every other view: from the chrome actually being rendered.
	// This was the last one still subtracting a hand-maintained constant, and
	// on a short terminal it overran the budget — which cost the status line,
	// the row that carries "press y to confirm" for the data reset.
	//
	// The note's row is reserved before the table is sized, unconditionally, and
	// only its text is decided afterwards.
	//
	// It cannot be decided first: whether any worker is hidden depends on the
	// height fitTable is about to choose. An earlier version guessed, guessed
	// wrong between budgets 13 and 18 — the guess ignored the chrome above the
	// table — and then substituted the real note after sizing, so one unbudgeted
	// row appeared and the shell deleted the last line. That line is the status
	// row, which carries "press y to confirm" for the data reset: the operator
	// pressed enter on the one irreversible action, saw nothing change, and was
	// left armed with no prompt.
	//
	// Reserving a row that turns out to be unused costs nothing, because the
	// shell pads the frame. Adding one after sizing costs the bottom line.
	const noteRow = " "

	body := footerStyle.Render("  (too little room to list workers)")
	hiddenNote := ""
	if fitTable(&v.workers, v.height, summary, noteRow, heading, items, editor, status) {
		body = v.workers.View()
		if hidden := v.hiddenWorkers(); hidden > 0 {
			hiddenNote = footerStyle.Render(fmt.Sprintf(
				"  %d more worker(s) not shown - any that had crashed would be listed first",
				hidden))
		}
	}
	if hiddenNote == "" {
		// Keep the reserved row rather than reflowing: the reservation is what
		// makes the arithmetic hold.
		hiddenNote = noteRow
	}

	return joinLines(summary, body, hiddenNote, heading, items, editor, status)
}

// choiceRow renders the open picker: every option on one line with the
// highlighted one marked, so the full set is visible while choosing.
func (v *serviceView) choiceRow() string {
	it := v.items[v.cursor]
	cells := make([]string, 0, len(it.options))
	for i, o := range it.options {
		if i == v.choiceIdx {
			cells = append(cells, tabActiveStyle.Render(o))
			continue
		}
		cells = append(cells, tabInactiveStyle.Render(o))
	}
	return it.label + ":  " + strings.Join(cells, " ")
}

func (v *serviceView) itemList() string {
	lines := make([]string, 0, len(v.items))
	for i, it := range v.items {
		cursor := "  "
		if i == v.cursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%-26s %s", cursor, it.label, v.itemValue(it))
		if it.help != "" && i == v.cursor {
			line += footerStyle.Render("   " + it.help)
		}
		if i == v.cursor {
			line = titleStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (v *serviceView) itemValue(it serviceItem) string {
	switch it.kind {
	case itemChoice:
		return v.logLevel
	case itemAction:
		return ""
	default:
		if it.strV == "" {
			return footerStyle.Render("(unset)")
		}
		return it.strV
	}
}

func (v *serviceView) Help() []key.Binding {
	switch {
	case v.editing:
		// While the field has the keyboard, j and k type letters. The other
		// four text-field views already branch here; this was the last one
		// still advertising keys that no longer do what they say.
		return inputHelp("save")
	case v.confirming >= 0:
		return []key.Binding{serviceConfirmKey}
	case v.choosing:
		return []key.Binding{choicePrevKey, choiceApplyKey, choiceCancelKey}
	default:
		// Only the action, not the movement. Every other tab leaves arrow and
		// j/k navigation unadvertised — the four table tabs all do — and this
		// one listing it was the sole inconsistency, spending two footer slots
		// on the keys a user is least likely to need told. What is worth stating
		// is enter, because "this row does something" is not guessable.
		return []key.Binding{serviceActivateKey}
	}
}
