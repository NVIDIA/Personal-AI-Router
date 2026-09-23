// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"nvpair-tui/rpc"
)

// TestProxyErrorTakesProxyDown is the regression guard for a strip that could
// only go green. Readiness was set on :ready and never cleared, so a proxy that
// failed kept advertising its port — pointing local clients at something that
// had stopped listening, which is the one thing this strip exists to tell them.
func TestProxyErrorTakesProxyDown(t *testing.T) {
	p := newProxyTracker()
	p.handleNotification(&rpc.Message{
		Method: "proxy:ready",
		Params: []byte(`{"port":11434}`),
	})
	if port, ready := p.portForEngine("ollama"); !ready || port != 11434 {
		t.Fatalf("after ready: port=%d ready=%v", port, ready)
	}

	p.handleNotification(&rpc.Message{Method: "proxy:error", Params: []byte(`{}`)})
	port, ready := p.portForEngine("ollama")
	if ready {
		t.Error("proxy still reads ready after an error frame")
	}
	if port != 11434 {
		t.Errorf("port = %d; the configured port should survive so the strip can name "+
			"which endpoint is down", port)
	}
	if !contains(p.strip(), "down") {
		t.Errorf("strip does not report the proxy down: %s", p.strip())
	}

	// The other proxy is untouched.
	if _, ready := p.portForEngine("lmstudio"); ready {
		t.Error("an ollama-proxy error changed the LM Studio proxy")
	}
}

// TestProxyNotificationsAreScopedByPrefix checks the two proxies are told apart.
// Their methods share a suffix and "lmstudio-proxy:" would match a naive
// "proxy:" test, so a mix-up would report one proxy's state against the other.
func TestProxyNotificationsAreScopedByPrefix(t *testing.T) {
	p := newProxyTracker()
	p.handleNotification(&rpc.Message{
		Method: "lmstudio-proxy:ready",
		Params: []byte(`{"port":1234}`),
	})

	if port, ready := p.portForEngine("lmstudio"); !ready || port != 1234 {
		t.Errorf("lmstudio proxy: port=%d ready=%v, want 1234/true", port, ready)
	}
	if _, ready := p.portForEngine("ollama"); ready {
		t.Error("an lmstudio-proxy frame marked the ollama proxy ready")
	}
}

// TestPortForEngineDistinguishesDownFromUnknown checks a port is not treated as
// usable just because it is known: an engine with no proxy and a proxy that is
// down both have to read as unusable.
func TestPortForEngineDistinguishesDownFromUnknown(t *testing.T) {
	p := newProxyTracker()
	if _, ready := p.portForEngine("ollama"); ready {
		t.Error("a proxy that has never reported reads as ready")
	}
	if port, ready := p.portForEngine("vllm"); ready || port != 0 {
		t.Errorf("unknown engine: port=%d ready=%v, want 0/false", port, ready)
	}
}
