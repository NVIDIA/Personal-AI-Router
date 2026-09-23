// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// detailPane is which half of the node detail screen has the keyboard.
type detailPane int

const (
	detailEngines detailPane = iota
	detailModels
)

// detailInputMode is which text field, if any, is capturing keys.
type detailInputMode int

const (
	detailInputNone detailInputMode = iota
	detailInputModelName
	detailInputEnginePort
	detailInputProxyPort
)

// nodeDetail is the drill-down for one machine: its engines and the models each
// engine holds, with the operations that apply to them.
//
// It is a full-screen screen rather than a split pane under the table. Engines
// and models are both lists needing their own selection and their own verbs, and
// six rows at the bottom of a table cannot carry that without becoming a
// puzzle. It is also why the model list finally exists at all: the operations
// were always available on the broker, but there was nowhere to put them.
//
// Local and remote nodes differ in what they permit. The engine manager has
// remote install, start, stop, and the four model operations, but no remote
// restart, uninstall, or port change — those need process ownership on the
// target host — so those verbs are hidden rather than offered and then failed.
type nodeDetail struct {
	client *rpc.Client
	node   nodeRow

	engines     []engineStatus
	engineTable table.Model
	models      modelsResult
	modelTable  table.Model
	modelRows   []detailModelRow
	pane        detailPane
	// proxy carries the client-facing endpoint ports for this machine, so the
	// port a client connects to sits beside the engine it reaches. Left nil for
	// a peer, whose proxies we do not configure.
	proxy *proxyTracker

	// catalog is the open download browser, nil when it is closed. It replaces
	// the whole detail screen while up: it is a list needing its own search and
	// selection, which does not fit alongside two other tables.
	catalog *catalogBrowser

	// telemetry is the node's own hardware readout, polled directly over HTTP
	// because the broker does not carry it. telemetryOK records whether the last
	// poll succeeded, so an unreachable node reads as unavailable rather than as
	// a machine with no hardware.
	// pending is a destructive action waiting for confirmation. Deleting a model
	// and uninstalling an engine both throw away gigabytes that have to be
	// downloaded again, and both keys sit among the harmless ones — d beside
	// enter, u beside s and x — so a slip is easy and expensive.
	//
	// The target is captured here at arm time rather than re-read on confirm,
	// because this list re-sorts underneath the cursor whenever a download
	// finishes or a peer republishes its inventory.
	pending *pendingDestructive

	telemetry   nodeTelemetry
	telemetryOK bool
	// telemetryGen identifies this screen's polling chain. Bubble Tea cannot
	// cancel a pending tick, so a chain is retired by no longer matching it.
	telemetryGen int
	// telemetryRunning is whether a chain is in flight. A node with no known
	// address has nothing to poll and so no chain; one can start later.
	telemetryRunning bool
	// enginesStale marks the engine list as last-known rather than current,
	// because the most recent read of it failed. Held as state rather than
	// announced, since the read repeats on a timer.
	enginesStale bool

	input  textinput.Model
	mode   detailInputMode
	status toast

	width, height int
}

// detailModelRow is one row of the model list: a model and the engine serving it.
type detailModelRow struct {
	engine string
	model  string
	loaded bool
}

type detailEnginesMsg struct {
	engines []engineStatus
	err     error
}

type detailModelsMsg struct {
	models modelsResult
	err    error
}

var (
	detailBackKey    = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))
	detailPaneKey    = key.NewBinding(key.WithKeys("left", "right", "h", "l"), key.WithHelp("h/l", "pane"))
	detailInstallKey = key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "install"))
	detailStartKey   = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start"))
	detailStopKey    = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop"))
	detailRestartKey = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart"))
	detailUninstKey  = key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "uninstall"))
	detailConfirmKey = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm"))
	// In the engines pane only, so these do not collide with the models pane's
	// p (browse) or e (eject). Each is mnemonic where it applies: e for the
	// engine's own port, p for the proxy fronting it.
	detailPortKey       = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "engine port"))
	detailProxyKey      = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "proxy port"))
	detailPullKey       = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "browse models"))
	detailPullByNameKey = key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "download by name"))
	detailLoadKey       = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "load"))
	detailEjectKey      = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "eject"))
	detailDeleteKey     = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete"))
)

// telemetryChains numbers polling chains so a screen only continues its own.
// Package-level rather than per-screen because each detail screen is a new value
// and the point is to differ from every previous one.
var telemetryChains int

func newNodeDetail(client *rpc.Client, node nodeRow) *nodeDetail {
	ti := textinput.New()
	telemetryChains++
	d := &nodeDetail{
		client:       client,
		node:         node,
		engineTable:  newTable(detailEngineColumns(defaultTableWidth, !node.self)),
		modelTable:   newTable(detailModelColumns(defaultTableWidth)),
		input:        ti,
		telemetryGen: telemetryChains,
	}
	if node.self {
		d.proxy = newProxyTracker()
	}
	// A remote node's models are already in the discovery snapshot the broker
	// enriched, so the list is populated before any request completes.
	d.models = modelsResult{
		Models:         node.models,
		ModelsByEngine: node.modelsByEngine,
		LoadedByEngine: node.loadedByEngine,
	}
	d.refreshModels()
	return d
}

// detailEngineColumns is the engine table's layout.
//
// On this machine it carries both ports side by side. They are easy to confuse —
// ENGINE PORT is where the engine itself listens, PROXY PORT is where the proxy
// fronting that engine listens, which is the one clients connect to — and having
// them in two different places was exactly what made the distinction unclear.
// A peer's proxies are not ours to configure, so the column is local-only.
func detailEngineColumns(w int, remote bool) []table.Column {
	cols := []column{
		flexCol("ENGINE", 10, 1),
		fixedCol("INSTALLED", 9),
		fixedCol("RUNNING", 7),
		fixedCol("HEALTHY", 7),
		fixedCol("ENGINE PORT", 11),
	}
	if !remote {
		cols = append(cols, fixedCol("PROXY PORT", 10))
	}
	return layoutColumns(w, cols)
}

func detailModelColumns(w int) []table.Column {
	return layoutColumns(w, []column{
		flexCol("MODEL", 12, 2),
		fixedCol("ENGINE", 10),
		fixedCol("LOADED", 6),
	})
}

// remote reports whether this detail screen targets another machine, which
// decides both the RPC variants used and the verbs offered.
func (d *nodeDetail) remote() bool { return !d.node.self }

// nodeArg is the node parameter the remote engine methods take, empty for this
// machine so the local methods are used.
func (d *nodeDetail) nodeArg() string {
	if d.remote() {
		return d.node.key
	}
	return ""
}

func (d *nodeDetail) Init() tea.Cmd {
	// The telemetry chain is started by this first poll, not by a tick: each
	// reading schedules the next one when it lands, so there is exactly one
	// chain and it cannot outrun a slow node.
	cmds := []tea.Cmd{d.enginesCmd(), d.modelsCmd(), d.telemetryCmd()}
	if d.proxy != nil {
		cmds = append(cmds, d.proxy.init(d.client))
	}
	if d.remote() {
		cmds = append(cmds, detailEnginesTickCmd(d.telemetryGen))
	}
	return tea.Batch(cmds...)
}

// remoteEngineRefresh is how often an open remote detail re-reads the peer's
// engines. Slower than the telemetry poll because each read crosses the cluster
// to another machine, and engine lifecycle changes in seconds, not milliseconds.
const remoteEngineRefresh = 5 * time.Second

// detailEnginesTickMsg re-reads a remote node's engines. gen scopes it to one
// screen, exactly as the telemetry chain does.
type detailEnginesTickMsg struct{ gen int }

func detailEnginesTickCmd(gen int) tea.Cmd {
	return tea.Tick(remoteEngineRefresh, func(time.Time) tea.Msg {
		return detailEnginesTickMsg{gen: gen}
	})
}

func (d *nodeDetail) telemetryCmd() tea.Cmd {
	cmd := pollTelemetryCmd(d.node.key, d.telemetryGen, telemetryHosts(d.node), d.node.port)
	// Whether a chain is running, so a node whose address is not known yet can
	// have one started later. Nothing schedules a tick when there is nothing to
	// poll, and the reply is what continues the chain — so without this a manual
	// entry opened before its first probe landed showed "unavailable" forever,
	// even once discovery supplied an address.
	d.telemetryRunning = cmd != nil
	return cmd
}

func (d *nodeDetail) enginesCmd() tea.Cmd {
	method, params := "engine:get-installed", map[string]any{}
	if d.remote() {
		method = "engine:remote-get-installed"
		params["node"] = d.node.key
	}
	return call(d.client, method, params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return detailEnginesMsg{err: err}
		}
		var r struct {
			Engines []engineStatus `json:"engines"`
		}
		_ = decodeParams(msg.Result, &r)
		return detailEnginesMsg{engines: r.Engines}
	})
}

// modelsCmd asks the engine manager for this machine's inventory. A remote
// node's models come from discovery instead, so there is nothing to request.
func (d *nodeDetail) modelsCmd() tea.Cmd {
	if d.remote() {
		return nil
	}
	return call(d.client, "engine:models", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return detailModelsMsg{err: err}
		}
		var r modelsResult
		_ = decodeParams(msg.Result, &r)
		return detailModelsMsg{models: r}
	})
}

func (d *nodeDetail) SetSize(w, h int) {
	d.width, d.height = w, h
	d.engineTable.SetColumns(detailEngineColumns(w, d.remote()))
	d.engineTable.SetWidth(w)
	d.modelTable.SetColumns(detailModelColumns(w))
	d.modelTable.SetWidth(w)
	// The browser replaces this whole screen, so it has to follow a resize too;
	// sizing it only when it opens left it at its original dimensions.
	if d.catalog != nil {
		d.catalog.SetSize(w, h)
	}

	d.sizeEngineTable()
}

// maxEngineRows caps the engine list. There are two engines today, so this is
// headroom rather than a limit; the models list is what should grow.
const maxEngineRows = 6

// sizeEngineTable gives the engine list what it needs, bounded by the cap and
// by leaving the models list a usable minimum.
//
// It is budget-aware, not just content-aware. Sized purely from the engine count
// it was not chrome the models table could shrink against, so on a short
// terminal the two tables together overran the frame and the shell deleted the
// status line — the exact failure the render-time sizing was introduced to make
// impossible.
func (d *nodeDetail) sizeEngineTable() {
	want := clampWidth(len(d.engines)+tableHeaderRows, 1+tableHeaderRows)
	if want > maxEngineRows {
		want = maxEngineRows
	}
	// Everything the engine table must not crowd out: the fixed furniture, the
	// hardware block, and a minimal models table.
	reserved := detailChromeRows + d.hardwareHeight() + (1 + tableHeaderRows)
	if room := d.height - reserved; room < want {
		want = clampWidth(room, 1+tableHeaderRows)
	}
	d.engineTable.SetHeight(want)
}

// detailChromeRows is the fixed furniture on the detail screen: the identity
// line, the two section headings, and the blank line between the sections. The
// editor and status line come and go, so View measures those directly rather
// than reserving for them here.
const detailChromeRows = 4

// hardwareHeight is the rows the hardware block occupies, so the model table can
// be sized without rendering it first.
func (d *nodeDetail) hardwareHeight() int {
	if !d.telemetryOK {
		return 1
	}
	// Must agree with hardwareBlock, including its cap — a height that ignored
	// the cap would over-reserve, and one that ignored the truncation line would
	// under-reserve by exactly the row that says devices were dropped.
	lines := clampWidth(len(d.telemetry.summary()), 1)
	if max := d.hardwareBudget(); lines > max {
		return max
	}
	return lines
}

func (d *nodeDetail) CapturingInput() bool {
	if d.catalog != nil {
		// The browser owns every key while open, not only while its filter is
		// focused: it has its own navigation, so the shell must not act on any
		// of it. This is deliberately unconditional rather than delegated —
		// "is the filter focused" is a weaker question than the one being asked.
		return true
	}
	if d.pending != nil {
		// An armed destructive action answers the next key, whatever it is.
		// Without this the shell's own bindings still fired, so tab or a digit
		// switched away and left the action armed behind an off-screen prompt —
		// to be confirmed by whatever the operator pressed on returning.
		return true
	}
	return d.mode != detailInputNone
}

// update handles a message, returning a command and whether the detail screen
// should stay open.
func (d *nodeDetail) update(msg tea.Msg) (tea.Cmd, bool) {
	// The open browser owns the keyboard and its own load reply. Everything else
	// still reaches the panes underneath so their state is current on return.
	if d.catalog != nil {
		_, isKey := msg.(tea.KeyMsg)
		_, isLoad := msg.(catalogLoadedMsg)
		if isKey || isLoad {
			cmd, picked, stayOpen := d.catalog.update(msg)
			if !stayOpen {
				engine := d.catalog.engine
				d.catalog = nil
				d.SetSize(d.width, d.height)
				if picked != "" {
					return d.downloadFromCatalog(engine, picked), true
				}
			}
			return cmd, true
		}
	}

	switch msg := msg.(type) {
	case detailEnginesMsg:
		if msg.err != nil {
			// Said once, not once every five seconds. A peer that has gone away
			// fails this poll on every tick, and re-firing the toast pinned a
			// raw Go error to the status line and buried every other message
			// behind it. Recorded instead, and rendered as a note beside the
			// engine list, the way an unreachable node's telemetry already is.
			d.enginesStale = true
			return nil, true
		}
		d.enginesStale = false
		d.engines = msg.engines
		d.refreshEngines()
		d.SetSize(d.width, d.height)
		return nil, true

	case detailModelsMsg:
		if msg.err != nil {
			d.status.error("load models failed: %s", msg.err)
			return nil, true
		}
		d.models = msg.models
		d.refreshModels()
		return nil, true

	case engineOpMsg:
		// The engine is named in the outcome as well as in the request. On a
		// host running two, "start failed" alone does not say which one.
		what := msg.what
		if label := d.engineLabel(msg.engine); label != "" {
			what += " (" + label + ")"
		}
		switch {
		case msg.err != nil:
			d.status.error("%s failed: %s", what, msg.err)
		case msg.detached:
			d.status.info("%s is continuing in the background - watch for progress", what)
		default:
			d.status.ok("%s requested", what)
		}
		return nil, true

	case proxyStatusMsg:
		if d.proxy != nil {
			d.proxy.apply(msg)
			d.refreshEngines()
		}
		return nil, true

	case proxyPortMsg:
		switch {
		case msg.err != nil:
			d.status.error("%s proxy port change failed: %s", msg.label, msg.err)
		case msg.actual == 0:
			// No port in the reply. Say only what is known rather than inventing
			// a confirmation; the ready push will correct the table either way.
			d.status.info("%s proxy port change accepted", msg.label)
		case msg.actual != msg.requested:
			// The refusal, named. A running engine outranks the proxy for a
			// port, so this is the normal answer to asking for one an engine
			// holds — and it used to render as "updated".
			d.status.error("%s proxy stayed on :%d - :%d is taken, most likely by a running engine",
				msg.label, msg.actual, msg.requested)
		default:
			d.status.ok("%s proxy now on :%d", msg.label, msg.actual)
		}
		return nil, true

	case nodeTelemetryMsg:
		// Scoped to this screen's own chain, exactly as the tick is. This reply
		// schedules the next poll, so accepting a superseded one would leave two
		// chains running against the same node.
		if msg.nodeKey != d.node.key || msg.gen != d.telemetryGen {
			return nil, true
		}
		// A failed poll is expected for an unreachable node and is recorded
		// rather than reported: the panel says telemetry is unavailable, and no
		// error toast fires every two seconds.
		was := d.hardwareHeight()
		d.telemetryOK = msg.err == nil
		if msg.err == nil {
			d.telemetry = msg.telemetry
		}
		// The hardware block's height changes what the engine table may take.
		// The models table needs no such nudge — View measures the hardware
		// block directly every frame.
		if d.hardwareHeight() != was {
			d.sizeEngineTable()
		}
		// Schedule the next poll now that this one is done, so the interval is
		// the gap between polls rather than the gap between their starts and a
		// slow node cannot accumulate overlapping requests.
		return telemetryTickCmd(d.node.key, d.telemetryGen), true

	case detailEnginesTickMsg:
		// A peer's engine state has no push: engine:state-changed carries a
		// local snapshot with no node on it, so it cannot be attributed to
		// another machine and is correctly ignored below. Polling is the only
		// way an open remote detail reflects an engine starting or stopping
		// over there; without it the screen showed whatever was true when it
		// was opened, including right after an action taken from this screen.
		if msg.gen != d.telemetryGen || !d.remote() {
			return nil, true
		}
		return tea.Batch(d.enginesCmd(), detailEnginesTickCmd(d.telemetryGen)), true

	case nodeTelemetryTickMsg:
		// Only this screen's own chain continues. A tick from a previous visit
		// to the same node is dropped rather than extended, so re-opening a
		// detail screen cannot leave two chains polling in parallel.
		if msg.nodeKey != d.node.key || msg.gen != d.telemetryGen {
			return nil, true
		}
		// Only the poll. The next tick is scheduled when this one comes back —
		// see nodeTelemetryMsg — because a poll can take longer than the
		// interval: it tries each of the node's addresses in turn, so a
		// multi-homed peer with an unreachable address already exceeds two
		// seconds. Scheduling the next tick alongside the poll rather than after
		// it started a new one regardless, so those polls accumulated for as
		// long as the screen stayed open.
		return d.telemetryCmd(), true

	case NotificationMsg:
		return d.handleNotification(msg.Msg), true

	case tea.KeyMsg:
		return d.handleKey(msg)
	}
	return nil, true
}

func (d *nodeDetail) handleNotification(msg *rpc.Message) tea.Cmd {
	switch msg.Method {
	case "engine:state-changed":
		var e engineStatus
		_ = decodeParams(msg.Params, &e)
		if e.Engine == "" || d.remote() {
			return nil
		}
		for i, existing := range d.engines {
			if existing.Engine == e.Engine {
				d.engines[i] = e
				d.refreshEngines()
				return nil
			}
		}
		d.engines = append(d.engines, e)
		d.refreshEngines()
		// A new engine row changes the split between the two tables.
		d.sizeEngineTable()

	case "engine:models-changed":
		// The manager polls each running engine's resident set and pushes this
		// on any change — explicit load/unload, LM Studio's JIT auto-load, and
		// idle eviction alike. Re-reading is cheaper than merging the delta.
		if !d.remote() {
			return d.modelsCmd()
		}

	case "discovery:nodes-changed":
		// This is where a peer's model inventory lives: the broker enriches the
		// discovery snapshot with each node's per-engine models, and there is no
		// per-node models RPC to ask instead. The detail seeded itself from the
		// snapshot it was opened with and then never looked again, so a model
		// pulled or deleted on that peer — including by an action taken from
		// this very screen — did not appear until the operator backed out and
		// came back in.
		if !d.remote() {
			return nil
		}
		var nodes []availableNode
		_ = decodeParams(msg.Params, &nodes)
		for _, n := range nodes {
			if n.HostUUID != d.node.key && n.ID != d.node.key {
				continue
			}
			d.node.models = n.Models
			d.node.modelsByEngine = n.ModelsByEngine
			d.node.loadedByEngine = n.LoadedByEngine
			d.models = modelsResult{
				Models:         n.Models,
				ModelsByEngine: n.ModelsByEngine,
				LoadedByEngine: n.LoadedByEngine,
			}
			d.refreshModels()

			// Adopt an address learned since the screen opened, and start
			// polling if there was nothing to poll before. A manual entry is
			// routinely opened before discovery has resolved it.
			d.node.address = n.IPAddress
			d.node.addresses = candidateAddresses(n)
			if n.Port != 0 {
				d.node.port = n.Port
			}
			if !d.telemetryRunning {
				return d.telemetryCmd()
			}
			break
		}

	case "engine:install-progress":
		var p struct {
			Engine  string `json:"engine"`
			Stage   string `json:"stage"`
			Percent int    `json:"percent"`
		}
		_ = decodeParams(msg.Params, &p)
		// Sticky for the same reason the pull feed is: an engine install is a
		// multi-hundred-megabyte download, and between two frames more than six
		// seconds apart an expiring line leaves the screen looking idle.
		d.status.busy("install %s: %s (%d%%)", d.engineLabel(p.Engine), p.Stage, p.Percent)

	case "engine:pull-progress":
		// Local pulls only. The engine manager emits this for its own downloads
		// and the payload carries no node, so a peer's screen would attribute
		// this machine's download to that peer — and now that the note is
		// sticky, it would sit there for the life of the pull. A peer's
		// downloads arrive on engine:remote-progress, which does carry a node.
		if d.remote() {
			return nil
		}
		// An in-progress frame keeps the note sticky, so a download that stalls
		// leaves its last reported percentage on screen instead of the line
		// quietly expiring and making a stuck pull look like nothing happened.
		// Only the terminal frames — done, failed — go back to expiring.
		kind, format, args := progressToast(msg.Params, d.engineLabel)
		if kind == toastInfo {
			d.status.busy(format, args...)
			return nil
		}
		d.status.set(kind, format, args...)

	case "engine:remote-progress":
		var p struct {
			Node    string `json:"node"`
			Engine  string `json:"engine"`
			Op      string `json:"op"`
			Stage   string `json:"stage"`
			Percent int    `json:"percent"`
			Message string `json:"message"`
		}
		_ = decodeParams(msg.Params, &p)
		if p.Node == d.node.key {
			d.status.busy("%s %s: %s (%d%%)", p.Op, d.engineLabel(p.Engine), p.Stage, p.Percent)
		}

	default:
		// A proxy rebinding its listener changes the proxy port shown against
		// the engine it fronts.
		if d.proxy != nil {
			d.proxy.handleNotification(msg)
			d.refreshEngines()
		}
	}
	return nil
}

// progressToast renders an engine:pull-progress frame. Terminal stages carry no
// meaningful percent (success is implicitly complete, error uses -1), so they
// read as outcomes rather than a misleading "success (0%)". A late failure that
// arrives after the synchronous call timed out still surfaces here.
func progressToast(params []byte, label func(string) string) (toastKind, string, []any) {
	var p struct {
		Engine  string `json:"engine"`
		Stage   string `json:"stage"`
		Percent int    `json:"percent"`
		Message string `json:"message"`
	}
	_ = decodeParams(params, &p)
	switch p.Stage {
	case "success":
		return toastOK, "download %s: done", []any{label(p.Engine)}
	case "error":
		detail := p.Message
		if detail == "" {
			detail = "failed"
		}
		return toastError, "download %s failed: %s", []any{label(p.Engine), detail}
	default:
		return toastInfo, "download %s: %s (%d%%)", []any{label(p.Engine), p.Stage, p.Percent}
	}
}

func (d *nodeDetail) handleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if d.mode != detailInputNone {
		switch msg.String() {
		case "enter":
			return d.submitInput(), true
		case "esc":
			d.mode = detailInputNone
			d.input.Blur()
			return nil, true
		}
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return cmd, true
	}

	// An armed destructive action answers the next key, whatever it is, so the
	// confirmation cannot be skipped past by a navigation key.
	if d.pending != nil {
		return d.resolvePending(msg), true
	}

	if key.Matches(msg, detailBackKey) {
		return nil, false
	}
	if key.Matches(msg, detailPaneKey) {
		if d.pane == detailEngines {
			d.pane = detailModels
		} else {
			d.pane = detailEngines
		}
		return nil, true
	}

	if d.pane == detailEngines {
		return d.handleEngineKey(msg), true
	}
	return d.handleModelKey(msg), true
}

func (d *nodeDetail) handleEngineKey(msg tea.KeyMsg) tea.Cmd {
	engine := d.selectedEngine()
	switch {
	case key.Matches(msg, detailInstallKey):
		return d.lifecycle(engine, "install")
	case key.Matches(msg, detailStartKey):
		return d.lifecycle(engine, "start")
	case key.Matches(msg, detailStopKey):
		return d.lifecycle(engine, "stop")
	case key.Matches(msg, detailRestartKey):
		return d.lifecycle(engine, "restart")
	case key.Matches(msg, detailUninstKey):
		if d.remote() {
			// Refused before arming, not after. The footer hides this key on a
			// peer, but the handler still matched it — so pressing it staged a
			// gigabyte-destroying confirmation that could only ever answer that
			// the operation is unavailable. Its siblings all check first.
			d.status.error("uninstalling an engine is only available on the machine running it")
			return nil
		}
		return d.uninstallEngine(engine)
	case key.Matches(msg, detailPortKey):
		if engine == nil {
			d.status.error("no engine selected")
			return nil
		}
		if d.remote() {
			d.status.error("an engine's port can only be changed on the machine running it")
			return nil
		}
		d.mode = detailInputEnginePort
		d.input.Placeholder = "port"
		d.input.CharLimit = 5
		d.input.SetValue(strconv.Itoa(engine.Port))
		d.input.Focus()
		return textinput.Blink
	case key.Matches(msg, detailProxyKey):
		if engine == nil {
			d.status.error("no engine selected")
			return nil
		}
		if d.proxy == nil {
			d.status.error("a node's proxy ports can only be changed on that machine")
			return nil
		}
		idx := d.proxy.indexForEngine(engine.Engine)
		if idx < 0 {
			d.status.error("no proxy fronts %s", engine.label())
			return nil
		}
		port, _ := d.proxy.portForEngine(engine.Engine)
		d.mode = detailInputProxyPort
		d.input.Placeholder = "port"
		d.input.CharLimit = 5
		d.input.SetValue(strconv.Itoa(port))
		d.input.Focus()
		return textinput.Blink
	}
	var cmd tea.Cmd
	d.engineTable, cmd = d.engineTable.Update(msg)
	return cmd
}

func (d *nodeDetail) handleModelKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, detailPullKey):
		return d.openCatalog()
	case key.Matches(msg, detailPullByNameKey):
		engine := d.pullTargetEngine()
		if engine == "" {
			d.status.error("no engine available to download into")
			return nil
		}
		d.mode = detailInputModelName
		d.input.Placeholder = fmt.Sprintf("model name for %s (e.g. llama3.2)", d.engineLabel(engine))
		d.input.CharLimit = 0
		d.input.SetValue("")
		d.input.Focus()
		return textinput.Blink
	case key.Matches(msg, detailLoadKey):
		return d.modelOp("load")
	case key.Matches(msg, detailEjectKey):
		return d.modelOp("unload")
	case key.Matches(msg, detailDeleteKey):
		return d.deleteSelectedModel()
	}
	var cmd tea.Cmd
	d.modelTable, cmd = d.modelTable.Update(msg)
	return cmd
}

func (d *nodeDetail) submitInput() tea.Cmd {
	val := strings.TrimSpace(d.input.Value())
	mode := d.mode
	d.mode = detailInputNone
	d.input.Blur()

	switch mode {
	case detailInputModelName:
		if val == "" {
			d.status.error("model name required")
			return nil
		}
		engine := d.pullTargetEngine()
		if engine == "" {
			d.status.error("no engine available to download into")
			return nil
		}
		d.status.busy("download %s: %s...", d.engineLabel(engine), val)
		return modelCmd(d.client, d.nodeArg(), engine, modelActions["pull"], val)

	case detailInputEnginePort:
		engine := d.selectedEngine()
		if engine == nil {
			return nil
		}
		port, ok := parsePort(val)
		if !ok {
			d.status.error("invalid port: enter a number between 1 and 65535")
			return nil
		}
		d.status.busy("setting the %s engine port to %d...", engine.label(), port)
		return call(d.client, "engine:set-port",
			map[string]any{"engine": engine.Engine, "port": port},
			func(_ *rpc.Message, err error) tea.Msg {
				return engineOpMsg{what: "set engine port", engine: engine.Engine, err: err}
			})

	case detailInputProxyPort:
		engine := d.selectedEngine()
		if engine == nil || d.proxy == nil {
			return nil
		}
		idx := d.proxy.indexForEngine(engine.Engine)
		if idx < 0 {
			return nil
		}
		port, ok := parsePort(val)
		if !ok {
			d.status.error("invalid port: enter a number between 1 and 65535")
			return nil
		}
		d.status.busy("setting the %s proxy port to %d...", engine.label(), port)
		return d.proxy.setPortCmd(d.client, idx, port)
	}
	return nil
}

// parsePort validates a typed TCP port.
func parsePort(val string) (int, bool) {
	port, err := strconv.Atoi(val)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func (d *nodeDetail) lifecycle(engine *engineStatus, op string) tea.Cmd {
	if engine == nil {
		// Say so. A key that ignores a press is indistinguishable from one the
		// terminal dropped, and this is reachable whenever a peer's engine read
		// has failed — exactly when the operator is trying to fix something.
		d.status.error("no engine selected")
		return nil
	}
	spec, ok := engineOps[op]
	if !ok {
		return nil
	}
	if spec.localOnly && d.remote() {
		d.status.error("%s is only available on the machine running the engine", spec.what)
		return nil
	}
	d.status.busy("%s %s...", spec.what, engine.label())
	return engineCmd(d.client, d.nodeArg(), engine.Engine, spec.method, op, spec.what)
}

// openCatalog opens the download browser for the engine a download would go
// into. A stopped engine cannot download, so that is reported here rather than
// after the operator has chosen a model.
func (d *nodeDetail) openCatalog() tea.Cmd {
	engine := d.pullTargetEngine()
	if engine == "" {
		d.status.error("start an engine before downloading a model")
		return nil
	}
	label := engine
	for _, e := range d.engines {
		if e.Engine == engine {
			label = e.label()
			break
		}
	}
	d.catalog = newCatalogBrowser(d.client, engine, label, d.node.name, d.remote())
	d.catalog.SetSize(d.width, d.height)
	return d.catalog.Init()
}

// downloadFromCatalog starts the download the operator picked in the browser.
func (d *nodeDetail) downloadFromCatalog(engine, model string) tea.Cmd {
	d.status.busy("download %s: %s...", d.engineLabel(engine), model)
	return modelCmd(d.client, d.nodeArg(), engine, modelActions["pull"], model)
}

func (d *nodeDetail) modelOp(op string) tea.Cmd {
	act, ok := modelActions[op]
	if !ok {
		return nil
	}
	row := d.selectedModel()
	if row == nil {
		d.status.error("no model selected")
		return nil
	}
	if row.engine == "" {
		// Every model operation is addressed to an engine, and this node did not
		// say which one serves this model. Guessing would act on the wrong one.
		d.status.error("%s did not report which engine serves %s", d.node.name, row.model)
		return nil
	}
	d.status.busy("%s %s...", act.what, row.model)
	return modelCmd(d.client, d.nodeArg(), row.engine, act, row.model)
}

// pendingDestructive is an armed action bound to the exact target it was armed
// against, so a list that re-sorts before the confirmation cannot redirect it.
type pendingDestructive struct {
	run func() tea.Cmd
}

// arm holds a destructive action until the operator confirms it.
//
// The prompt is pinned rather than left to expire: an armed action outliving
// the message that explains it turns the next keystroke into a confirmation the
// operator has no reason to expect.
func (d *nodeDetail) arm(prompt string, run func() tea.Cmd) tea.Cmd {
	d.pending = &pendingDestructive{run: run}
	d.status.arm("%s press y to confirm, any other key to cancel", prompt)
	return nil
}

// resolvePending answers an armed action. Anything but the confirmation key
// cancels, so a stray keystroke never destroys anything.
func (d *nodeDetail) resolvePending(msg tea.KeyMsg) tea.Cmd {
	act := d.pending
	d.pending = nil
	if key.Matches(msg, detailConfirmKey) {
		return act.run()
	}
	// Replaces the pinned prompt, which would otherwise stay on screen.
	d.status.info("cancelled")
	return nil
}

// deleteSelectedModel arms the delete against the model highlighted right now.
func (d *nodeDetail) deleteSelectedModel() tea.Cmd {
	row := d.selectedModel()
	if row == nil {
		d.status.error("no model selected")
		return nil
	}
	if row.engine == "" {
		d.status.error("%s did not report which engine serves %s", d.node.name, row.model)
		return nil
	}
	target := *row
	return d.arm(fmt.Sprintf("delete %s from %s?", target.model, d.engineLabel(target.engine)), func() tea.Cmd {
		d.status.busy("delete %s...", target.model)
		return modelCmd(d.client, d.nodeArg(), target.engine, modelActions["delete"], target.model)
	})
}

// uninstallEngine arms the uninstall against the engine highlighted right now.
func (d *nodeDetail) uninstallEngine(engine *engineStatus) tea.Cmd {
	if engine == nil {
		d.status.error("no engine selected")
		return nil
	}
	name, label := engine.Engine, engine.label()
	return d.arm(fmt.Sprintf("uninstall %s and its downloaded models?", label), func() tea.Cmd {
		return d.lifecycle(&engineStatus{Engine: name, DisplayName: label}, "uninstall")
	})
}

// pullTargetEngine is the engine a download goes into: the one highlighted in
// the engine pane when it can serve, else the only running engine. Downloading
// requires a running engine, so a stopped one is never chosen silently.
func (d *nodeDetail) pullTargetEngine() string {
	if e := d.selectedEngine(); e != nil && e.Running {
		return e.Engine
	}
	for _, e := range d.engines {
		if e.Running {
			return e.Engine
		}
	}
	return ""
}

func (d *nodeDetail) selectedEngine() *engineStatus {
	idx := d.engineTable.Cursor()
	if idx < 0 || idx >= len(d.engines) {
		return nil
	}
	return &d.engines[idx]
}

func (d *nodeDetail) selectedModel() *detailModelRow {
	idx := d.modelTable.Cursor()
	if idx < 0 || idx >= len(d.modelRows) {
		return nil
	}
	return &d.modelRows[idx]
}

func (d *nodeDetail) refreshEngines() {
	rows := make([]table.Row, 0, len(d.engines))
	for _, e := range d.engines {
		port := "-"
		if e.Port != 0 {
			port = strconv.Itoa(e.Port)
		}
		row := table.Row{e.label(), yesNo(e.Installed), yesNo(e.Running), yesNo(e.Healthy), port}
		if d.proxy != nil {
			row = append(row, d.proxyPortCell(e.Engine))
		}
		rows = append(rows, row)
	}
	d.engineTable.SetRows(rows)
	// The engine list is briefly empty on a peer whose first read fails, and a
	// cursor left at -1 disables install, start, stop, and the port editors for
	// the life of the screen.
	restoreCursor(&d.engineTable, len(rows))
}

// proxyPortCell renders the endpoint clients use for an engine. A port with the
// proxy down is not usable, so that reads differently from a live one, and an
// engine no proxy fronts says so rather than showing a blank.
func (d *nodeDetail) proxyPortCell(engine string) string {
	port, ready := d.proxy.portForEngine(engine)
	switch {
	case d.proxy.indexForEngine(engine) < 0:
		return "-"
	case port == 0:
		return "?"
	case !ready:
		return strconv.Itoa(port) + " down"
	default:
		return strconv.Itoa(port)
	}
}

// refreshModels flattens the per-engine inventory into one sorted list, marking
// those resident in memory.
func (d *nodeDetail) refreshModels() {
	// Captured before the rows are rebuilt. Reading it afterwards returns
	// whatever now sits at the old index — that is, exactly the row the cursor
	// slid onto — so restoring it would be a no-op that puts the cursor back
	// where it already was.
	selected := d.selectedModelKey()

	engines := make([]string, 0, len(d.models.ModelsByEngine))
	for name := range d.models.ModelsByEngine {
		engines = append(engines, name)
	}
	sort.Strings(engines)

	d.modelRows = d.modelRows[:0]
	for _, engine := range engines {
		loaded := make(map[string]bool, len(d.models.LoadedByEngine[engine]))
		for _, m := range d.models.LoadedByEngine[engine] {
			loaded[m] = true
		}
		models := append([]string(nil), d.models.ModelsByEngine[engine]...)
		sort.Strings(models)
		for _, m := range models {
			d.modelRows = append(d.modelRows, detailModelRow{engine: engine, model: m, loaded: loaded[m]})
		}
	}

	// A node can report models without saying which engine serves each one —
	// noderec documents the unattributed case as live, for a peer that predates
	// attribution or is running a different version. Building rows only from the
	// per-engine map showed those nodes as having no models at all, and the
	// empty-state hint then blamed a stopped engine for what is a data-shape
	// difference. Fall back to the flat union rather than regressing to nothing.
	if len(d.modelRows) == 0 && len(d.models.Models) > 0 {
		models := append([]string(nil), d.models.Models...)
		sort.Strings(models)
		for _, m := range models {
			d.modelRows = append(d.modelRows, detailModelRow{model: m})
		}
	}

	rows := make([]table.Row, 0, len(d.modelRows))
	for _, r := range d.modelRows {
		engine := d.engineLabel(r.engine)
		if r.engine == "" {
			// The node reported the model but not which engine serves it.
			engine = "unknown"
		}
		rows = append(rows, table.Row{r.model, engine, yesNo(r.loaded)})
	}
	// Put the cursor back on whatever was highlighted. This list is sorted and
	// rebuilt wholesale on every engine:models-changed and every discovery
	// snapshot, so a download finishing elsewhere inserts a row and shifts
	// everything below it. bubbles only clamps a cursor that has run off the end
	// — it does not track what the cursor was pointing at — so without this the
	// selection silently slides onto a different model, and the next d or enter
	// acts on that one.
	d.modelTable.SetRows(rows)
	d.restoreModelSelection(selected)
	restoreCursor(&d.modelTable, len(rows))
}

// engineLabel is an engine key in the vocabulary the rest of the screen uses.
//
// The engines table shows the display name, so a prompt or a message quoting
// the wire id names the same thing twice over on one screen — worst on a
// confirmation, which is the last place to introduce a word the operator has
// not seen. Falls back to the key for an engine not in the list, which is what
// a stale selection would produce.
func (d *nodeDetail) engineLabel(key string) string {
	for _, e := range d.engines {
		if e.Engine == key {
			return e.label()
		}
	}
	// The engine list is not always there to answer. It is fetched per screen
	// and a peer's can be empty — the manager not running, the read failing, or
	// a reply that genuinely lists nothing — while the models table still has
	// rows, because a remote node's models come from discovery instead. Falling
	// back to the raw wire id made the same engine read "Ollama" on one machine
	// and "ollama" on another, purely from whether that fetch had landed.
	//
	// engineDisplayName answers from the static proxy inventory, so it does not
	// depend on any fetch. Only a genuinely unknown engine reaches the key.
	return engineDisplayName(key)
}

// selectedModelKey identifies the highlighted model across a rebuild. Engine
// and name together, because the same model can be present under both engines.
func (d *nodeDetail) selectedModelKey() (key detailModelRow) {
	if r := d.selectedModel(); r != nil {
		key = *r
	}
	return key
}

// restoreModelSelection puts the cursor back on a model after the rows changed.
// A model that is gone — just deleted, or evicted from the peer's inventory —
// leaves the cursor where bubbles clamped it.
func (d *nodeDetail) restoreModelSelection(want detailModelRow) {
	if want.model == "" {
		return
	}
	for i, r := range d.modelRows {
		if r.engine == want.engine && r.model == want.model {
			d.modelTable.SetCursor(i)
			return
		}
	}
}

func (d *nodeDetail) View() string {
	if d.catalog != nil {
		return d.catalog.View()
	}

	scope := "this machine"
	if d.remote() {
		scope = "remote node"
	}
	identity := titleStyle.Render(d.node.name) + footerStyle.Render(fmt.Sprintf(
		"   %s:%d   %s   %s   %s",
		d.node.address, d.node.port, d.node.presence, d.node.membership, scope))

	editor := ""
	if d.mode != detailInputNone {
		editor = d.inputLabel() + d.input.View()
	}
	enginesBody := d.engineTable.View()
	switch {
	case len(d.engines) == 0 && d.enginesStale:
		enginesBody = footerStyle.Render(fmt.Sprintf(
			"  %s is not answering - no engine list available.", d.node.name))
	case len(d.engines) == 0:
		// The manager answered, and its answer was nothing. That is a different
		// fact from the branch above, where it did not answer at all, and the
		// two must not read alike: this one means the engine list itself is
		// empty, which on this machine is a broken engine manager rather than
		// anything the operator did. Bare, "No engines reported" invited the
		// reading that PAIR had lost track of an engine that was plainly there.
		enginesBody = footerStyle.Render(d.emptyEnginesHint())
	case d.enginesStale:
		enginesBody += "\n" + footerStyle.Render(fmt.Sprintf(
			"  %s is not answering - this list may be out of date.", d.node.name))
	}
	modelsEmpty := ""
	if len(d.modelRows) == 0 {
		modelsEmpty = footerStyle.Render(d.emptyModelsHint())
	}

	// A blank line between the sections: two tables stacked flush read as one
	// table with a stray header in the middle. It is a real row, so it is
	// measured as one.
	const separator = " "
	status := d.status.render()
	hardware := d.hardwareBlock()
	enginesHeading := d.paneHeading("Engines", detailEngines)
	modelsHeading := d.paneHeading("Models", detailModels)

	// The engine table's height comes from the engine count, not the budget, so
	// on a very short screen it is the piece that will not shrink. Replaced by a
	// line when there is no room, the same way the models table is — otherwise
	// it holds three rows it cannot afford and the status line pays for them.
	if len(d.engines) > 0 {
		engineRoom := d.height - countLines(identity) - countLines(hardware) -
			countLines(enginesHeading) - countLines(editor) - countLines(status)
		// Against what will actually be rendered, not against the table's
		// minimum. The section is a row taller than the table whenever the list
		// is stale — the "not answering" note — which is the ordinary state for
		// a peer that has gone away, so comparing to the minimum let the section
		// through at exactly the sizes where it did not fit.
		if engineRoom < countLines(enginesBody) {
			return joinLines(identity, hardware, enginesHeading,
				footerStyle.Render(fmt.Sprintf(
					"  (too little room to list %d engine(s))", len(d.engines))),
				editor, status)
		}
	}

	// Everything above the models section is either fixed or already sized, so
	// what is left decides how much of that section can appear. Measuring the
	// real strings — including the editor and status line, which come and go —
	// is what keeps the frame exact whichever of them are on screen.
	above := countLines(identity) + countLines(hardware) +
		countLines(enginesHeading) + countLines(enginesBody) +
		countLines(editor) + countLines(status)
	room := d.height - above

	// The section costs a separator and a heading before it shows anything, so
	// below that it is dropped whole rather than rendered as a heading over
	// nothing. The engine list and the status line are the better use of the
	// last rows.
	const sectionChrome = 2
	if room < sectionChrome+1 {
		return joinLines(identity, hardware, enginesHeading, enginesBody, editor, status)
	}

	modelsBody := modelsEmpty
	if modelsEmpty == "" {
		if fitTable(&d.modelTable, room-sectionChrome) {
			modelsBody = d.modelTable.View()
		} else {
			modelsBody = footerStyle.Render("  (too little room to list models)")
		}
	}

	return joinLines(
		identity,
		hardware,
		enginesHeading,
		enginesBody,
		separator,
		modelsHeading,
		modelsBody,
		editor,
		status,
	)
}

// hardwareBlock renders the node's own GPU, CPU, and memory readings.
func (d *nodeDetail) hardwareBlock() string {
	if !d.telemetryOK {
		if d.node.presence != presenceOnline {
			return footerStyle.Render("  hardware: unavailable (node is not reachable)")
		}
		return footerStyle.Render("  hardware: unavailable")
	}
	lines := d.telemetry.summary()
	if len(lines) == 0 {
		return footerStyle.Render("  hardware: no GPU, CPU, or memory reported")
	}
	// Capped, because this is the one block whose height comes from the machine
	// rather than from the layout: a host reports a line per GPU, and an
	// eight-GPU box produced ten lines that nothing could shrink. Everything
	// below it — the engine table, the models list, the status line — was then
	// pushed off the frame, so the screen showed a hardware readout and nothing
	// else, with no sign anything was missing.
	if max := d.hardwareBudget(); len(lines) > max {
		hidden := len(lines) - (max - 1)
		lines = append(lines[:max-1],
			footerStyle.Render(fmt.Sprintf("  ...and %d more device(s)", hidden)))
	}
	return strings.Join(lines, "\n")
}

// hardwareBudget is the most rows the hardware block may take.
//
// Half the screen, floored at one line so there is always something. The engine
// list, the models list, and the status row all sit below it and matter more
// than an exhaustive device inventory — the operator came here to act on an
// engine, not to audit GPUs.
func (d *nodeDetail) hardwareBudget() int {
	return clampWidth(d.height/2, 1)
}

// emptyModelsHint explains an empty list in terms of the thing to fix, since
// "no models" has several quite different causes.
// emptyEnginesHint explains an engine list that came back empty.
//
// Every supported engine is meant to appear here whether or not it is
// installed — that is how you install one — so an empty list is not "nothing is
// installed", it is the engine manager failing to enumerate. Saying where to
// look beats a flat statement the operator cannot act on.
func (d *nodeDetail) emptyEnginesHint() string {
	if d.remote() {
		return "  No engines reported by this node - its engine manager may not be running."
	}
	// Named rather than numbered: a hardcoded tab number is the same drift that
	// left the footer advertising "1-9" against five tabs.
	return "  No engines reported. Every supported engine should be listed here even when not installed, so this points at the engine manager rather than at what you have installed - check the Logs tab."
}

func (d *nodeDetail) emptyModelsHint() string {
	switch {
	case d.node.presence != presenceOnline:
		return "  No models reported - this node is not reachable."
	case !d.anyEngineRunning():
		return "  No models reported - start an engine to see its models."
	case d.remote():
		return "  No models reported by this node's engines."
	default:
		return "  No models installed. Press p to download one."
	}
}

func (d *nodeDetail) anyEngineRunning() bool {
	for _, e := range d.engines {
		if e.Running {
			return true
		}
	}
	return false
}

func (d *nodeDetail) paneHeading(label string, pane detailPane) string {
	if d.pane == pane {
		return titleStyle.Render("▸ " + label)
	}
	return footerStyle.Render("  " + label)
}

func (d *nodeDetail) inputLabel() string {
	switch d.mode {
	case detailInputEnginePort:
		return "engine port: "
	case detailInputProxyPort:
		return "proxy port: "
	default:
		return "download model: "
	}
}

// Help lists the verbs for the focused pane, filtered to what this node
// actually permits, so a remote node never advertises an operation the engine
// manager cannot perform on it.
func (d *nodeDetail) Help() []key.Binding {
	if d.catalog != nil {
		return d.catalog.Help()
	}
	if d.mode != detailInputNone {
		if d.mode == detailInputModelName {
			return inputHelp("download")
		}
		return inputHelp("set port")
	}
	if d.pending != nil {
		return []key.Binding{detailConfirmKey}
	}
	bindings := []key.Binding{detailBackKey, detailPaneKey}
	if d.pane == detailEngines {
		bindings = append(bindings, detailInstallKey, detailStartKey, detailStopKey)
		if !d.remote() {
			bindings = append(bindings,
				detailRestartKey, detailUninstKey, detailPortKey, detailProxyKey)
		}
		return bindings
	}
	return append(bindings,
		detailPullKey, detailPullByNameKey, detailLoadKey, detailEjectKey, detailDeleteKey)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
