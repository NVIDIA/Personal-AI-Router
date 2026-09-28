// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// workload is the subset of the workload-manager's object the view shows.
type workload struct {
	ID             string `json:"id"`
	Model          string `json:"model"`
	Engine         string `json:"engine"`
	RunID          string `json:"runId"`
	State          string `json:"state"`
	OriginatedFrom string `json:"originatedFrom"`
	CreatedAt      int64  `json:"createdAt"` // Unix millis
}

// workloadsView shows live cluster workloads after subscribing. Requests already
// in flight appear on their next event; this table does not fetch a baseline.
type workloadsView struct {
	client *rpc.Client
	table  table.Model
	order  []string
	byKey  map[string]workload
	status string

	width, height int
}

type workloadsSubscribedMsg struct{ err error }

func newWorkloadsView(client *rpc.Client) *workloadsView {
	v := &workloadsView{client: client, byKey: map[string]workload{}}
	v.table = newTable(nil)
	return v
}

func (v *workloadsView) Title() string { return "Workloads" }

func (v *workloadsView) Init() tea.Cmd {
	return call(v.client, "workloads:subscribe", nil, func(_ *rpc.Message, err error) tea.Msg {
		return workloadsSubscribedMsg{err: err}
	})
}

func (v *workloadsView) SetSize(w, h int) {
	v.width, v.height = w, h
	const engine, state, age = 10, 10, 6
	id := clampWidth((w-engine-state-age-2)/3, 8)
	model := clampWidth(w-engine-state-age-id-2, 10)
	v.table.SetColumns([]table.Column{
		{Title: "ID", Width: id},
		{Title: "MODEL", Width: model},
		{Title: "ENGINE", Width: engine},
		{Title: "STATE", Width: state},
		{Title: "AGE", Width: age},
	})
	v.table.SetWidth(w)
	v.table.SetHeight(clampWidth(h-1, 1))
}

func (v *workloadsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case workloadsSubscribedMsg:
		if msg.err != nil {
			v.status = "workloads subscribe failed: " + msg.err.Error()
		}
		return nil

	case NotificationMsg:
		switch msg.Msg.Method {
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
				Engine         string `json:"engine"`
				RunID          string `json:"runId"`
			}
			_ = decodeParams(msg.Msg.Params, &p)
			for key, w := range v.byKey {
				if w.OriginatedFrom == p.OriginatedFrom && w.ID == p.WorkloadID && (p.Engine == "" || p.Engine == w.Engine) && (p.RunID == "" || p.RunID == w.RunID) {
					v.remove(key)
				}
			}
		}
		return nil

	case tea.KeyMsg:
		var cmd tea.Cmd
		v.table, cmd = v.table.Update(msg)
		return cmd
	}
	return nil
}

func (v *workloadsView) upsert(w workload) {
	key := workloadKey(w.OriginatedFrom, w.ID, w.Engine, w.RunID)
	if previous, ok := v.byKey[key]; ok && (previous.State == "completed" || previous.State == "failed") && w.State != "completed" && w.State != "failed" {
		return
	}
	if _, ok := v.byKey[key]; !ok {
		v.order = append(v.order, key)
	}
	v.byKey[key] = w
	v.refreshRows()
}

func (v *workloadsView) remove(key string) {
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

func (v *workloadsView) refreshRows() {
	rows := make([]table.Row, 0, len(v.order))
	for _, k := range v.order {
		w := v.byKey[k]
		rows = append(rows, table.Row{
			truncate(w.ID, 12),
			w.Model,
			w.Engine,
			w.State,
			ageLabel(w.CreatedAt),
		})
	}
	v.table.SetRows(rows)
}

func (v *workloadsView) View() string {
	if v.status != "" {
		return statusErrStyle.Render(v.status)
	}
	if len(v.order) == 0 {
		return footerStyle.Render("No active workloads. Live cluster workloads will appear here as they run.")
	}
	return v.table.View()
}

func (v *workloadsView) Help() []key.Binding { return nil }

func workloadKey(origin, id string, identity ...string) string {
	key := origin + "/" + id
	for _, part := range identity {
		key += "/" + part
	}
	return key
}
