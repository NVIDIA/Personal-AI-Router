// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package engines is the single source of truth for the identity of every
// inference engine PAIR fronts, shared by the proxy, the broker, the scheduler,
// and the TUI.
//
// These values are cross-process contracts, not labels. Name is the engine id
// carried in every engine-manager JSON-RPC call, in the per-engine model
// attribution on a discovery record, on a workload's Engine field, and as the
// basename of the engine-manager manifest that describes how to install and run
// the engine. A mismatch between any two of those does not fail to compile in a
// world where each component spells the id itself — it produces an engine that
// discovers, advertises, and never resolves a model owner. Declaring the set
// once, here, is what turns that class of bug into a build error.
//
// Design decisions this package encodes:
//
//   - Name is lower-case and unpunctuated ("lmstudio", not "lm-studio"). The
//     desktop UI uses its own hyphenated spelling for display types and bridges
//     to this one at the process boundary; that bridge is the only place the two
//     vocabularies are allowed to meet.
//
//   - There are two proxy identities, and which one a caller wants depends on
//     whether it is naming a *facade* or the *process*.
//
//     ComponentName ("<Name>-proxy") is the per-engine facade identity: the
//     JSON-RPC relay prefix, the error-ID prefix, and the TUI proxies-view tab.
//     These are per-engine because a client addressing "ollama-proxy:nodes/list"
//     or reading an "ollama-proxy:upstream-unreachable" error means that engine,
//     not whichever process happens to serve it.
//
//     ProxyComponent ("nvpair-proxy") is the process identity: the applog
//     component, the subprocess diagnostics name, the supervisor label, and the
//     crash key the TUI health view matches. These belong to the process
//     because one OS process serves every facade, so there is no single engine
//     to attribute them to; logs carry an engine field instead.
//
//     The errors-pipeline *source* deliberately stays per-facade. It is only
//     log context on the broker's side, and knowing which engine reported an
//     error is more useful there than knowing it came from the proxy process.
//
//     The supervisor label and the TUI crash key are matched against each
//     other, so they must move together — the broker stamps its crash id from
//     the label and the health view looks that id up by name. Moving one and
//     not the other is silent in both directions: the crash entry matches no
//     row, and the stale row can never leave "ok". It compiles, and every other
//     test passes. TestHealthProxyRowMatchesTheBrokerCrashIdentity in
//     nvpair-tui/ui is what turns this paragraph into something enforced.
//
//     These were one value until the proxy binary began hosting more than one
//     engine. Ollama's relay prefix was itself once the bare "proxy" while its
//     error IDs were already "ollama-proxy" — two near-identical identities
//     that coincided for LM Studio and diverged for Ollama, which is the trap
//     this package exists to prevent. Keep the distinction explicit: a new
//     identity belongs to the facade or to the process, never to both.
//
//   - FacadePort is the engine's own client-facing default — the port a stock
//     install listens on, and therefore the port PAIR's compatibility proxy
//     claims so existing clients keep working. EnginePortBase is where PAIR
//     relocates the engine to free that port, and the base of the next-free-port
//     search. The two must differ.
//
//   - All is ordered, and Ollama is first. The broker prepares managed ports in
//     this order, and Ollama's preparation reserves any inherited OLLAMA_HOST
//     alias that later engines must route around. Iterating a map here would
//     make that a coin flip and the test that guards it a flake.
package engines

import (
	"strings"

	"nvpair-shared/noderec"
)

// Engine is the cross-process identity of one inference engine.
type Engine struct {
	// Name is the canonical engine id used by engine-manager RPCs, discovery
	// model attribution, workload records, the scheduler, and the manifest
	// basename. It must match services/nvpair-engine-manager/manifests/<Name>.json.
	Name string

	// DisplayName is the engine's name in user-facing text. Not derivable from
	// Name: it carries capitalization and, for LM Studio, a space.
	DisplayName string

	// DiscoveryService is the compact TXT key under which a node advertises this
	// engine's port on the consolidated _nvpair-node record.
	DiscoveryService noderec.ServiceKey

	// FacadePort is the engine's stock client-facing port, which PAIR's proxy
	// claims in managed mode.
	FacadePort int

	// EnginePortBase is where PAIR relocates the engine so the proxy can take
	// FacadePort, and the base of the next-free-port search.
	EnginePortBase int

	// PortFile is the per-user file this engine's proxy persists its chosen
	// port to. Declared rather than derived from Name, because Ollama's
	// predates the multi-engine world and is the bare "proxy-port.json";
	// deriving it would rename the file under every existing install and
	// silently orphan the port a user had set. The proxy writes it and the
	// broker reads it when reserving ports away from the OLLAMA_HOST alias,
	// so the two must agree — which is why it lives here.
	PortFile string
}

// ProxyComponent is the proxy *process* identity, as distinct from the
// per-engine facade identity ComponentName returns. It matches the binary name
// in versions.json and the desktop binary inventory, so a log prefix, a support
// bundle, and a process listing all agree.
//
// It is what the applog component, supervisor label, subprocess diagnostics
// name, and TUI health crash key all spell. The last two are matched against
// each other; see the package comment.
const ProxyComponent = "nvpair-proxy"

// ComponentName is the engine's *facade* identity: the JSON-RPC relay prefix,
// the error-ID prefix, the errors-pipeline log source, and the TUI
// proxies-view tab. Always "<Name>-proxy".
//
// Not the supervisor label or the health crash key — those name the process,
// which hosts every facade. The persisted-port filename is never derived from
// it either; see PortFile.
func (e Engine) ComponentName() string { return e.Name + "-proxy" }

// all is the ordered engine set. Ollama is first; see the package comment.
var all = []Engine{
	{
		Name:             "ollama",
		DisplayName:      "Ollama",
		DiscoveryService: noderec.ServiceOllama,
		FacadePort:       11434,
		EnginePortBase:   11435,
		PortFile:         "proxy-port.json",
	},
	{
		Name:             "lmstudio",
		DisplayName:      "LM Studio",
		DiscoveryService: noderec.ServiceLMStudio,
		FacadePort:       1234,
		EnginePortBase:   1235,
		PortFile:         "lmstudio-proxy-port.json",
	},
}

// All returns the engine set in preparation order. The result is a copy, so a
// caller cannot reorder the shared table.
func All() []Engine {
	out := make([]Engine, len(all))
	copy(out, all)
	return out
}

// Names returns every engine id in preparation order.
func Names() []string {
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.Name
	}
	return out
}

// ByName resolves an engine id. Callers narrowing an id that arrived from a
// flag, a config file, or the wire must check ok rather than assuming.
func ByName(name string) (Engine, bool) {
	for _, e := range all {
		if e.Name == name {
			return e, true
		}
	}
	return Engine{}, false
}

// AddressMethod prefixes a facade-scoped JSON-RPC method with its engine, so a
// process hosting several facades can tell which one a message concerns.
//
// This is the proxy's *internal* wire addressing, deliberately not
// ComponentName: "ollama-proxy:" is what a client addresses the component by,
// and reusing it here would leave two different meanings on one string. The
// prefix matches the engine ids in facade/enable, so one message and its enable
// name the facade the same way.
func AddressMethod(engine, method string) string { return engine + ":" + method }

// SplitAddressedMethod separates a facade-scoped method's engine prefix from the
// method itself.
//
// A process-scoped method comes back with an empty engine and unchanged, so a
// caller distinguishes the two by the engine rather than by guessing from the
// method name. That matters because plenty of unaddressed methods contain a
// colon already — "errors:report", "discovery:subscribe", "workload:started" —
// and only a known engine id counts as an address.
func SplitAddressedMethod(method string) (engine, bare string) {
	name, rest, found := strings.Cut(method, ":")
	if !found {
		return "", method
	}
	if _, ok := ByName(name); !ok {
		return "", method
	}
	return name, rest
}
