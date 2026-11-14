// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Owner-side ingress plane for Custom endpoints (Task 10).
//
// These pin the broker half of the peer routing path: what reconcileCustomAdvertise
// hands the custom facade so a peer request that arrives over cluster mTLS can be
// forwarded to the right user-configured endpoint. Four properties carry the design:
//
//   - The pushed set is (probe-healthy slot ∩ persisted config): health alone
//     would forward to an endpoint nobody configured, and configuration alone
//     would forward to one the prober no longer vouches for. Both halves must
//     agree before the ingress releases a request toward a third-party host and
//     injects a credential.
//   - Keys ride the push because this is the broker↔proxy stdio link. Asserting
//     the key PRESENT here is the other half of the contract custombridge_test.go
//     pins by asserting it ABSENT in everything client-facing.
//   - The "backends" key always serializes as an array, never null: the proxy
//     decodes a null as the single-backend class, so clearing a set with null
//     would silently leave the stale set forwarding.
//   - The push is change-gated (serialized set including the keys, plus proxy
//     handle identity) and resets while the facade is down, so steady state
//     keeps the stdio link quiet while replaced or respawned proxies re-learn
//     the set without any event.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-ui-broker/relay"
)

// customIngressBroker wires a broker for reconcile-driven ingress pushes: a
// custom facade that has reported ready on port 8000, a frame-recording fake
// proxy that answers requests (so the inline bridge call resolves the way a
// real proxy would), and a persisted custom-endpoint config.
func customIngressBroker(t *testing.T, entries []persistedCustomEndpoint) (*Broker, string, chan bridgeFrame) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configs", "custom-endpoints.json")
	if len(entries) > 0 {
		require.NoError(t, writePersistedCustomEndpoints(path, entries))
	}
	seen := make(chan bridgeFrame, 8)
	proxy := newCustomProxyFaker(t, seen)
	proxy.facadeState = readyFacade(customProxyProfile.Name, 8000)
	b := &Broker{
		codec:               NewCodec(rwDiscard{}),
		store:               newDiscoveryStore(),
		regCache:            relay.NewRegistrationCache(),
		customEndpointsPath: path,
	}
	b.setEngineProxyHandle(customProxyProfile, proxy)
	return b, path, seen
}

// replaceCustomProxy points the broker's custom-facade handle at a fresh
// frame-recording fake process, simulating a respawned proxy.
func replaceCustomProxy(t *testing.T, b *Broker) chan bridgeFrame {
	t.Helper()
	seen := make(chan bridgeFrame, 8)
	proxy := newCustomProxyFaker(t, seen)
	proxy.facadeState = readyFacade(customProxyProfile.Name, 8000)
	b.setEngineProxyHandle(customProxyProfile, proxy)
	return seen
}

// decodeBackendSet parses a recorded push in exactly the shape the proxy
// handler decodes: Backends behind a pointer, so a wire "null" decodes to nil
// and these tests fail on the same ambiguity the proxy would silently mishandle.
func decodeBackendSet(t *testing.T, frame bridgeFrame) (string, *[]proxyBackendEntry) {
	t.Helper()
	var params struct {
		Engine   string               `json:"engine"`
		Backends *[]proxyBackendEntry `json:"backends"`
	}
	require.NoError(t, json.Unmarshal([]byte(frame.params), &params), "push params: %v", frame.params)
	return params.Engine, params.Backends
}

const backendSetMethod = "custom:node/set-local-backend"

// A configured, probe-healthy endpoint set must reach the facade in one frame:
// addressed to the custom facade, carrying each endpoint's URL, credential,
// and declared models, ordered by endpoint key so the same config produces the
// same bytes on every tick and every proxy build.
func TestReconcileCustomAdvertise_PushesKeyedBackendSetToCustomFacade(t *testing.T) {
	b, _, seen := customIngressBroker(t, []persistedCustomEndpoint{
		{ID: "ep-medium", BaseURL: "https://gpu.example:8443", APIKey: "sk-medium"},
		{ID: "ep-small", BaseURL: "http://10.0.0.9:8000", APIKey: "sk-small"},
	})
	seedCustomEndpoint(b, "ep-small", "small-model")
	seedCustomEndpoint(b, "ep-medium", "medium-model", "shared-model")

	b.reconcileCustomAdvertise()

	frames := takeBridgeFrames(t, seen, 3*time.Second)
	require.Len(t, frames, 1, "one converged backend set must reach the proxy exactly once")
	frame := frames[0]
	require.Equal(t, backendSetMethod, frame.method,
		"the set is facade-scoped and must be addressed to the custom facade")

	engine, backends := decodeBackendSet(t, frame)
	assert.Equal(t, "custom", engine, "payload names its engine class")
	require.NotNil(t, backends, `"backends" must serialize as an array, never null — the proxy reads null as the single-backend class`)
	require.Len(t, *backends, 2)
	first, second := (*backends)[0], (*backends)[1]
	assert.Equal(t, "https://gpu.example:8443", first.URL, "entries are ordered by endpoint key")
	assert.Equal(t, "sk-medium", first.APIKey, "the key rides this stdio hop because only the owner may inject it")
	assert.Equal(t, []string{"medium-model", "shared-model"}, first.ModelNames,
		"per-endpoint attribution is the routing contract peers relied on")
	assert.Equal(t, "http://10.0.0.9:8000", second.URL)
	assert.Equal(t, "sk-small", second.APIKey)
	assert.Equal(t, []string{"small-model"}, second.ModelNames)
}

// Once the proxy has been given the current set, a facade that keeps reporting
// ready must not repaint the wire: the gate keys on (serialized set + proxy
// identity), and an unchanged state shipped every five seconds forever is noise
// that also drags the credential along on every quiet tick.
func TestReconcileCustomAdvertise_KeepsSteadyStateIngressQuiet(t *testing.T) {
	b, _, seen := customIngressBroker(t, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000", APIKey: "sk-one"},
	})
	seedCustomEndpoint(b, "custom:http://10.0.0.9:8000", "m-one")

	b.reconcileCustomAdvertise()
	require.NotEmpty(t, takeBridgeFrames(t, seen, 3*time.Second), "the baseline set must be pushed")

	b.reconcileCustomAdvertise()
	b.reconcileCustomAdvertise()
	assert.Empty(t, takeBridgeFrames(t, seen, 2*time.Second), "an unchanged set must not re-push")
}

// A credential rotation (same URL, same models, new key) is still a change:
// pushing it is what keeps the ingress from presenting a revoked key forever.
func TestReconcileCustomAdvertise_RepushesWhenOnlyAnEndpointKeyChanges(t *testing.T) {
	b, path, seen := customIngressBroker(t, []persistedCustomEndpoint{
		{ID: "ep-small", BaseURL: "http://10.0.0.9:8000", APIKey: "sk-one"},
	})
	seedCustomEndpoint(b, "ep-small", "small-model")

	b.reconcileCustomAdvertise()
	require.NotEmpty(t, takeBridgeFrames(t, seen, 3*time.Second), "baseline push with the old key")

	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{ID: "ep-small", BaseURL: "http://10.0.0.9:8000", APIKey: "sk-two"},
	}))
	b.reconcileCustomAdvertise()

	frames := takeBridgeFrames(t, seen, 3*time.Second)
	require.Len(t, frames, 1, "a key-only edit must re-push — the gate key includes the credential")
	_, backends := decodeBackendSet(t, frames[0])
	require.NotNil(t, backends)
	require.Len(t, *backends, 1)
	assert.Equal(t, "sk-two", (*backends)[0].APIKey, "the re-push carries the current config key")
}

// Neither half of the truth alone may forward: a slot with no persisted config
// (the config is the truth about what may be used) and a configured endpoint
// with no probe slot (unprobed, unhealthy, or model-less) build no set. The
// empty-empty state adopts quietly with a live facade, so the wire stays silent.
func TestReconcileCustomAdvertise_NeitherUnprobedNorUnconfiguredBuildsASet(t *testing.T) {
	t.Run("probe slot without matching config is not routed", func(t *testing.T) {
		b, _, seen := customIngressBroker(t, nil)
		seedCustomEndpoint(b, "ep-unconfigured", "orphan-model")

		b.reconcileCustomAdvertise()
		b.reconcileCustomAdvertise()
		assert.Empty(t, takeBridgeFrames(t, seen, 2*time.Second),
			"a model the prober saw but nobody configured must never become an ingress forwarding target")
	})

	t.Run("configured endpoint without a probe slot is not routed", func(t *testing.T) {
		b, _, seen := customIngressBroker(t, []persistedCustomEndpoint{
			{ID: "ep-ghost", BaseURL: "http://10.0.0.5:8000", APIKey: "sk-ghost"},
		})

		b.reconcileCustomAdvertise()
		b.reconcileCustomAdvertise()
		assert.Empty(t, takeBridgeFrames(t, seen, 2*time.Second),
			"an endpoint with no healthy probe is not routable (D1: no toggle, health gates)")
	})
}

// Removing the last endpoint must CLEAR the proxy's set, not just stop
// repeating it: a stale set would keep answering peers after the owner stopped
// routing. The clear travels as an empty "backends" array — spelling null here
// would decode as the single-backend class on the proxy and silently keep the
// old set.
func TestReconcileCustomAdvertise_ClearsStaleBackendSetWhenEndpointsGone(t *testing.T) {
	b, _, seen := customIngressBroker(t, []persistedCustomEndpoint{
		{ID: "ep-small", BaseURL: "http://10.0.0.9:8000", APIKey: "sk-one"},
	})
	seedCustomEndpoint(b, "ep-small", "small-model")

	b.reconcileCustomAdvertise()
	require.NotEmpty(t, takeBridgeFrames(t, seen, 3*time.Second), "baseline set pushed")

	b.store.Remove("ep-small", sourceCustom)
	b.reconcileCustomAdvertise()

	frames := takeBridgeFrames(t, seen, 3*time.Second)
	require.Len(t, frames, 1, "the lost set must be explicitly cleared")
	require.Equal(t, backendSetMethod, frames[0].method)
	assert.Contains(t, frames[0].params, `"backends":[]`, "the clear is an empty array, never null")
	engine, backends := decodeBackendSet(t, frames[0])
	assert.Equal(t, "custom", engine)
	require.NotNil(t, backends, "a null clear would fall through to the single-backend class on the proxy")
	assert.Empty(t, *backends)

	// And the cleared state stays quiet — the clear already converged the proxy.
	b.reconcileCustomAdvertise()
	assert.Empty(t, takeBridgeFrames(t, seen, 2*time.Second), "an already-empty set must not re-push")
}

// A respawned proxy process starts with no backend set. Identity is half of the
// gate key, so the unchanged set re-pushes to the replacement without any event
// — this is how ingress state survives a proxy restart, mirroring the
// replaced-engine-manager replay for the model inventory.
func TestReconcileCustomAdvertise_RepushesToReplacedProxyProcess(t *testing.T) {
	b, _, seen1 := customIngressBroker(t, []persistedCustomEndpoint{
		{ID: "ep-small", BaseURL: "http://10.0.0.9:8000", APIKey: "sk-one"},
	})
	seedCustomEndpoint(b, "ep-small", "small-model")

	b.reconcileCustomAdvertise()
	require.NotEmpty(t, takeBridgeFrames(t, seen1, 3*time.Second), "baseline push to the first proxy")

	seen2 := replaceCustomProxy(t, b)
	b.reconcileCustomAdvertise()

	frames := takeBridgeFrames(t, seen2, 3*time.Second)
	require.Len(t, frames, 1, "the replacement proxy must re-receive the current set")
	assert.Equal(t, backendSetMethod, frames[0].method)
}

// With the facade down there is nothing to receive a set: the reconcile must
// go silent AND reset the gate, so when a facade comes back — even with the
// exact same endpoints — it re-learns the set instead of inheriting a gate
// keyed to a state its proxy never got.
func TestReconcileCustomAdvertise_FacadeDownSuppressesPushAndResetRelearns(t *testing.T) {
	b, _, seen1 := customIngressBroker(t, []persistedCustomEndpoint{
		{ID: "ep-small", BaseURL: "http://10.0.0.9:8000", APIKey: "sk-one"},
	})
	seedCustomEndpoint(b, "ep-small", "small-model")

	b.reconcileCustomAdvertise()
	require.NotEmpty(t, takeBridgeFrames(t, seen1, 3*time.Second), "baseline push while up")

	b.setEngineProxyHandle(customProxyProfile, nil)
	b.reconcileCustomAdvertise()
	b.reconcileCustomAdvertise()

	seen2 := replaceCustomProxy(t, b)
	b.reconcileCustomAdvertise()

	assert.Empty(t, takeBridgeFrames(t, seen1, 1*time.Second),
		"frames queued at the old proxy must not be treated as delivered to the new one")
	frames := takeBridgeFrames(t, seen2, 3*time.Second)
	require.Len(t, frames, 1, "a re-enabled facade must re-learn the set, not inherit the down-period gate")
	assert.Equal(t, backendSetMethod, frames[0].method)
}
