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
	ScheduledOn    string `json:"scheduledOn"`
	CreatedAt      int64  `json:"createdAt"` // Unix millis
}

// workloadsView combines the initial snapshot with live workload events.
type workloadsView struct {
	client *rpc.Client
	table  table.Model
	order  []string
	byKey  map[string]workload
	status string

	width, height int
}

type workloadsSubscribedMsg struct{ err error }
type workloadsInitialMsg struct {
	workloads []workload
	err       error
}
type workloadCancelMsg struct {
	accepted bool
	err      error
}

var workloadCancelKey = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel local llama request"))

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
	v.table.SetHeight(clampWidth(h-3, 1))
}

func (v *workloadsView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case workloadCancelMsg:
		if msg.err != nil {
			v.status = "Cancel failed: " + msg.err.Error()
		} else if msg.accepted {
			v.status = "Cancellation requested; awaiting terminal workload event."
		} else {
			v.status = "Request is no longer active in that proxy run."
		}
		return nil
	case workloadsSubscribedMsg:
		if msg.err != nil {
			v.status = "workloads subscribe failed: " + msg.err.Error()
			return nil
		}
		return call(v.client, "workloads:get-initial", nil, func(msg *rpc.Message, err error) tea.Msg {
			if err != nil {
				return workloadsInitialMsg{err: err}
			}
			var result struct {
				Workloads []workload `json:"workloads"`
			}
			err = decodeParams(msg.Result, &result)
			return workloadsInitialMsg{workloads: result.Workloads, err: err}
		})
	case workloadsInitialMsg:
		if msg.err != nil {
			v.status = "Workload baseline unavailable: " + msg.err.Error()
			return nil
		}
		for _, w := range msg.workloads {
			v.upsert(w)
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
		if key.Matches(msg, workloadCancelKey) {
			index := v.table.Cursor()
			if index < 0 || index >= len(v.order) {
				return nil
			}
			w := v.byKey[v.order[index]]
			if w.Engine != "llamacpp" || w.RunID == "" || w.State != "running" {
				v.status = "Only active llama requests with a run identity can be cancelled."
				return nil
			}
			return call(v.client, "workloads:cancel", map[string]string{"id": w.ID, "runId": w.RunID, "engine": w.Engine, "originatedFrom": w.OriginatedFrom}, func(msg *rpc.Message, err error) tea.Msg {
				if err != nil {
					return workloadCancelMsg{err: err}
				}
				var result struct {
					Accepted bool `json:"accepted"`
				}
				err = decodeParams(msg.Result, &result)
				return workloadCancelMsg{accepted: result.Accepted, err: err}
			})
		}
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
	if len(v.order) == 0 {
		empty := "No active workloads. Live cluster workloads will appear here as they run."
		// An empty list and a failed baseline fetch look identical otherwise,
		// so a subscribe or get-initial error would be written to v.status and
		// never rendered.
		if v.status != "" {
			return footerStyle.Render(empty) + "\n" + footerStyle.Render(v.status)
		}
		return footerStyle.Render(empty)
	}
	detail := ""
	if index := v.table.Cursor(); index >= 0 && index < len(v.order) {
		w := v.byKey[v.order[index]]
		target := w.ScheduledOn
		if target == "" {
			target = "unknown (not reported)"
		}
		detail = "Origin: " + w.OriginatedFrom + " | Runs on: " + target
	}
	return v.table.View() + "\n" + footerStyle.Width(v.width).Render(detail) + "\n" + footerStyle.Render(v.status)
}

func (v *workloadsView) Help() []key.Binding { return []key.Binding{workloadCancelKey} }

func workloadKey(origin, id string, identity ...string) string {
	key := origin + "/" + id
	for _, part := range identity {
		key += "/" + part
	}
	return key
}
