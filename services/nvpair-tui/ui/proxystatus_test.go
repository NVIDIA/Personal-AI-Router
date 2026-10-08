// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-tui/rpc"
)

// TestProxyErrorTakesProxyDown is the regression guard for a strip that could
// only go green. Readiness was set on :ready and never cleared, so a proxy that
// failed kept advertising its port — pointing local clients at something that
// had stopped listening, which is the one thing this strip exists to tell them.
func TestProxyErrorTakesProxyDown(t *testing.T) {
	p := newProxyTracker()
	p.handleNotification(&rpc.Message{
		Method: "ollama-proxy:ready",
		Params: []byte(`{"port":11434}`),
	})
	port, ready := p.portForEngine("ollama")
	require.True(t, ready, "after ready")
	require.Equal(t, 11434, port)

	p.handleNotification(&rpc.Message{Method: "ollama-proxy:error", Params: []byte(`{}`)})
	port, ready = p.portForEngine("ollama")
	assert.False(t, ready, "proxy still reads ready after an error frame")
	assert.Equal(t, 11434, port, "the configured port should survive so the strip can name which endpoint is down")
	assert.Contains(t, p.strip(), "down", "strip must report the proxy down")

	// The other proxy is untouched.
	_, ready = p.portForEngine("lmstudio")
	assert.False(t, ready, "an ollama-proxy error changed the LM Studio proxy")
}

// TestProxyNotificationsAreScopedByPrefix checks the two proxies are told apart.
// Their methods share a suffix, so a mix-up would report one proxy's state
// against the other.
func TestProxyNotificationsAreScopedByPrefix(t *testing.T) {
	p := newProxyTracker()
	p.handleNotification(&rpc.Message{
		Method: "lmstudio-proxy:ready",
		Params: []byte(`{"port":1234}`),
	})

	port, ready := p.portForEngine("lmstudio")
	assert.True(t, ready)
	assert.Equal(t, 1234, port)
	_, ready = p.portForEngine("ollama")
	assert.False(t, ready, "an lmstudio-proxy frame marked the ollama proxy ready")
}

// TestProxyPushesUseTheFacadePrefix is the regression guard for the Ollama
// proxy's pushes being dropped.
//
// The broker forwards each facade's pushes as <engine>-proxy:<method>. Routing
// still looked for the bare "proxy:" of the single-engine proxy this replaced,
// so Ollama readiness and failure only ever reached the screen through the
// periodic status poll — and a proxy that died read as up until the next one.
func TestProxyPushesUseTheFacadePrefix(t *testing.T) {
	p := newProxyTracker()
	for _, e := range p.engines {
		p.handleNotification(&rpc.Message{Method: e.prefix + ":ready", Params: []byte(`{"port":4000}`)})
		port, ready := p.portForEngine(e.engine)
		assert.True(t, ready, "%s: a ready push under %q must be applied", e.engine, e.prefix)
		assert.Equal(t, 4000, port, "%s: a ready push under %q must be applied", e.engine, e.prefix)
	}

	p = newProxyTracker()
	p.handleNotification(&rpc.Message{Method: "proxy:ready", Params: []byte(`{"port":4000}`)})
	for _, e := range p.engines {
		_, ready := p.portForEngine(e.engine)
		assert.False(t, ready, "%s: an unaddressed push was attributed to it", e.engine)
	}
}

// TestFailedStatusReadTakesTheProxyDown checks a status read that failed is not
// ignored. Ignoring it left the strip showing the last good answer — a green
// port nothing may be listening on — for as long as the reads kept failing.
func TestFailedStatusReadTakesTheProxyDown(t *testing.T) {
	p := newProxyTracker()
	p.apply(proxyStatusMsg{idx: 0, ready: true, port: 11434})
	p.apply(proxyStatusMsg{idx: 0, err: errFake{}})
	port, ready := p.portForEngine("ollama")
	assert.False(t, ready, "the proxy still reads ready after its status read failed")
	assert.Equal(t, 11434, port, "the last known port should stay, shown as down")
}

// TestNotRunningKeepsTheLastKnownPort checks the broker's {ready:false, port:0}
// for a facade it is not running does not blank the port — the same rule the
// error push already followed, so the two paths no longer disagree.
func TestNotRunningKeepsTheLastKnownPort(t *testing.T) {
	p := newProxyTracker()
	p.apply(proxyStatusMsg{idx: 0, ready: true, port: 11434})
	p.apply(proxyStatusMsg{idx: 0, ready: false, port: 0})
	port, ready := p.portForEngine("ollama")
	assert.False(t, ready)
	assert.Equal(t, 11434, port, "last known port should be shown as down")
}

// TestPortForEngineDistinguishesDownFromUnknown checks a port is not treated as
// usable just because it is known: an engine with no proxy and a proxy that is
// down both have to read as unusable.
func TestPortForEngineDistinguishesDownFromUnknown(t *testing.T) {
	p := newProxyTracker()
	_, ready := p.portForEngine("ollama")
	assert.False(t, ready, "a proxy that has never reported reads as ready")
	port, ready := p.portForEngine("vllm")
	assert.False(t, ready)
	assert.Zero(t, port)
}
