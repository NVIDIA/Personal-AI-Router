// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

func focusVLLMProxy(t *testing.T, view *proxiesView) {
	t.Helper()
	view.SetSize(100, 20)
	for i, engine := range view.engines {
		if engine.prefix == "vllm-proxy" {
			view.focus = i
			engine.nodes = []proxyNode{{ID: "node-a", Host: "spark-a", Port: 8001}}
			view.refreshRows(i)
			return
		}
	}
	t.Fatal("vLLM proxy is absent")
}

func TestVLLMProxyMutationsFailClosedUntilGroupReleased(t *testing.T) {
	view := newProxiesView(nil)
	focusVLLMProxy(t, view)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'p'}},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune{'a'}},
	} {
		if cmd := view.handleKey(key); cmd != nil {
			t.Fatalf("unknown group authority dispatched %q", key.String())
		}
		if !strings.Contains(view.status, "ownership is not known") {
			t.Fatalf("unknown group reason missing after %q: %q", key.String(), view.status)
		}
	}
	if view.editingPort || len(view.Help()) != 1 {
		t.Fatalf("blocked vLLM controls remained visible: editing=%v help=%d", view.editingPort, len(view.Help()))
	}

	no, yes := false, true
	view.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: &no, Reserved: &yes, Reason: "cleanup required"}})
	if cmd := view.selectNode("node-a"); cmd != nil || !strings.Contains(view.status, "cleanup required") {
		t.Fatalf("held direct select bypassed gate: cmd=%v status=%q", cmd != nil, view.status)
	}
	view.editingPort = true
	view.portInput.SetValue("9001")
	if cmd := view.submitPort(); cmd != nil || view.editingPort || !strings.Contains(view.status, "cleanup required") {
		t.Fatalf("held direct port bypassed gate: cmd=%v editing=%v status=%q", cmd != nil, view.editingPort, view.status)
	}

	view.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: &no, Reserved: &no}})
	if cmd := view.selectNode("node-a"); cmd == nil {
		t.Fatal("released group did not restore vLLM proxy selection")
	}
	if got := len(view.Help()); got != 4 {
		t.Fatalf("released group help count = %d, want 4", got)
	}
}

func TestServingGroupHoldCancelsOpenVLLMPortEditor(t *testing.T) {
	view := newProxiesView(nil)
	focusVLLMProxy(t, view)
	no := false
	view.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: &no, Reserved: &no}})
	if cmd := view.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}); cmd == nil || !view.editingPort {
		t.Fatalf("released vLLM port editor did not open: cmd=%v editing=%v", cmd != nil, view.editingPort)
	}
	yes := true
	view.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: &no, Reserved: &yes, Reason: "journal changed"}})
	if view.editingPort || !strings.Contains(view.status, "journal changed") {
		t.Fatalf("new hold did not cancel editor: editing=%v status=%q", view.editingPort, view.status)
	}
}

func TestNonVLLMProxyMutationsDoNotInheritVLLMHold(t *testing.T) {
	view := newProxiesView(nil)
	view.SetSize(100, 20)
	view.engines[0].nodes = []proxyNode{{ID: "node-a"}}
	view.refreshRows(0)
	if view.engines[0].prefix == "vllm-proxy" {
		t.Fatal("test needs a non-vLLM first proxy")
	}
	if cmd := view.selectNode("node-a"); cmd == nil {
		t.Fatal("unknown vLLM group authority blocked a different proxy")
	}
}

func TestProxiesViewIncludesLlamaCppAndVLLM(t *testing.T) {
	v := newProxiesView(nil)
	if len(v.engines) != 4 {
		t.Fatalf("engines = %d, want 4", len(v.engines))
	}
	got := v.engines[2]
	if got.label != "llama.cpp" || got.prefix != "llamacpp-proxy" {
		t.Fatalf("engine[2] = {%q, %q}, want {llama.cpp, llamacpp-proxy}", got.label, got.prefix)
	}
	got = v.engines[3]
	if got.label != "vLLM" || got.prefix != "vllm-proxy" {
		t.Fatalf("engine[3] = {%q, %q}, want {vLLM, vllm-proxy}", got.label, got.prefix)
	}
}

func TestHandleNotificationLlamaCppProxy(t *testing.T) {
	v := newProxiesView(nil)
	params, err := json.Marshal(map[string]int{"port": 8080})
	if err != nil {
		t.Fatal(err)
	}
	v.handleNotification(&rpc.Message{
		Method: "llamacpp-proxy:ready",
		Params: params,
	})
	e := v.engines[2]
	if !e.ready || e.port != 8080 {
		t.Fatalf("llamacpp engine ready=%v port=%d, want ready :8080", e.ready, e.port)
	}
	if v.engines[0].ready {
		t.Fatal("ollama proxy must not consume llamacpp-proxy notifications")
	}
}
