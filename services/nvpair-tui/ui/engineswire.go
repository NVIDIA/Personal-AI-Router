// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// engineStatus mirrors nvpair-engine-manager's EngineStatus snapshot, the
// element of engine:get-installed and the engine:state-changed payload.
type engineStatus struct {
	Engine      string `json:"engine"`
	DisplayName string `json:"display_name"`
	Installed   bool   `json:"installed"`
	Running     bool   `json:"running"`
	Healthy     bool   `json:"healthy"`
	Port        int    `json:"port"`
}

func (e engineStatus) label() string {
	if e.DisplayName != "" {
		return e.DisplayName
	}
	return e.Engine
}

// modelsResult mirrors nvpair-engine-manager's ModelsResult, the engine:models
// reply and the engine:models-changed payload. LoadedByEngine names the models
// currently resident in memory, which the engine-manager polls for and pushes —
// so a client never has to poll to know what is loaded.
type modelsResult struct {
	Models         []string            `json:"models"`
	ModelsByEngine map[string][]string `json:"modelsByEngine"`
	LoadedByEngine map[string][]string `json:"loadedByEngine"`
}

// engineOp is one lifecycle request and how to describe it to the operator.
type engineOp struct {
	method string
	what   string
	// localOnly marks an operation the engine manager exposes no remote
	// equivalent for, so it is hidden on a peer's node rather than offered and
	// then failing.
	localOnly bool
}

// engineOps are the lifecycle operations, keyed by the local method name. The
// remote variants take a node and cover a deliberately smaller set: the manager
// has remote install/start/stop but no remote restart, uninstall, or port
// change, because those need process ownership on the target host.
var engineOps = map[string]engineOp{
	"install":   {method: "engine:install", what: "install"},
	"start":     {method: "engine:start", what: "start"},
	"stop":      {method: "engine:stop", what: "stop"},
	"restart":   {method: "engine:restart", what: "restart", localOnly: true},
	"uninstall": {method: "engine:uninstall", what: "uninstall", localOnly: true},
}

// remoteEngineMethods maps a local lifecycle method to its remote counterpart.
var remoteEngineMethods = map[string]string{
	"engine:install": "engine:remote-install",
	"engine:start":   "engine:remote-start",
	"engine:stop":    "engine:remote-stop",
}

// modelAction is one model operation: the remote method that performs it on a
// peer, and the operator-facing verb. The local engine:action name and params
// are per engine — see modelActionWire.
type modelAction struct {
	op     string
	remote string
	what   string
}

var modelActions = map[string]modelAction{
	"load": {op: "load", remote: "engine:remote-load-model", what: "load"},
	// "eject" to match the key's own label and the docs; the backend's method
	// keeps its own name. A key labelled eject that reports "unload requested"
	// leaves the operator wondering whether it did something else.
	"unload": {op: "unload", remote: "engine:remote-unload-model", what: "eject"},
	"delete": {op: "delete", remote: "engine:remote-delete-model", what: "delete"},
	"pull":   {op: "pull", remote: "engine:remote-pull-model", what: "download"},
}

// modelActionWire builds the local engine:action name and params for a model
// operation on a specific engine.
//
// The engines do not share a contract here, so one action name for both is
// wrong in ways that fail quietly. This mirrors nvpair-engine-manager's own
// modelActionWire, which is authoritative:
//
//   - Ollama has no load action at all. Warming a model is run_model with
//     streaming off; sending load_model just errors.
//   - Ollama only frees a model when keep_alive is 0. Without it the request
//     succeeds and the model stays resident.
//   - The two engines key the model differently — Ollama's delete takes "name",
//     LM Studio's takes "model" — so both are sent where a name is all that is
//     needed, which is also why a pull works on either engine.
func modelActionWire(engine, op, model string) map[string]any {
	both := map[string]string{"name": model, "model": model}
	switch op {
	case "load":
		if engine == "ollama" {
			return actionParams(engine, "run_model",
				map[string]any{"model": model, "stream": false})
		}
		return actionParams(engine, "load_model", map[string]any{"model": model})
	case "unload":
		if engine == "ollama" {
			return actionParams(engine, "unload_model",
				map[string]any{"model": model, "keep_alive": 0})
		}
		return actionParams(engine, "unload_model", map[string]any{"model": model})
	case "delete":
		return actionParams(engine, "delete_model", anyMap(both))
	default: // pull
		return actionParams(engine, "pull_model", anyMap(both))
	}
}

// actionParams wraps an engine:action envelope around per-action params.
func actionParams(engine, action string, params map[string]any) map[string]any {
	return map[string]any{"engine": engine, "action": action, "params": params}
}

func anyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// engineOpMsg is the outcome of a lifecycle or model command.
type engineOpMsg struct {
	what   string
	engine string
	err    error
	// detached marks a request that outlived its reply deadline but is still
	// running on the engine. Distinct from both success and failure: nothing
	// went wrong, and nothing has finished either.
	detached bool
}

// engineCmd issues a lifecycle request against a node. An empty node means this
// machine and uses the local method; otherwise the remote counterpart is used
// and the node travels in the params.
func engineCmd(client *rpc.Client, node, engine, method, op, what string) tea.Cmd {
	params := map[string]any{"engine": engine}
	if node != "" {
		remote, ok := remoteEngineMethods[method]
		if !ok {
			return func() tea.Msg {
				return engineOpMsg{what: what, engine: engine,
					err: errors.New("not supported on a remote node")}
			}
		}
		method = remote
		params["node"] = node
	}
	return call(client, method, params, func(_ *rpc.Message, err error) tea.Msg {
		return classifyOpResult(what, engine, op, err)
	})
}

// modelCmd issues a model operation against a node.
//
// A download that outlasts callTimeout is reported as detached rather than
// failed: a multi-gigabyte pull routinely exceeds the reply deadline while the
// engine keeps working, and the engine:pull-progress feed carries the real
// outcome. Reporting a failure there would be wrong — but so was the silence
// this replaced, which left the operator with no acknowledgement that the
// download had started at all, and nothing to distinguish it from a keystroke
// that missed.
func modelCmd(client *rpc.Client, node, engine string, act modelAction, model string) tea.Cmd {
	method := "engine:action"
	params := modelActionWire(engine, act.op, model)
	if node != "" {
		// The remote methods take the operation in the method name, so the
		// engine's own action vocabulary stays on the target's side.
		method = act.remote
		params = map[string]any{"node": node, "engine": engine, "model": model}
	}
	what := act.what + " " + model
	return call(client, method, params, func(_ *rpc.Message, err error) tea.Msg {
		return classifyOpResult(what, engine, act.op, err)
	})
}

// longRunningOps are the operations whose real duration is set by how much data
// has to move or how slow an engine is to become ready, not by the RPC.
//
// A deadline on one of these means the reply was slow, not that the work
// failed — the engine keeps going and reports the true outcome on its progress
// feed. Reporting a failure is actively misleading: the operator sees "load
// failed" at the same moment the model finishes loading.
//
// Ollama's load is `run_model` with streaming off, which does not answer until
// the model is resident and has produced a response, so a large model on cold
// storage exceeds the reply deadline routinely. Install downloads an engine.
// Delete and unload, by contrast, are quick, and a deadline there is a real
// fault worth surfacing.
var longRunningOps = map[string]bool{
	"pull":      true,
	"load":      true,
	"install":   true,
	"uninstall": true,
}

// classifyOpResult reports an operation as done, failed, or still running.
func classifyOpResult(what, engine, op string, err error) tea.Msg {
	if err != nil && longRunningOps[op] && errors.Is(err, context.DeadlineExceeded) {
		return engineOpMsg{what: what, engine: engine, detached: true}
	}
	return engineOpMsg{what: what, engine: engine, err: err}
}
