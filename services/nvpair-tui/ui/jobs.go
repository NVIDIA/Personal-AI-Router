// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// workload is the subset of the workload-manager's object the view shows.
//
// OriginatedFrom and ScheduledOn are both stable node UUIDs, not names: the
// broker stamps its own resolveLocalNodeID onto local-origin events, and peers
// carry theirs. They are resolved through a nodeNamer before display.
type workload struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Engine string `json:"engine"`
	State  string `json:"state"`
	// OriginatedFrom is the node the request arrived at.
	OriginatedFrom string `json:"originatedFrom"`
	// ScheduledOn is the node that actually ran it, absent until the backend
	// has chosen a target. Origin and target differ whenever work is routed to
	// a peer, which is the whole point of a cluster — so both are shown.
	ScheduledOn string `json:"scheduledOn"`
	CreatedAt   int64  `json:"createdAt"` // Unix millis
}

// jobsView shows inference work across the cluster, headed by where local
// clients should send it.
//
// The two belong together: a job list is the answer to "is my traffic being
// served", and the proxy endpoints are the answer to "where do I send it". The
// jobs themselves come from a baseline snapshot plus the live stream — both are
// needed, since subscribing alone leaves work that was already running
// invisible until its next state change, which for a long generation is minutes.
type jobsView struct {
	client *rpc.Client
	table  table.Model
	proxy  *proxyTracker
	// namer turns the node UUIDs on each job into names. It is fed from the
	// discovery and membership pushes the shell broadcasts to every view, so it
	// costs one identity call at startup and nothing after that.
	namer *nodeNamer

	order []string
	byKey map[string]workload
	// trimmed counts finished jobs dropped to stay inside maxFinishedJobs, so
	// the tab can say the history is not complete. Every other list here
	// discloses when it is showing less than everything; this one silently
	// discarded the oldest entries.
	trimmed int
	// sinceProxyPoll counts shell ticks since the last proxy status read.
	sinceProxyPoll int
	status         toast

	// showAll includes finished work. The manager keeps history, and a
	// completed job is the evidence that routing worked, so it is reachable —
	// but active work is what an operator is usually watching.
	showAll bool

	// demo is the Inference Demo. It lives on this tab rather than with the rest
	// of the service controls because the proxy ports it needs are already here,
	// and because the traffic it produces appears in the table below it — so
	// starting it and seeing the result are the same screen.
	demo *demoRunner

	width, height int
}

type workloadsSubscribedMsg struct{ err error }

// workloadsLoadedMsg carries the workloads:get-initial baseline.
type workloadsLoadedMsg struct {
	workloads []workload
	err       error
}

// jobsIdentityMsg carries this machine's identity, so its own jobs show a name.
type jobsIdentityMsg struct {
	id  clusterIdentity
	err error
}

var (
	jobsAllKey = key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "active/all"))
	// t for test, matching the desktop app's button. The bubbles table binds
	// j/k, f/b, u/d, g/G and the arrows, so t is one of the few letters left
	// that does not fight the paging keys on a scrollable table.
	jobsDemoKey = key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "test traffic"))
)

func newJobsView(client *rpc.Client) *jobsView {
	v := &jobsView{
		client: client,
		byKey:  map[string]workload{},
		proxy:  newProxyTracker(),
		namer:  newNodeNamer(),
		demo:   newDemoRunner(),
	}
	v.table = newTable(workloadColumns(defaultTableWidth))
	return v
}

// workloadColumns is the job table's layout, shared by construction and resize
// so the two cannot drift.
func workloadColumns(w int) []table.Column {
	return layoutColumns(w, []column{
		flexCol("MODEL", 10, 2),
		fixedCol("ENGINE", 10),
		fixedCol("STATE", 11),
		flexCol("FROM", 8, 1),
		flexCol("RAN ON", 8, 1),
		fixedCol("AGE", 6),
	})
}

func (v *jobsView) Title() string { return "Jobs" }

// Init subscribes and fetches the baseline. Subscribing first means a workload
// that changes state between the two calls arrives as a push and is merged by
// key rather than lost.
func (v *jobsView) Init() tea.Cmd {
	return tea.Batch(
		call(v.client, "workloads:subscribe", nil, func(_ *rpc.Message, err error) tea.Msg {
			return workloadsSubscribedMsg{err: err}
		}),
		v.loadCmd(),
		v.proxy.init(v.client),
		nodeIdentityCmd(v.client, func(id clusterIdentity, err error) tea.Msg {
			return jobsIdentityMsg{id: id, err: err}
		}),
	)
}

func (v *jobsView) loadCmd() tea.Cmd {
	return call(v.client, "workloads:get-initial", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return workloadsLoadedMsg{err: err}
		}
		var r struct {
			Workloads []workload `json:"workloads"`
		}
		_ = decodeParams(msg.Result, &r)
		return workloadsLoadedMsg{workloads: r.Workloads}
	})
}

// SetSize records the budget and fixes the table's width. Its height is set in
// View, from the chrome actually being rendered — see fitTable.
func (v *jobsView) SetSize(w, h int) {
	v.width, v.height = w, h
	v.table.SetColumns(workloadColumns(w))
	v.table.SetWidth(w)
}

func (v *jobsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case workloadsSubscribedMsg:
		if msg.err != nil {
			v.status.error("workloads subscribe failed: %s", msg.err)
		}
		return nil

	case workloadsLoadedMsg:
		if msg.err != nil {
			v.status.error("load jobs failed: %s", msg.err)
			return nil
		}
		// Merged, not assigned: a push may already have landed for a workload
		// in this snapshot, and the push is the fresher of the two.
		for _, w := range msg.workloads {
			if _, seen := v.byKey[workloadKey(w.OriginatedFrom, w.ID)]; !seen {
				v.upsert(w)
			}
		}
		return nil

	case proxyStatusMsg:
		v.proxy.apply(msg)
		return nil

	case jobsIdentityMsg:
		if msg.err == nil {
			v.namer.setSelf(msg.id)
			v.refreshRows()
		}
		return nil

	case demoTargetsMsg:
		if msg.err != nil {
			v.status.error("inference demo: %s", msg.err)
			return nil
		}
		if !v.demo.armed(msg) {
			// Either the run was stopped while discovery was out, or no engine
			// offered a model. Only the second is worth saying, and it needs to
			// name the remedy rather than the failure.
			if v.demo.status == demoIdle && len(msg.targets) == 0 {
				v.status.error("no model available to send traffic to - install one from a node's detail screen first")
			}
			return nil
		}
		return nil

	case TickMsg:
		// AGE is relative, so the table has to repaint on the clock.
		v.refreshRows()
		// The demo's whole schedule runs off this tick; every submission offset
		// is a whole number of seconds, so one second is enough resolution and
		// the run needs no timer of its own.
		demoCmds, finished := v.demo.tick()
		if finished {
			v.status.info("inference demo finished - the work it sent is in the table")
		}
		// Re-read proxy readiness periodically, but not on every tick. A proxy
		// that crashes emits no error frame, so a strip corrected only by pushes
		// can advertise a port nothing is listening on for the rest of the
		// session — but the shell ticks once a second, and two broker round
		// trips per second, each relayed on to a worker, is a poor trade on the
		// headless machines this client is meant to be left running on.
		v.sinceProxyPoll++
		if v.sinceProxyPoll >= proxyPollTicks {
			v.sinceProxyPoll = 0
			demoCmds = append(demoCmds, v.proxy.refreshCmd(v.client))
		}
		return tea.Batch(demoCmds...)

	case NotificationMsg:
		switch msg.Msg.Method {
		case "discovery:nodes-changed":
			// Not a job event, but the only place the UUID-to-name mapping for
			// the FROM and RAN ON columns comes from.
			var nodes []availableNode
			_ = decodeParams(msg.Msg.Params, &nodes)
			v.namer.learnDiscovered(nodes)
			v.refreshRows()
		case "nodes:changed":
			var r struct {
				Nodes []clusterNode `json:"nodes"`
			}
			_ = decodeParams(msg.Msg.Params, &r)
			v.namer.learnMembers(r.Nodes)
			v.refreshRows()
		case "workloads:upsert":
			var p struct {
				WorkloadInfo workload `json:"workloadInfo"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			v.upsert(p.WorkloadInfo)
		case "workloads:remove":
			var p struct {
				WorkloadID     string `json:"workloadId"`
				OriginatedFrom string `json:"originatedFrom"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			v.remove(workloadKey(p.OriginatedFrom, p.WorkloadID))
		default:
			v.proxy.handleNotification(msg.Msg)
		}
		return nil

	case tea.KeyMsg:
		if key.Matches(msg, jobsAllKey) {
			v.showAll = !v.showAll
			v.refreshRows()
			return nil
		}
		if key.Matches(msg, jobsDemoKey) {
			return v.toggleDemo()
		}
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	return nil
}

// proxyPollTicks is how many one-second shell ticks pass between proxy status
// reads. Five matches the service tab's own poll: a proxy that has stopped
// listening should be noticed promptly, but it is a rare event and the check
// costs a broker round trip that fans out to a worker.
const proxyPollTicks = 5

// maxFinishedJobs bounds the completed and failed jobs kept in memory.
//
// The terminal client is meant to be left running — that is the point of a
// status screen — and the broker never asks a client to forget a job, so
// without a bound every job the cluster has ever run accumulates for the life
// of the process. The desktop caps its history at 30 for the same reason; this
// is more generous because scrolling a terminal table is cheap, and it is a cap
// on finished work only, so no in-flight job is ever dropped.
const maxFinishedJobs = 200

func (v *jobsView) upsert(w workload) {
	key := workloadKey(w.OriginatedFrom, w.ID)
	if _, ok := v.byKey[key]; !ok {
		v.order = append(v.order, key)
	}
	v.byKey[key] = w
	v.trimFinished()
	v.refreshRows()
}

// finishedCount is how many retained jobs have finished.
func (v *jobsView) finishedCount() int {
	n := 0
	for _, key := range v.order {
		if !workloadActive(v.byKey[key].State) {
			n++
		}
	}
	return n
}

// trimFinished drops the oldest finished jobs once there are too many.
//
// Only finished ones are eligible: an active job is what the operator is
// watching, and the count of those is bounded by the cluster's own capacity
// anyway. Eviction walks oldest-first because v.order is append-ordered, so the
// history the operator loses is the history they are least likely to want.
func (v *jobsView) trimFinished() {
	finished := 0
	for _, key := range v.order {
		if !workloadActive(v.byKey[key].State) {
			finished++
		}
	}
	if finished <= maxFinishedJobs {
		return
	}

	drop := finished - maxFinishedJobs
	kept := make([]string, 0, len(v.order))
	for _, key := range v.order {
		if drop > 0 && !workloadActive(v.byKey[key].State) {
			delete(v.byKey, key)
			drop--
			v.trimmed++
			continue
		}
		kept = append(kept, key)
	}
	v.order = kept
}

func (v *jobsView) remove(key string) {
	if _, ok := v.byKey[key]; !ok {
		return
	}
	delete(v.byKey, key)
	for i, k := range v.order {
		if k == key {
			v.order = append(v.order[:i], v.order[i+1:]...)
			break
		}
	}
	v.refreshRows()
}

// workloadActive reports whether a job is still in flight. The manager's state
// vocabulary grows over time, so this names the terminal states and treats
// anything else as active rather than silently hiding unfamiliar work.
func workloadActive(state string) bool {
	switch strings.ToLower(state) {
	case "completed", "complete", "done", "failed", "error", "errored", "cancelled", "canceled":
		return false
	default:
		return true
	}
}

func (v *jobsView) visible() []workload {
	out := make([]workload, 0, len(v.order))
	for _, k := range v.order {
		w := v.byKey[k]
		if v.showAll || workloadActive(w.State) {
			out = append(out, w)
		}
	}
	return out
}

func (v *jobsView) refreshRows() {
	jobs := v.visible()
	rows := make([]table.Row, 0, len(jobs))
	for _, w := range jobs {
		rows = append(rows, table.Row{
			w.Model,
			engineDisplayName(w.Engine),
			w.State,
			v.namer.name(w.OriginatedFrom),
			v.ranOn(w),
			ageLabel(w.CreatedAt),
		})
	}
	v.table.SetRows(rows)
	// The last SetRows without one. Jobs starts empty on every launch, which is
	// the case that strands the cursor at -1; nothing is keyed off the selection
	// here today, but a table with no visible highlight is wrong on its own and
	// this becomes a real defect the moment a row action is added.
	restoreCursor(&v.table, len(rows))
}

// ranOn renders the node that served a job. The backend fills scheduledOn once
// it has chosen a target, so an unplaced job says so rather than showing a blank
// that reads as "nowhere".
func (v *jobsView) ranOn(w workload) string {
	if w.ScheduledOn == "" {
		if workloadActive(w.State) {
			return footerStyle.Render("choosing")
		}
		return unknownNodeLabel
	}
	return v.namer.name(w.ScheduledOn)
}

func (v *jobsView) View() string {
	strip := v.proxy.strip()

	empty := ""
	if len(v.table.Rows()) == 0 {
		empty = footerStyle.Render(v.emptyHint())
	}
	// Disclosed for the same reason the node table says "showing N of M": a
	// list showing less than everything looks complete otherwise. Two ways it
	// can be short — the active-only filter, and the history cap.
	trimNote := ""
	switch {
	case !v.showAll && v.finishedCount() > 0:
		trimNote = footerStyle.Render(fmt.Sprintf(
			"%d finished job(s) hidden - press a to include them", v.finishedCount()))
	case v.showAll && v.trimmed > 0:
		trimNote = footerStyle.Render(fmt.Sprintf(
			"%d older finished job(s) dropped to bound memory", v.trimmed))
	}

	// The blank line after the strip is a real row and is measured as one.
	const separator = " "
	demoNote := v.demo.note()
	status := v.status.render()

	body := empty
	if len(v.table.Rows()) > 0 {
		if fitTable(&v.table, v.height, strip, separator, trimNote, demoNote, status) {
			body = v.table.View()
		} else {
			body = footerStyle.Render("  (too little room to list jobs)")
		}
	}
	return joinLines(strip, separator, body, trimNote, demoNote, status)
}

// close releases the demo's children. Part of the closer contract in model.go.
func (v *jobsView) close() { v.demo.close() }

// toggleDemo starts a demo, or stops the one already running.
//
// One key for both, because the note on screen says which it will do and a
// second key would sit unused for all but sixty seconds of a session.
func (v *jobsView) toggleDemo() tea.Cmd {
	if v.demo.status != demoIdle {
		v.demo.stop()
		v.status.info("inference demo stopped - requests already sent will still finish")
		return nil
	}
	cmd, err := v.demo.start(v.proxy)
	if err != nil {
		v.status.error("inference demo: %s", err)
		return nil
	}
	return cmd
}

func (v *jobsView) emptyHint() string {
	if !v.showAll && len(v.order) > 0 {
		return "No active jobs. Press a to include finished ones."
	}
	return "No jobs yet. Inference sent to a proxy endpoint above will appear here."
}

func (v *jobsView) Help() []key.Binding {
	demo := jobsDemoKey
	if v.demo.status != demoIdle {
		// Relabelled rather than replaced, so the footer and the note on screen
		// agree about what the key does right now.
		demo = key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "stop test"))
	}
	return []key.Binding{jobsAllKey, demo}
}

func workloadKey(origin, id string) string { return origin + "/" + id }
