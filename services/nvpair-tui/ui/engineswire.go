// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"errors"
	"fmt"

	"nvpair-shared/engines"
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

// modelWire is how one engine spells each model operation: the engine:action
// name, and the params that action takes for a given model.
type modelWire map[string]func(model string) (action string, params map[string]any)

// modelWires is every engine's spelling of the model operations, keyed by
// engine id.
//
// The engines do not share a contract here, so one spelling for both is wrong
// in ways that fail quietly. This mirrors nvpair-engine-manager's own
// modelActionWire, which is authoritative for the remote path. An engine is
// supported by adding its entry; one without an entry is refused rather than
// sent another engine's vocabulary, and TestEveryEngineHasAModelWire fails
// until the entry exists.
var modelWires = map[string]modelWire{
	engines.NameOllama: {
		// Ollama has no load action at all. Warming a model is run_model with
		// streaming off; sending load_model just errors.
		"load": func(model string) (string, map[string]any) {
			return "run_model", map[string]any{"model": model, "stream": false}
		},
		// Ollama only frees a model when keep_alive is 0. Without it the
		// request succeeds and the model stays resident.
		"unload": func(model string) (string, map[string]any) {
			return "unload_model", map[string]any{"model": model, "keep_alive": 0}
		},
		"delete": nameAndModel("delete_model"),
		"pull":   nameAndModel("pull_model"),
	},
	engines.NameLMStudio: {
		"load":   modelOnly("load_model"),
		"unload": modelOnly("unload_model"),
		"delete": nameAndModel("delete_model"),
		"pull":   nameAndModel("pull_model"),
	},
	// llama.cpp's router takes the model under "model" alone, delete included.
	// Its delete puts the params in the query string, where a second key would
	// be sent to the engine as a parameter it does not take.
	engines.NameLlamaCPP: {
		"load":   modelOnly("load_model"),
		"unload": modelOnly("unload_model"),
		"delete": modelOnly("delete_model"),
		"pull":   modelOnly("pull_model"),
	},
}

// downloadExamples show what a download name looks like for each engine. The
// engines spell them very differently: an Ollama tag, a Hugging Face repo, and
// a repo with its quantization after a colon.
var downloadExamples = map[string]string{
	engines.NameOllama:   "llama3.2",
	engines.NameLMStudio: "lmstudio-community/Qwen3-8B-GGUF",
	engines.NameLlamaCPP: "ggml-org/gemma-3-1b-it-GGUF:Q4_K_M",
}

// downloadPrompt is the placeholder for the download-by-name field, with an
// example in the engine's own spelling where there is one.
func downloadPrompt(engine, label string) string {
	if example, ok := downloadExamples[engine]; ok {
		return fmt.Sprintf("model name for %s (e.g. %s)", label, example)
	}
	return "model name for " + label
}

// modelOnly is an action that takes the model under "model".
func modelOnly(action string) func(string) (string, map[string]any) {
	return func(model string) (string, map[string]any) {
		return action, map[string]any{"model": model}
	}
}

// nameAndModel is an action that takes the model under both keys. The engines
// name it differently — Ollama's delete and pull take "name", LM Studio's take
// "model" — so both are sent where a name is all the action needs.
func nameAndModel(action string) func(string) (string, map[string]any) {
	return func(model string) (string, map[string]any) {
		return action, map[string]any{"name": model, "model": model}
	}
}

// modelActionWire builds the local engine:action envelope for a model
// operation on a specific engine.
func modelActionWire(engine, op, model string) (map[string]any, error) {
	wire, ok := modelWires[engine]
	if !ok {
		return nil, fmt.Errorf("model operations are not supported for engine %q", engine)
	}
	build, ok := wire[op]
	if !ok {
		return nil, fmt.Errorf("unknown model operation %q", op)
	}
	action, params := build(model)
	return map[string]any{"engine": engine, "action": action, "params": params}, nil
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
	what := act.what + " " + model
	// The remote methods take the operation in the method name, so the
	// engine's own action vocabulary stays on the target's side.
	method := act.remote
	params := map[string]any{"node": node, "engine": engine, "model": model}
	if node == "" {
		method = "engine:action"
		local, err := modelActionWire(engine, act.op, model)
		if err != nil {
			return func() tea.Msg { return engineOpMsg{what: what, engine: engine, err: err} }
		}
		params = local
	}
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
//
// Start and restart belong here too. The engine manager answers only once the
// engine passes its readiness probe, and the manifests allow that probe six
// hundred seconds for Ollama and sixty for LM Studio — the desktop app gives
// the same calls a fourteen-minute envelope for exactly this reason. Held to
// the thirty-five-second reply deadline, a cold start behind a slow probe
// reported failure while the engine went on to come up.
//
// Stop, delete, and unload, by contrast, are quick — stop is bounded by the
// manifest's five-second grace — and a deadline there is a real fault worth
// surfacing.
var longRunningOps = map[string]bool{
	"pull":      true,
	"load":      true,
	"install":   true,
	"uninstall": true,
	"start":     true,
	"restart":   true,
}

// classifyOpResult reports an operation as done, failed, or still running.
func classifyOpResult(what, engine, op string, err error) tea.Msg {
	if err != nil && longRunningOps[op] && errors.Is(err, context.DeadlineExceeded) {
		return engineOpMsg{what: what, engine: engine, detached: true}
	}
	return engineOpMsg{what: what, engine: engine, err: err}
}
