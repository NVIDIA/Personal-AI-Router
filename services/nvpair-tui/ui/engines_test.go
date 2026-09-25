// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"nvpair-tui/rpc"
	"strings"
	"testing"
	"time"
)

func TestLongEngineCallKeepsObservedLoadTruth(t *testing.T) {
	if engineOperationTimeout <= 30*time.Minute || callTimeout != 35*time.Second {
		t.Fatal("engine operation budget must cover the backend without extending ordinary calls")
	}
	v := newEnginesView(nil)
	v.pendingLoadEngine, v.pendingLoadModel = "llamacpp", "owner/model:Q4"
	timedOut := engineOpMsg{what: "load_model owner/model:Q4", engine: "llamacpp", err: context.DeadlineExceeded}
	v.Update(timedOut)
	if v.pendingLoadModel == "" || !strings.Contains(v.status, "outcome unknown") || strings.Contains(v.status, "failed") {
		t.Fatalf("client timeout invented backend failure: %s", v.status)
	}
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:models-changed", Params: json.RawMessage(`{"engine":"llamacpp","models":{"models":["owner/model:Q4"],"modelsByEngine":{"llamacpp":["owner/model:Q4"]},"loadedByEngine":{"llamacpp":["owner/model:Q4"]}}}`)}})
	v.Update(timedOut)
	if v.pendingLoadModel != "" || v.status != "Model loaded: owner/model:Q4" {
		t.Fatalf("late client timeout erased observed success: %s", v.status)
	}
	v.pendingLoadEngine, v.pendingLoadModel = "llamacpp", "other/model:Q4"
	v.Update(engineOpMsg{what: "load_model other/model:Q4", engine: "llamacpp", err: errors.New("vendor rejected load")})
	if v.pendingLoadModel != "" || !strings.Contains(v.status, "failed") {
		t.Fatal("actual backend error was hidden")
	}
}

func TestUnknownEngineProgressIsIndeterminate(t *testing.T) {
	for _, method := range []string{"engine:install-progress", "engine:pull-progress"} {
		v := newEnginesView(nil)
		v.Update(NotificationMsg{Msg: &rpc.Message{Method: method, Params: json.RawMessage(`{"engine":"llamacpp","stage":"installing","percent":-1}`)}})
		if strings.Contains(v.status, "%") {
			t.Fatalf("unknown progress shown as percent: %s", v.status)
		}
		v.Update(NotificationMsg{Msg: &rpc.Message{Method: method, Params: json.RawMessage(`{"engine":"llamacpp","stage":"installing","percent":50}`)}})
		if !strings.Contains(v.status, "50%") {
			t.Fatal("known progress omitted")
		}
	}
}

func TestModelRefreshPreservesActionFailure(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: true})
	v.Update(engineOpMsg{what: "pull invalid-reference", engine: "llamacpp", err: errors.New("expected owner/repository")})
	v.Update(engineModelsMsg{engine: "llamacpp", names: []string{"cached"}, loaded: map[string]bool{}})
	if !strings.Contains(v.status, "expected owner/repository") {
		t.Fatal("inventory refresh hid the actionable failure")
	}
}

func TestModelSnapshotSettlesObservedLoad(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: true})
	v.pendingLoadEngine, v.pendingLoadModel = "llamacpp", "cached"
	v.Update(engineModelsMsg{engine: "llamacpp", names: []string{"cached"}, loaded: map[string]bool{"cached": true}})
	if v.pendingLoadModel != "" || v.status != "Model loaded: cached" {
		t.Fatal("observed loaded snapshot left a pending load")
	}
}

func TestLlamaLoadAcceptanceWaitsForObservation(t *testing.T) {
	v := newEnginesView(nil)
	v.pendingLoadEngine, v.pendingLoadModel = "llamacpp", "owner/model:Q4"
	v.Update(engineOpMsg{what: "load_model owner/model:Q4", engine: "llamacpp"})
	if v.pendingLoadModel == "" || !strings.Contains(v.status, "waiting") {
		t.Fatal("RPC acceptance completed the load")
	}
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:models-changed", Params: json.RawMessage(`{"engine":"llamacpp","models":{"models":["owner/model:Q4"],"modelsByEngine":{"llamacpp":["owner/model:Q4"]},"loadedByEngine":{"llamacpp":["owner/model:Q4"]}}}`)}})
	if v.pendingLoadModel != "" || !strings.Contains(v.status, "Model loaded") {
		t.Fatal("loaded observation did not settle action")
	}
	v.Update(engineOpMsg{what: "load_model owner/model:Q4", engine: "llamacpp"})
	if strings.Contains(v.status, "waiting") {
		t.Fatal("late RPC response reopened a settled load")
	}
}

func TestLlamaManagedActionsAndOwnership(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: true})
	for _, tc := range []struct {
		key    rune
		action string
	}{{'L', "load_model"}, {'e', "unload_model"}, {'d', "delete_model"}, {'c', "cancel_pull"}, {'I', "import_model"}} {
		v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{tc.key}})
		if !v.pulling || v.modelAction != tc.action {
			t.Fatalf("key %c: action %s", tc.key, v.modelAction)
		}
		v.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	}
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: false})
	if cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}}); cmd != nil || v.pulling {
		t.Fatal("external engine accepted model mutation")
	}
	if cmd, handled := v.handleAction(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}); cmd != nil || !handled {
		t.Fatal("external engine accepted stop")
	}
}

// TestNoManagedLlamaUpdateControl pins the scoped-down engine surface: the TUI
// offers install, lifecycle, uninstall and model actions for managed llama, but
// no runtime update key, help entry, or engine:action{action:"update"} call.
func TestNoManagedLlamaUpdateControl(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: true})
	for _, b := range v.Help() {
		if strings.Contains(strings.ToLower(b.Help().Desc), "update") {
			t.Fatalf("help still advertises an update control: %q", b.Help().Desc)
		}
		for _, k := range b.Keys() {
			if k == "U" {
				t.Fatal("U is still bound in the engines view")
			}
		}
	}
	if cmd, handled := v.handleAction(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}}); handled || cmd != nil {
		t.Fatal("U still dispatches an engine lifecycle action")
	}
	_ = v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	if v.pulling || v.status != "" {
		t.Fatalf("U changed engine view state: pulling=%v status=%q", v.pulling, v.status)
	}
}

func TestLlamaAccelerationLabel(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", DisplayName: "llama.cpp", Installed: true, Managed: true, Acceleration: "cuda", Devices: []string{"CUDA0: NVIDIA GB10 (122564 MiB, 512 MiB free)"}})
	v.merge(engineStatus{Engine: "ollama", DisplayName: "Ollama", Installed: true, Acceleration: "cuda"})
	if rows := v.table.Rows(); rows[0][0] != "llama.cpp CUDA" || rows[1][0] != "Ollama" {
		t.Fatalf("rows = %v", rows)
	}
	v.merge(engineStatus{Engine: "llamacpp", DisplayName: "llama.cpp"})
	if rows := v.table.Rows(); rows[0][0] != "llama.cpp" {
		t.Fatalf("stale acceleration after uninstall: %v", rows)
	}
}

func TestLlamaModelInventorySeparatesDownloadedAndLoaded(t *testing.T) {
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: true})
	v.Update(engineModelsMsg{names: []string{"cold", "resident"}, loaded: map[string]bool{"resident": true}})
	rows := v.models.Rows()
	if rows[0][1] != "downloaded" || rows[1][1] != "loaded" {
		t.Fatalf("inventory = %v", rows)
	}
}

// TestPullParamsSendsBothKeys guards the LM Studio pull fix: the pull params
// must carry the model under BOTH "name" (Ollama's /api/pull body key) and
// "model" (LM Studio's `lms get {model}` CLI placeholder). Sending only "name"
// silently ran `lms get "" --yes`, so a TUI pull never reached LM Studio.
func TestPullParamsSendsBothKeys(t *testing.T) {
	p := pullParams("lmstudio", "owner/model")

	if p["engine"] != "lmstudio" {
		t.Fatalf("engine = %v, want lmstudio", p["engine"])
	}
	if p["action"] != "pull_model" {
		t.Fatalf("action = %v, want pull_model", p["action"])
	}
	inner, ok := p["params"].(map[string]string)
	if !ok {
		t.Fatalf("params = %T, want map[string]string", p["params"])
	}
	if inner["name"] != "owner/model" {
		t.Fatalf(`params["name"] = %q, want "owner/model"`, inner["name"])
	}
	if inner["model"] != "owner/model" {
		t.Fatalf(`params["model"] = %q, want "owner/model" (LM Studio reads this key)`, inner["model"])
	}
}
