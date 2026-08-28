// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"
	"time"

	svcerrors "nvpair-shared/errors"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// crashPrefix is the id prefix the broker stamps on its sticky
// "subprocess X exited unexpectedly" errors. The Health view derives
// per-worker liveness from the presence/absence of these in the
// errors:update snapshot, since the broker exposes no dedicated
// workers:get-status RPC.
const crashPrefix = "supervisor:subprocess-crashed:"

// healthPollInterval is how often the Overview re-pings the broker for
// liveness + uptime.
const healthPollInterval = 5 * time.Second

// healthWorkers are the workers the broker supervises and reports crashes
// for. nvpair-errors is deliberately absent: it is the error sink itself and
// cannot report its own death, so it has no crash entry to key on.
var healthWorkers = []string{
	"scanner",
	"node-info",
	"proxy",
	"lmstudio-proxy",
	"workload-manager",
	"engine-manager",
	"manual-nodes",
	"settings",
	"cluster-manager",
}

// healthView is the Overview tab: broker liveness/version/uptime from
// periodic pings, plus a per-worker health table derived from the broker's
// crash-error stream.
type healthView struct {
	client *rpc.Client
	table  table.Model

	brokerVersion string
	uptime        time.Duration
	pingErr       error
	crashed       map[string]svcerrors.ServiceError

	// localNodeUUID is this host's stable per-host UUID (from cluster:get-node-id).
	// The broker stamps local-origin reports' NodeID with this UUID, so a crash
	// entry whose NodeID differs belongs to a peer and must be ignored —
	// errors:update is the full cross-node snapshot when nvpair-errors runs with
	// --peer-sync. (Filtering on the display hostname instead would reject every
	// local UUID-stamped crash as remote.)
	localNodeUUID string
	// lastErrs is the most recent errors:update snapshot, retained so the
	// crash table can be re-filtered once localNodeID resolves.
	lastErrs []svcerrors.ServiceError

	width, height int
}

type healthTickMsg struct{}

type healthPingMsg struct {
	version string
	uptime  time.Duration
	err     error
}

type healthNodeIDMsg struct {
	nodeUUID string
	err      error
}

func newHealthView(client *rpc.Client) *healthView {
	v := &healthView{client: client, crashed: map[string]svcerrors.ServiceError{}}
	v.table = newTable([]table.Column{
		{Title: "WORKER", Width: 20},
		{Title: "STATUS", Width: 10},
		{Title: "DETAIL", Width: 40},
	})
	return v
}

func (v *healthView) Title() string { return "Overview" }

func (v *healthView) Init() tea.Cmd {
	return tea.Batch(v.pingCmd(), v.tickCmd(), v.nodeIDCmd())
}

// nodeIDCmd resolves this host's stable UUID so the crash table can drop
// peer-origin entries from the cross-node errors:update snapshot. It keys on
// NodeUUID (not the display NodeID/hostname) to match the UUID the broker stamps
// on local reports.
func (v *healthView) nodeIDCmd() tea.Cmd {
	return call(v.client, "cluster:get-node-id", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return healthNodeIDMsg{err: err}
		}
		var id clusterIdentity
		_ = decodeParams(msg.Result, &id)
		return healthNodeIDMsg{nodeUUID: id.NodeUUID}
	})
}

func (v *healthView) pingCmd() tea.Cmd {
	return call(v.client, "ping", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return healthPingMsg{err: err}
		}
		var r struct {
			Version  string `json:"version"`
			UptimeMS int64  `json:"uptime_ms"`
		}
		_ = decodeParams(msg.Result, &r)
		return healthPingMsg{version: r.Version, uptime: time.Duration(r.UptimeMS) * time.Millisecond}
	})
}

func (v *healthView) tickCmd() tea.Cmd {
	return tea.Tick(healthPollInterval, func(time.Time) tea.Msg { return healthTickMsg{} })
}

func (v *healthView) SetSize(w, h int) {
	v.width, v.height = w, h
	detail := clampWidth(w-20-10-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "WORKER", Width: 20},
		{Title: "STATUS", Width: 10},
		{Title: "DETAIL", Width: detail},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-3, 1))
}

func (v *healthView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case healthTickMsg:
		return tea.Batch(v.pingCmd(), v.tickCmd())

	case healthPingMsg:
		v.pingErr = msg.err
		if msg.err == nil {
			v.brokerVersion = msg.version
			v.uptime = msg.uptime
		}
		return nil

	case healthNodeIDMsg:
		if msg.err == nil && msg.nodeUUID != "" {
			v.localNodeUUID = msg.nodeUUID
			// Re-filter the last snapshot now that we know who we are.
			v.rebuildCrashes(v.lastErrs)
		}
		return nil

	case NotificationMsg:
		if msg.Msg.Method == "errors:update" {
			var errs []svcerrors.ServiceError
			_ = decodeParams(msg.Msg.Params, &errs)
			v.rebuildCrashes(errs)
		}
		return nil

	case tea.KeyMsg:
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	return nil
}

func (v *healthView) rebuildCrashes(errs []svcerrors.ServiceError) {
	v.lastErrs = errs
	crashed := map[string]svcerrors.ServiceError{}
	for _, e := range errs {
		// errors:update is the full cross-node snapshot (nvpair-errors runs
		// with --peer-sync in a cluster), so a peer's crashed worker would
		// otherwise paint this host's same-named worker DOWN. Keep only
		// local-origin crashes. Once localNodeUUID is known, drop entries
		// whose NodeID (a UUID, as the broker stamps it) is a different node;
		// an empty NodeID is treated as local (be defensive).
		if v.localNodeUUID != "" && e.NodeID != "" && e.NodeID != v.localNodeUUID {
			continue
		}
		if strings.HasPrefix(e.ID, crashPrefix) {
			crashed[strings.TrimPrefix(e.ID, crashPrefix)] = e
		}
	}
	v.crashed = crashed
	v.refreshRows()
}

func (v *healthView) refreshRows() {
	rows := make([]table.Row, 0, len(healthWorkers))
	for _, w := range healthWorkers {
		status, detail := "ok", ""
		if e, down := v.crashed[w]; down {
			status, detail = "DOWN", e.Message
		}
		rows = append(rows, table.Row{w, status, detail})
	}
	v.table.SetRows(rows)
}

func (v *healthView) View() string {
	var b strings.Builder
	if v.pingErr != nil {
		b.WriteString(statusErrStyle.Render("broker not responding: " + v.pingErr.Error()))
	} else {
		summary := fmt.Sprintf("broker v%s  up %s", v.brokerVersion, v.uptime.Round(time.Second))
		b.WriteString(statusOKStyle.Render(summary))
	}
	b.WriteString("\n\n")
	if len(v.table.Rows()) == 0 {
		v.refreshRows()
	}
	b.WriteString(v.table.View())
	b.WriteString("\n")
	b.WriteString(footerStyle.Render("status is best-effort: DOWN means the broker reported a crash; nvpair-errors self-crashes are not surfaced here"))
	return b.String()
}

func (v *healthView) Help() []key.Binding { return nil }
