// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

func releaseTUIVLLMGroupGate(view *enginesView) {
	no := false
	view.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: &no, Reserved: &no}})
}

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
	yes := true
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: &yes})
	v.Update(engineOpMsg{what: "pull invalid-reference", engine: "llamacpp", err: errors.New("expected owner/repository")})
	v.Update(engineModelsMsg{engine: "llamacpp", names: []string{"cached"}, loaded: map[string]bool{}})
	if !strings.Contains(v.status, "expected owner/repository") {
		t.Fatal("inventory refresh hid the actionable failure")
	}
}

func TestModelSnapshotSettlesObservedLoad(t *testing.T) {
	yes := true
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: &yes})
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
	yes, no := true, false
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: &yes})
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
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: &no})
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
	yes := true
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: &yes})
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
	yes := true
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", DisplayName: "llama.cpp", Installed: true, Managed: &yes, Acceleration: "cuda", Devices: []string{"CUDA0: NVIDIA GB10 (122564 MiB, 512 MiB free)"}})
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
	yes := true
	v := newEnginesView(nil)
	v.SetSize(90, 20)
	v.merge(engineStatus{Engine: "llamacpp", Installed: true, Managed: &yes})
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

func TestVLLMExactPullInputAndCancellableProgress(t *testing.T) {
	yes := true
	view := newEnginesView(nil)
	view.SetSize(120, 20)
	view.merge(engineStatus{Engine: "vllm", Installed: true, Managed: &yes})
	releaseTUIVLLMGroupGate(view)
	if cmd := view.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}); cmd == nil || !view.pulling || !strings.Contains(view.input.Placeholder, "40-character") {
		t.Fatalf("managed vLLM pull input = cmd:%v pulling:%v placeholder:%q", cmd != nil, view.pulling, view.input.Placeholder)
	}
	view.input.SetValue("owner/model@main")
	if cmd := view.submitPull(); cmd != nil || !strings.Contains(view.status, "40-character") {
		t.Fatalf("mutable revision was dispatched: cmd=%v status=%q", cmd != nil, view.status)
	}
	const op = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	view.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:pull-progress", Params: json.RawMessage(`{"engine":"vllm","stage":"resuming","percent":5,"message":"owner/model@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operationId":"` + op + `"}`)}})
	if !view.pullActive || view.pullOpID != op || !strings.Contains(view.View(), "resuming") {
		t.Fatalf("progress did not expose cancellation binding: active=%v op=%q view=%q", view.pullActive, view.pullOpID, view.View())
	}
	found := false
	for _, binding := range view.Help() {
		found = found || binding.Help().Key == "c"
	}
	if !found {
		t.Fatal("active vLLM pull did not expose cancel control")
	}
	view.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:pull-progress", Params: json.RawMessage(`{"engine":"vllm","stage":"canceled","message":"partial download retained for resume","operationId":"` + op + `"}`)}})
	if view.pullActive || !strings.Contains(view.View(), "retained for resume") {
		t.Fatalf("cancel terminal state = active:%v view:%q", view.pullActive, view.View())
	}
	view.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:pull-progress", Params: json.RawMessage(`{"engine":"vllm","stage":"success","percent":100,"message":"nvidia/Qwen3.8-Flash-Next-NVFP4@fc694b54fb0174e0913e6adf86691ef85a4ead47 (license: nvidia-open-model-license; notices retained with model)","operationId":"` + op + `"}`)}})
	if !strings.Contains(view.View(), "nvidia-open-model-license") || !strings.Contains(view.View(), "notices retained") {
		t.Fatalf("success did not expose retained license notice: %q", view.View())
	}
}

func TestValidVLLMHubModelRequiresImmutableCommit(t *testing.T) {
	if !validVLLMHubModel("nvidia/Qwen3.8-Flash-Next-NVFP4@fc694b54fb0174e0913e6adf86691ef85a4ead47") {
		t.Fatal("fixed Qwen candidate rejected")
	}
	for _, value := range []string{"owner/model", "owner/model@main", "owner/model@AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "owner/other/model@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if validVLLMHubModel(value) {
			t.Errorf("mutable/invalid model %q accepted", value)
		}
	}
}

func TestVLLMCancelUsesExactProgressBinding(t *testing.T) {
	const model = "owner/model@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const operationID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	request, result := runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		view := newEnginesView(client)
		view.pullActive, view.pullModel, view.pullOpID = true, model, operationID
		return view.cancelVLLMPull()
	}, map[string]bool{"accepted": true})
	if request.Method != "engine:action" || !strings.Contains(string(request.Params), `"action":"cancel_pull"`) ||
		!strings.Contains(string(request.Params), `"model":"`+model+`"`) || !strings.Contains(string(request.Params), `"operationId":"`+operationID+`"`) {
		t.Fatalf("cancel request = %s %s", request.Method, request.Params)
	}
	if msg, ok := result.(engineOpMsg); !ok || msg.err != nil || msg.what != "cancel pull" {
		t.Fatalf("cancel result = %#v", result)
	}
}

func TestVLLMControlsFailClosedOnMissingAuthority(t *testing.T) {
	status := engineStatus{Engine: "vllm", Running: true}
	if status.enabled() {
		t.Fatal("missing vLLM enabled state inherited running=true")
	}
	if status.managed() {
		t.Fatal("missing vLLM managed state granted authority")
	}
	for _, action := range []string{"install", "uninstall", "start", "stop", "restart", "update"} {
		if got := engineActionBlock(status, action); got == "" {
			t.Errorf("%s was not blocked for ambiguous vLLM", action)
		}
	}
}

func TestVLLMManagedLifecycleAndLegacyFallbackRemainAvailable(t *testing.T) {
	yes := true
	no := false
	managed := engineStatus{Engine: "vllm", Managed: &yes, Enabled: &yes}
	for _, action := range []string{"start", "stop", "restart", "update"} {
		if got := engineActionBlock(managed, action); got != "" {
			t.Errorf("managed vLLM %s blocked: %s", action, got)
		}
	}
	legacy := engineStatus{Engine: "ollama", Running: true}
	if !legacy.enabled() || !legacy.managed() || engineActionBlock(legacy, "install") != "" {
		t.Fatalf("legacy fallback changed: %+v", legacy)
	}
	if (engineStatus{Engine: "ollama", Running: true, Enabled: &no}).enabled() {
		t.Fatal("explicit legacy disabled state was ignored")
	}
	if !(engineStatus{Engine: "lmstudio", Enabled: &yes}).enabled() {
		t.Fatal("explicit legacy enabled state was ignored")
	}
}

func TestVLLMUpdateRequiresManagement(t *testing.T) {
	yes := true
	no := false
	view := newEnginesView(nil)
	view.SetSize(100, 20)
	view.merge(engineStatus{Engine: "vllm", Managed: &yes})
	releaseTUIVLLMGroupGate(view)
	if cmd, handled := view.handleAction(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}}); !handled || cmd == nil {
		t.Fatalf("managed update did not dispatch: handled=%v cmd=%v", handled, cmd != nil)
	}

	view = newEnginesView(nil)
	view.SetSize(100, 20)
	view.merge(engineStatus{Engine: "vllm", Managed: &no, Adopted: &yes})
	if cmd, handled := view.handleAction(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}}); !handled || cmd != nil {
		t.Fatalf("adopted update was not blocked: handled=%v cmd=%v", handled, cmd != nil)
	}
}

func TestVLLMInstallKeyRequiresExplicitCapabilityAndOwnership(t *testing.T) {
	no := false
	yes := true
	for _, status := range []engineStatus{
		{Engine: "vllm", InstallSupported: &yes},
		{Engine: "vllm", Managed: &no, Adopted: &yes, InstallSupported: &yes},
	} {
		view := newEnginesView(nil)
		view.SetSize(100, 20)
		view.merge(status)
		cmd, handled := view.handleAction(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
		if !handled || cmd != nil || view.status == "" {
			t.Fatalf("ambiguous/adopted install was not rejected: handled=%v cmd=%v status=%q", handled, cmd != nil, view.status)
		}
	}
	installable := engineStatus{Engine: "vllm", Managed: &no, Adopted: &no, InstallSupported: &yes}
	if blocked := engineActionBlock(installable, "install"); blocked != "" {
		t.Fatalf("explicitly supported non-adopted install blocked: %s", blocked)
	}
	view := newEnginesView(nil)
	view.SetSize(100, 20)
	view.merge(installable)
	releaseTUIVLLMGroupGate(view)
	if cmd, handled := view.handleAction(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}}); !handled || cmd == nil {
		t.Fatalf("supported non-adopted install did not dispatch: handled=%v cmd=%v", handled, cmd != nil)
	}
	installed := installable
	installed.Installed = true
	if blocked := engineActionBlock(installed, "install"); blocked == "" {
		t.Fatal("installed vLLM unexpectedly offered install")
	}
}

func TestVLLMEngineActionsStayHiddenUntilServingGroupReleased(t *testing.T) {
	yes := true
	view := newEnginesView(nil)
	view.SetSize(100, 20)
	view.merge(engineStatus{Engine: "vllm", Managed: &yes})
	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}}
	if cmd, handled := view.handleAction(key); !handled || cmd != nil || len(view.Help()) != 0 {
		t.Fatalf("unknown group authority was not held: handled=%v cmd=%v help=%d", handled, cmd != nil, len(view.Help()))
	}
	no := false
	view.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: &no, Reserved: &yes, Reason: "cleanup required"}})
	if cmd, handled := view.handleAction(key); !handled || cmd != nil || !strings.Contains(view.status, "cleanup required") {
		t.Fatalf("retained group was not held: handled=%v cmd=%v status=%q", handled, cmd != nil, view.status)
	}
	releaseTUIVLLMGroupGate(view)
	if cmd, handled := view.handleAction(key); !handled || cmd == nil {
		t.Fatalf("released group did not restore managed update: handled=%v cmd=%v", handled, cmd != nil)
	}
}

func TestEngineVersionIsVisible(t *testing.T) {
	view := newEnginesView(nil)
	view.SetSize(120, 20)
	view.merge(engineStatus{Engine: "vllm", DisplayName: "vLLM", Version: "0.29.0"})
	if got := view.View(); !strings.Contains(got, "0.29.0") {
		t.Fatalf("engine version missing from TUI: %q", got)
	}
}

func TestVLLMUnavailableInstallReasonIsVisible(t *testing.T) {
	no := false
	view := newEnginesView(nil)
	view.SetSize(160, 20)
	view.merge(engineStatus{
		Engine: "vllm", DisplayName: "vLLM", InstallSupported: &no,
		InstallReason: "Managed vLLM has no native Windows recipe; WSL2 child ownership is not implemented.",
	})
	got := view.View()
	for _, text := range []string{"managed vllm prerequisite", "no native Windows recipe", "WSL2 child ownership"} {
		if !strings.Contains(got, text) {
			t.Fatalf("vLLM prerequisite %q missing from TUI: %q", text, got)
		}
	}
}

func TestVLLMSelectionRequiresStoppedManagedEngineAndReleasedGroup(t *testing.T) {
	yes := true
	key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'M'}}
	view := newEnginesView(nil)
	view.SetSize(100, 20)
	view.merge(engineStatus{Engine: "vllm", Installed: true, Managed: &yes})
	if cmd := view.handleKey(key); cmd != nil || view.selecting || !strings.Contains(view.status, "held") {
		t.Fatalf("unknown group authority allowed selection: cmd=%v selecting=%v status=%q", cmd != nil, view.selecting, view.status)
	}
	releaseTUIVLLMGroupGate(view)
	if cmd := view.handleKey(key); cmd == nil || !view.selecting || !view.CapturingInput() {
		t.Fatalf("stopped managed vLLM did not enter typed selection: cmd=%v selecting=%v", cmd != nil, view.selecting)
	}

	running := newEnginesView(nil)
	running.SetSize(100, 20)
	running.merge(engineStatus{Engine: "vllm", Installed: true, Running: true, Managed: &yes})
	releaseTUIVLLMGroupGate(running)
	if cmd := running.handleKey(key); cmd != nil || running.selecting || !strings.Contains(running.status, "stopped PAIR-managed") {
		t.Fatalf("running vLLM selection was not refused: cmd=%v selecting=%v status=%q", cmd != nil, running.selecting, running.status)
	}
}

func TestVLLMSelectionUsesExactRetainedIDAndRefreshesSnapshot(t *testing.T) {
	yes := true
	view := newEnginesView(nil)
	view.SetSize(100, 20)
	view.merge(engineStatus{Engine: "vllm", Installed: true, Managed: &yes})
	releaseTUIVLLMGroupGate(view)
	view.selecting = true
	view.input.SetValue("owner/model@revision")
	if cmd := view.submitVLLMSelection(); cmd == nil || view.selecting || view.input.Value() != "" {
		t.Fatalf("selection was not dispatched once: cmd=%v selecting=%v input=%q", cmd != nil, view.selecting, view.input.Value())
	}
	if cmd := view.Update(engineOpMsg{what: "select model", engine: "vllm"}); cmd == nil {
		t.Fatal("successful selection did not refresh authoritative engine status")
	}
}

func TestVLLMSelectedModelIsVisibleFromEngineStatus(t *testing.T) {
	view := newEnginesView(nil)
	view.SetSize(120, 20)
	view.merge(engineStatus{Engine: "vllm", DisplayName: "vLLM", SelectedModel: "owner/model@revision"})
	if got := view.View(); !strings.Contains(got, "selected model: owner/model@revision") {
		t.Fatalf("selected model missing from TUI: %q", got)
	}
}

func TestVLLMSelectionRPCContainsOnlyRetainedModelID(t *testing.T) {
	const model = "owner/model@revision"
	request, result := runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		view := newEnginesView(client)
		view.selecting = true
		view.input.SetValue(model)
		return view.submitVLLMSelection()
	}, map[string]any{
		"engine": "vllm", "model": model, "selected": true,
		"status": map[string]any{"engine": "vllm", "selected_model": model},
	})
	if request.Method != "engine:vllm-select-model" || string(request.Params) != `{"model":"owner/model@revision"}` {
		t.Fatalf("selection request = %s %s", request.Method, request.Params)
	}
	if msg, ok := result.(engineOpMsg); !ok || msg.err != nil || msg.what != "select model" {
		t.Fatalf("selection result = %#v", result)
	}
}

func TestVLLMSelectionTimeoutCoversRetainedContentVerification(t *testing.T) {
	if vllmModelSelectionTimeout < 10*time.Minute || vllmModelSelectionTimeout <= callTimeout {
		t.Fatalf("model-selection timeout = %s, want full verification budget", vllmModelSelectionTimeout)
	}
}
