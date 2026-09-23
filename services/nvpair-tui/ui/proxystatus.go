// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"

	"nvpair-shared/engines"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

// proxyEngine is one engine's facade on the proxy the broker fronts. Every
// facade speaks the same control contract; only the JSON-RPC prefix and the
// label differ.
//
// A facade, not a process: one nvpair-proxy serves them all, so readiness and
// port are per-engine here while a crash is reported once against the process
// (see serviceWorkers). The prefix is the per-engine ComponentName, because
// addressing "ollama-proxy:nodes/list" means that engine regardless of which
// process happens to serve it.
type proxyEngine struct {
	label  string // "Ollama" / "LM Studio"
	prefix string // "ollama-proxy" / "lmstudio-proxy"
	// engine is the engine-manager engine name this facade fronts, used to line
	// a facade up with the engine it serves.
	engine string
	ready  bool
	port   int
}

// proxyTracker keeps every facade's readiness and listen port current from the
// broker. It is a shared component rather than a tab: where a request is served
// belongs with the jobs, while which port to listen on belongs with the rest of
// the service configuration.
//
// It deliberately does not track upstreams. The proxies' node lists are a
// second, staler view of the machines the Nodes tab already owns — and one that
// kept showing peers from a cluster this node had left.
type proxyTracker struct {
	engines []*proxyEngine
}

type proxyStatusMsg struct {
	idx   int
	ready bool
	port  int
	err   error
}

// engineDisplayName is an engine's wire id rendered the way the operator sees
// it elsewhere.
//
// The Jobs and Errors tabs name engines too, and neither has an engine list to
// resolve a label from — they receive the id on the wire and nothing else. The
// pairing is already fixed here, in the proxy inventory, so this reads it from
// the same place rather than repeating it. An unknown id passes through, which
// is what a new engine would produce until this list caught up.
func engineDisplayName(engine string) string {
	want := strings.ToLower(strings.TrimSpace(engine))
	for _, e := range newProxyTracker().engines {
		if e.engine == want {
			return e.label
		}
	}
	return engine
}

// newProxyTracker builds one entry per engine, in the shared table's order, so
// an engine added there appears here rather than being silently absent from
// every screen that reads a facade's port.
func newProxyTracker() *proxyTracker {
	all := engines.All()
	out := make([]*proxyEngine, 0, len(all))
	for _, e := range all {
		out = append(out, &proxyEngine{
			label:  e.DisplayName,
			prefix: e.ComponentName(),
			engine: e.Name,
		})
	}
	return &proxyTracker{engines: out}
}

// indexForEngine finds the proxy fronting an engine-manager engine, or -1. Each
// proxy serves exactly one engine type, which is what lets a node's engine list
// show the client-facing port beside each engine's own.
func (p *proxyTracker) indexForEngine(engine string) int {
	want := strings.ToLower(strings.TrimSpace(engine))
	for i, e := range p.engines {
		if e.engine == want {
			return i
		}
	}
	return -1
}

// portForEngine is the listen port of the proxy fronting an engine, and whether
// that proxy is up. A port with the proxy down is not an endpoint a client can
// use, so callers distinguish the two.
func (p *proxyTracker) portForEngine(engine string) (int, bool) {
	idx := p.indexForEngine(engine)
	if idx < 0 {
		return 0, false
	}
	return p.engines[idx].port, p.engines[idx].ready
}

// init subscribes to every engine's facade and fetches its current status.
// Subscribing is idempotent at the broker, so several consumers may each hold a
// tracker.
func (p *proxyTracker) init(client *rpc.Client) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(p.engines)*2)
	for i, e := range p.engines {
		cmds = append(cmds,
			call(client, e.prefix+":subscribe", nil, func(_ *rpc.Message, _ error) tea.Msg { return nil }),
			p.statusCmd(client, i),
		)
	}
	return tea.Batch(cmds...)
}

func (p *proxyTracker) statusCmd(client *rpc.Client, idx int) tea.Cmd {
	e := p.engines[idx]
	return call(client, e.prefix+":get-status", nil, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return proxyStatusMsg{idx: idx, err: err}
		}
		var r struct {
			Ready bool `json:"ready"`
			Port  int  `json:"port"`
		}
		_ = decodeParams(msg.Result, &r)
		return proxyStatusMsg{idx: idx, ready: r.Ready, port: r.Port}
	})
}

// A proxy's listen port is not changed from here. It is one of the three
// fields engine:apply-settings writes together against a revision, so it goes
// through the node detail's settings editor with the server port and the launch
// arguments. This tracker only reads: readiness, and the port in force.

// apply folds a status reply into the tracker.
func (p *proxyTracker) apply(msg proxyStatusMsg) {
	if msg.err != nil || msg.idx < 0 || msg.idx >= len(p.engines) {
		return
	}
	p.engines[msg.idx].ready = msg.ready
	p.engines[msg.idx].port = msg.port
}

// handleNotification consumes a proxy push. A ready frame carries the port
// directly; an error frame takes the proxy down.
//
// Both directions are handled deliberately. Readiness only ever being set meant
// a proxy that died stayed green with its old port on screen, pointing clients
// at an endpoint that had stopped listening — the one thing this strip exists to
// tell them.
func (p *proxyTracker) handleNotification(msg *rpc.Message) {
	idx := -1
	switch {
	case strings.HasPrefix(msg.Method, "lmstudio-proxy:"):
		idx = 1
	case strings.HasPrefix(msg.Method, "proxy:"):
		idx = 0
	default:
		return
	}
	switch {
	case strings.HasSuffix(msg.Method, ":ready"):
		var r struct {
			Port int `json:"port"`
		}
		_ = decodeParams(msg.Params, &r)
		p.engines[idx].ready = true
		if r.Port != 0 {
			p.engines[idx].port = r.Port
		}
	case strings.HasSuffix(msg.Method, ":error"):
		// The port is kept: it is still the configured value, and showing the
		// proxy as down at a known port is more useful than blanking it.
		p.engines[idx].ready = false
	}
}

// refreshCmd re-reads every facade's status.
//
// Polled on the view's tick as well as pushed, because a proxy going away does
// not always announce it — a crash, or a broker restart, produces no error frame
// — and a readiness strip that can only be corrected by a push stays wrong
// indefinitely.
func (p *proxyTracker) refreshCmd(client *rpc.Client) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(p.engines))
	for i := range p.engines {
		cmds = append(cmds, p.statusCmd(client, i))
	}
	return tea.Batch(cmds...)
}

// strip is the one-line summary of where local clients should point, and whether
// anything is listening. Routing mode is stated because it is not adjustable:
// the scheduler and proxies own placement.
func (p *proxyTracker) strip() string {
	parts := make([]string, 0, len(p.engines))
	for _, e := range p.engines {
		if e.ready {
			parts = append(parts, fmt.Sprintf("%s %s", e.label, statusOKStyle.Render(fmt.Sprintf(":%d", e.port))))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", e.label, statusErrStyle.Render("down")))
	}
	return strings.Join(parts, "   ") + footerStyle.Render("   routing=automatic")
}
