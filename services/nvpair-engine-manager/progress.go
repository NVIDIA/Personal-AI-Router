// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "sync"

// ProgressEvent is one progress frame for a long-running engine operation
// (install or model pull). The same event feeds two consumers: the local
// engine:install-progress notification path (this node's own UI) and the ec
// streaming handlers that relay live progress to a remote initiator.
type ProgressEvent struct {
	Engine  string `json:"engine"`
	Op      string `json:"op"` // "install" | "pull"
	Stage   string `json:"stage,omitempty"`
	Percent int    `json:"percent"`
	Message string `json:"message,omitempty"`
}

// wirePercentIncluded reports whether a pull progress percent should appear on
// the wire. Zero means indeterminate (CLI pulls without byte progress); negative
// values are terminal error sentinels. Install progress still always includes
// percent (including 0) via emitInstallProgress.
func wirePercentIncluded(pct int) bool {
	return pct > 0 || pct < 0
}

// progressHub fans per-engine ProgressEvents to transient subscribers. An ec
// streaming handler subscribes for the duration of one remote op; the executor
// publishes as the install/pull makes progress. Publishing must never block the
// operation, so a slow or full subscriber simply drops frames — progress is
// advisory, and the terminal result is delivered out-of-band (as the stream's
// final frame / the RPC response), not through the hub.
//
// Subscriptions are scoped by engine, not by operation id: a subscriber watching
// "ollama" sees every ollama frame, so if a local pull and a remote ec pull of
// the same engine run concurrently, the remote initiator's stream may interleave
// the local pull's advisory frames. That's intentional and harmless — each
// stream's authoritative terminal result is the out-of-band final frame, never a
// hub frame — and installs (emitInstallProgress) already have this property.
type progressHub struct {
	mu   sync.Mutex
	next int
	subs map[int]*progressSub
}

type progressSub struct {
	engine string
	ch     chan ProgressEvent
}

func newProgressHub() *progressHub {
	return &progressHub{subs: make(map[int]*progressSub)}
}

// subscribe registers interest in one engine's progress (empty engine = all).
// The returned cancel removes the subscription and closes the channel; callers
// must invoke it when the op ends so the hub doesn't leak subscribers.
func (h *progressHub) subscribe(engine string) (<-chan ProgressEvent, func()) {
	sub := &progressSub{engine: engine, ch: make(chan ProgressEvent, 64)}
	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = sub
	h.mu.Unlock()
	return sub.ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(sub.ch)
		}
		h.mu.Unlock()
	}
}

// publish delivers ev to every subscriber watching ev.Engine, dropping the
// frame for any subscriber whose buffer is full. Send and close are both done
// under h.mu, so publish never sends on a channel closed by cancel.
func (h *progressHub) publish(ev ProgressEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		if sub.engine != "" && sub.engine != ev.Engine {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
		}
	}
}
