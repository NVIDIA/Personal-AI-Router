// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"

	"testing"

	"github.com/stretchr/testify/require"
)

// TestModelLifecycle exercises the full model lifecycle through the engine
// Action API against the stateful fake engine, the Ollama-shape (HTTP) way:
//
//	list -> pull -> list(now present) -> run/generate -> delete -> list(gone)
//
// This proves the runner correctly drives list/download/use/delete and that
// each step is observable. LM Studio's cmd-shape actions (lms get/ls/load)
// and guarded filesystem deletion are covered separately, as is llama.cpp's
// query-parameter delete endpoint.
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
	require.NoError(t, ex.Start(ctx, "fake"), "start")

	list := func() string {
		r, err := ex.Action(ctx, "fake", "list_models", nil)
		require.NoError(t, err, "list_models")
		return string(r)
	}
	act := func(name, params string) json.RawMessage {
		r, err := ex.Action(ctx, "fake", name, json.RawMessage(params))
		require.NoError(t, err, "action (%v, %v)", name, err)
		return r
	}

	const model = "demo-model:1b"
	require.NotContains(t, list(), model, "model")
	act("pull_model", `{"name":"`+model+`"}`)
	require.Contains(t, list(), model, "model")
	act("run_model", `{"model":"`+model+`","stream":false}`)
	require.Contains(t, string(act("loaded_models", "null")), model, "model")
	act("unload_model", `{"model":"`+model+`","keep_alive":0}`)
	require.NotContains(t, string(act("loaded_models", "null")), model, "model")
	act("delete_model", `{"name":"`+model+`"}`)
	require.NotContains(t, list(), model, "model")
}
