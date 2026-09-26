// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestWorkloadsRemainALiveTable(t *testing.T) {
	v := newWorkloadsView(nil)
	v.SetSize(100, 20)
	if cmd := v.Update(workloadsSubscribedMsg{}); cmd != nil {
		t.Fatal("live table must not request an expanded startup snapshot")
	}
	for _, engine := range []string{"ollama", "lmstudio", "llamacpp"} {
		w := workload{ID: "1", Model: "model", Engine: engine, RunID: "a", OriginatedFrom: "origin-node", State: "running"}
		params, err := json.Marshal(map[string]workload{"workloadInfo": w})
		if err != nil {
			t.Fatal(err)
		}
		v.Update(NotificationMsg{Msg: &rpc.Message{Method: "workloads:upsert", Params: params}})
	}
	rows := v.table.Rows()
	if len(rows) != 3 {
		t.Fatalf("workload rows = %v, want all three engines", rows)
	}
	for i, engine := range []string{"ollama", "lmstudio", "llamacpp"} {
		if rows[i][1] != "model" || rows[i][2] != engine || rows[i][3] != "running" {
			t.Fatalf("row %d lost workload attribution: %v", i, rows[i])
		}
	}
	if v.View() != v.table.View() {
		t.Fatal("workload view must not add a detail/status pane")
	}
	if height := lipgloss.Height(v.View()); height != 19 {
		t.Fatalf("table height = %d, want 19 including headers", height)
	}
}

func TestWorkloadsHaveNoCancellationControl(t *testing.T) {
	v := newWorkloadsView(nil)
	v.SetSize(100, 20)
	v.upsert(workload{ID: "1", Engine: "llamacpp", RunID: "a", OriginatedFrom: "self", State: "running"})
	before := v.View()
	if cmd := v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}}); cmd != nil {
		t.Fatal("c must not dispatch workload cancellation")
	}
	if len(v.Help()) != 0 || v.View() != before {
		t.Fatal("workload cancellation changed view or help")
	}
}

func TestWorkloadsShowSubscriptionFailure(t *testing.T) {
	v := newWorkloadsView(nil)
	if cmd := v.Update(workloadsSubscribedMsg{err: errors.New("subscription unavailable")}); cmd != nil {
		t.Fatal("failed subscription must not request a snapshot")
	}
	if !strings.Contains(v.View(), "subscription unavailable") {
		t.Fatal("subscription error hidden by empty workload table")
	}
}

func TestWorkloadsKeepEngineRunIdentityAndTerminalTruth(t *testing.T) {
	v := newWorkloadsView(nil)
	v.SetSize(100, 20)
	for _, w := range []workload{
		{ID: "1", Engine: "ollama", RunID: "a", OriginatedFrom: "self", State: "running"},
		{ID: "1", Engine: "llamacpp", RunID: "a", OriginatedFrom: "self", State: "completed"},
		{ID: "1", Engine: "llamacpp", RunID: "b", OriginatedFrom: "self", State: "running"},
	} {
		v.upsert(w)
	}
	v.upsert(workload{ID: "1", Engine: "llamacpp", RunID: "a", OriginatedFrom: "self", State: "running"})
	if len(v.byKey) != 3 {
		t.Fatalf("identity collision: %v", v.byKey)
	}
	if v.byKey[workloadKey("self", "1", "llamacpp", "a")].State != "completed" {
		t.Fatal("late event regressed completed request")
	}
}

func TestWorkloadsRemovalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
		want   int
	}{
		{"exact", `{"workloadId":"1","originatedFrom":"self","engine":"llamacpp","runId":"a"}`, 3},
		{"origin and id", `{"workloadId":"1","originatedFrom":"self"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newWorkloadsView(nil)
			v.SetSize(100, 20)
			for _, w := range []workload{
				{ID: "1", Engine: "ollama", RunID: "a", OriginatedFrom: "self"},
				{ID: "1", Engine: "llamacpp", RunID: "a", OriginatedFrom: "self"},
				{ID: "1", Engine: "llamacpp", RunID: "b", OriginatedFrom: "self"},
				{ID: "1", Engine: "llamacpp", RunID: "a", OriginatedFrom: "peer"},
			} {
				v.upsert(w)
			}
			v.Update(NotificationMsg{Msg: &rpc.Message{Method: "workloads:remove", Params: json.RawMessage(tc.params)}})
			if len(v.byKey) != tc.want || len(v.table.Rows()) != tc.want {
				t.Fatalf("removal changed other identities: %v", v.byKey)
			}
			if _, exists := v.byKey[workloadKey("self", "1", "llamacpp", "a")]; exists {
				t.Fatal("removed workload still present")
			}
			if _, exists := v.byKey[workloadKey("peer", "1", "llamacpp", "a")]; !exists {
				t.Fatal("removal crossed the origin boundary")
			}
		})
	}
}
