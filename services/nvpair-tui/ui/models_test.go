// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"errors"
	"strings"
	"testing"

	"nvpair-tui/rpc"
)

func TestModelsViewLoadsEveryEngineAndResidency(t *testing.T) {
	var inventory modelInventory
	raw := []byte(`{"modelsByEngine":{"ollama":["b:latest","a:latest"],"llamacpp":["owner/model-GGUF:Q4_K_M"]},"loadedByEngine":{"ollama":["b:latest"],"llamacpp":["owner/model-GGUF:Q4_K_M"]}}`)
	if err := decodeParams(raw, &inventory); err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	view := newModelsView(nil)
	view.Update(modelsLoadedMsg{inventory: inventory})
	if len(view.rows) != 3 {
		t.Fatalf("model rows = %v, want three rows", view.rows)
	}
	if row := view.rows[0]; row.engine != "llamacpp" || row.model != "owner/model-GGUF:Q4_K_M" || row.state != "loaded" {
		t.Errorf("first row = %+v, want loaded llama.cpp model", row)
	}
	if row := view.rows[1]; row.engine != "ollama" || row.model != "a:latest" || row.state != "idle" {
		t.Errorf("second row = %+v, want idle Ollama a:latest", row)
	}
	if row := view.rows[2]; row.model != "b:latest" || row.state != "loaded" {
		t.Errorf("third row = %+v, want loaded Ollama b:latest", row)
	}
	view.table.SetCursor(1)
	selected, ok := view.selectedModel()
	if !ok || selected != view.rows[1] {
		t.Fatalf("selected model = %+v, %v; want second row", selected, ok)
	}
}

func TestModelsChangedReplacesResidencySnapshot(t *testing.T) {
	view := newModelsView(nil)
	view.apply(modelInventory{
		ByEngine:       map[string][]string{"llamacpp": {"owner/model:Q4_K_M"}},
		LoadedByEngine: map[string][]string{"llamacpp": {}},
	})
	view.Update(NotificationMsg{Msg: &rpc.Message{
		Method: "engine:models-changed",
		Params: []byte(`{"engine":"llamacpp","models":{"modelsByEngine":{"llamacpp":["owner/model:Q4_K_M"]},"loadedByEngine":{"llamacpp":["owner/model:Q4_K_M"]}}}`),
	}})
	if len(view.rows) != 1 || view.rows[0].state != "loaded" {
		t.Fatalf("rows after residency push = %+v, want one loaded model", view.rows)
	}
}

func TestModelActionRequestsMatchEngineContracts(t *testing.T) {
	test := func(name, engine, what, wantAction string, wantStream, wantKeepAlive bool) {
		t.Run(name, func(t *testing.T) {
			request := newModelActionRequest(modelRow{engine: engine, model: "owner/model"}, what)
			if request.Engine != engine || request.Action != wantAction || request.Params.Model != "owner/model" {
				t.Fatalf("request = %+v, want %s %s for owner/model", request, engine, wantAction)
			}
			if (request.Params.Stream != nil) != wantStream {
				t.Errorf("stream presence = %v, want %v", request.Params.Stream != nil, wantStream)
			}
			if request.Params.Stream != nil && *request.Params.Stream {
				t.Error("Ollama load must send stream=false")
			}
			if (request.Params.KeepAlive != nil) != wantKeepAlive {
				t.Errorf("keep_alive presence = %v, want %v", request.Params.KeepAlive != nil, wantKeepAlive)
			}
			if request.Params.KeepAlive != nil && *request.Params.KeepAlive != 0 {
				t.Errorf("keep_alive = %d, want 0", *request.Params.KeepAlive)
			}
		})
	}
	test("llama.cpp load", "llamacpp", "load", "load_model", false, false)
	test("llama.cpp unload", "llamacpp", "unload", "unload_model", false, false)
	test("Ollama load", "ollama", "load", "run_model", true, false)
	test("Ollama unload", "ollama", "unload", "unload_model", false, true)
}

func TestModelsViewEmptyAndErrorStates(t *testing.T) {
	view := newModelsView(nil)
	if got := view.View(); !strings.Contains(got, "No local models") {
		t.Fatalf("empty view = %q, want empty-state guidance", got)
	}

	view.Update(modelsLoadedMsg{err: errors.New("manager unavailable")})
	if got := view.View(); !strings.Contains(got, "manager unavailable") {
		t.Fatalf("error view = %q, want backend error", got)
	}
}
