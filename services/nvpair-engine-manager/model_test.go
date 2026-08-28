// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestModelLifecycle exercises the full model lifecycle through the engine
// Action API against the stateful fake engine, the Ollama-shape (HTTP) way:
//
//	list -> pull -> list(now present) -> run/generate -> delete -> list(gone)
//
// This proves the runner correctly drives list/download/use/delete and that
// each step is observable. LM Studio's cmd-shape actions (lms get/ls/load)
// are covered by TestCmdAction; LM Studio has no CLI model-delete (vendor
// limitation), which is why only Ollama exposes delete_model.
func TestModelLifecycle(t *testing.T) {
	m := testEngineManifest(fakeEngineBin) // already has list_models (GET /api/tags)
	m.Actions["pull_model"] = Action{HTTP: &ActionHTTP{Method: "POST", Path: "/api/pull"}}
	m.Actions["run_model"] = Action{HTTP: &ActionHTTP{Method: "POST", Path: "/api/generate"}}
	m.Actions["unload_model"] = Action{HTTP: &ActionHTTP{Method: "POST", Path: "/api/generate"}}
	m.Actions["loaded_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/api/ps"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
	m.Actions["delete_model"] = Action{HTTP: &ActionHTTP{Method: "DELETE", Path: "/api/delete"}}

	ex := newTestExecutor(t, m)
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}

	list := func() string {
		r, err := ex.Action(ctx, "fake", "list_models", nil)
		if err != nil {
			t.Fatalf("list_models: %v", err)
		}
		return string(r)
	}
	act := func(name, params string) json.RawMessage {
		r, err := ex.Action(ctx, "fake", name, json.RawMessage(params))
		if err != nil {
			t.Fatalf("action %s: %v", name, err)
		}
		return r
	}

	const model = "demo-model:1b"
	if strings.Contains(list(), model) {
		t.Fatalf("model %q unexpectedly present before pull", model)
	}
	act("pull_model", `{"name":"`+model+`"}`)
	if !strings.Contains(list(), model) {
		t.Fatalf("model %q not listed after pull", model)
	}
	act("run_model", `{"model":"`+model+`","stream":false}`)
	if !strings.Contains(string(act("loaded_models", "null")), model) {
		t.Fatalf("model %q not loaded after warm-up run", model)
	}
	act("unload_model", `{"model":"`+model+`","keep_alive":0}`)
	if strings.Contains(string(act("loaded_models", "null")), model) {
		t.Fatalf("model %q still loaded after unload", model)
	}
	act("delete_model", `{"name":"`+model+`"}`)
	if strings.Contains(list(), model) {
		t.Fatalf("model %q still listed after delete", model)
	}
}
