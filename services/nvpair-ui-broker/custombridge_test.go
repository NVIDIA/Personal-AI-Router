// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Bridge-plane contract for Custom endpoints (Task 6).
//
// These pin the second half of the custom plane: what the broker does with the
// probe results nvpair-manual-nodes emits. Four properties carry the design:
//
//   - Health and configuration together gate routability (no on/off toggle):
//     only a healthy endpoint with models becomes a proxy candidate and
//     claims its store routing slot; unhealthy, model-less, or unconfigured
//     states retract both, never leaving a stale candidate. Neither claim
//     makes the endpoint a node: Snapshot() skips custom-only records, so
//     the Add Node list, the TUI node views, and the scheduler's node
//     universe stay machine-only. Slot assertions here read customSlots()
//     — the internal routing view — while the snapshot assertions pin that
//     the client wire never carries an endpoint.
//   - The proxy-bound candidate payload is the one place a key may ride a
//     JSON-RPC frame that leaves the broker's config code — the local
//     broker↔proxy stdio link, keyed by the proxy's own 401×3 budget that
//     AddManual resets. Nothing client-facing (discovery wire, event
//     payloads) ever carries it: the add-manual assertion requires the key
//     present on that one hop, while the snapshot assertions pin the wire
//     both free of the endpoint and free of its key.
//   - The broker's persisted config is the key's source of truth: the
//     credential in the add-manual payload is read from the config at
//     bridge time, so an edit that swaps the key re-issues with the new one,
//     and an event naming no configured endpoint cannot resurrect a
//     removed candidate.
//   - Removal is decided by the config path, not the probe path: a removed
//     entry drops its store/proxy claims immediately, so a down or
//     event-silent prober cannot strand a candidate that still points at
//     the endpoint.

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bridgeTestKey = "sk-bridge-secret"

type bridgeFrame struct {
	method string
	params string
}

// newCustomProxyFaker builds a proxyProcess over one end of a net.Pipe whose
// other end records every downward frame and answers immediately — the same
// fake shape newCustomProberFaker uses for the worker side. A recorded frame
// is a frame the real proxy would receive addressed to the custom facade.
func newCustomProxyFaker(t *testing.T, seen chan bridgeFrame) *proxyProcess {
	t.Helper()
	brokerSide, serverSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = serverSide.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(brokerSide))}
	go proxy.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(serverSide)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.ID == nil || !msg.IsRequest() {
				continue
			}
			select {
			case seen <- bridgeFrame{method: msg.Method, params: string(msg.Params)}:
			case <-time.After(2 * time.Second):
			}
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()
	return proxy
}

// customBridgeBroker wires a broker with a live discovery store, a persisted
// custom-endpoint config (or none), and the custom-facade handle pointed at a
// frame-recording fake proxy, so event handling can be driven end to end
// through manualNotificationHandler — the real notification entry point,
// switch included.
func customBridgeBroker(t *testing.T, entries []persistedCustomEndpoint) (*Broker, chan bridgeFrame) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configs", "custom-endpoints.json")
	if len(entries) > 0 {
		require.NoError(t, writePersistedCustomEndpoints(path, entries))
	}
	seen := make(chan bridgeFrame, 8)
	proxy := newCustomProxyFaker(t, seen)
	b := &Broker{
		codec:               NewCodec(rwDiscard{}),
		store:               newDiscoveryStore(),
		customEndpointsPath: path,
	}
	b.setEngineProxyHandle(customProxyProfile, proxy)
	return b, seen
}

// takeBridgeFrames drains frames for d, waiting up to 3s for the first so a
// missing frame fails as "nothing sent", not an empty-slice coincidence.
func takeBridgeFrames(t *testing.T, seen chan bridgeFrame, d time.Duration) []bridgeFrame {
	t.Helper()
	var frames []bridgeFrame
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case frame := <-seen:
			frames = append(frames, frame)
		case <-timer.C:
			return frames
		}
	}
}

func updatedParams(id, baseUrl string, healthy bool, models ...string) json.RawMessage {
	payload := map[string]any{
		"id":      id,
		"baseUrl": baseUrl,
		"healthy": healthy,
	}
	if len(models) > 0 {
		payload["models"] = models
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

func snapshotJSON(b *Broker) string {
	data, err := json.Marshal(b.store.Snapshot())
	if err != nil {
		panic(err)
	}
	return string(data)
}

// A healthy endpoint with models becomes both a routing candidate (add-manual
// addressed to the custom facade, carrying the key because that local hop is
// where the key is allowed) and a store routing slot whose models feed the
// advertised union and the ingress backend set — but not the discovery wire,
// which lists machines. Reading the key from persisted config here — not from
// the event, which carries none — is the plumbing that keeps it out of every
// probe payload.
func TestCustomUpdated_HealthyEndpointBridgesCandidateAndStoreBucket(t *testing.T) {
	b, seen := customBridgeBroker(t, []persistedCustomEndpoint{
		{Name: "lab", BaseURL: "http://10.0.0.9:8000", APIKey: bridgeTestKey},
	})

	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))

	frames := takeBridgeFrames(t, seen, 3*time.Second)
	require.Len(t, frames, 1, "expected exactly one downward frame")
	require.Equal(t, "custom:node/add-manual", frames[0].method,
		"candidate must reach the custom facade addressed, not the ollama default")

	var candidate struct {
		ID        string   `json:"id"`
		Host      string   `json:"host"`
		Port      int      `json:"port"`
		Addresses []string `json:"addresses"`
		Models    []string `json:"models"`
		APIKey    string   `json:"apiKey"`
		Scheme    string   `json:"scheme"`
	}
	require.NoError(t, json.Unmarshal([]byte(frames[0].params), &candidate))
	assert.Equal(t, "custom:http://10.0.0.9:8000", candidate.ID, "candidate id is the endpoint key")
	assert.Equal(t, "10.0.0.9", candidate.Host)
	assert.Equal(t, 8000, candidate.Port, "the port travels; it is never re-inferred downstream")
	assert.Equal(t, []string{"10.0.0.9"}, candidate.Addresses)
	assert.Equal(t, []string{"tinyllm"}, candidate.Models)
	assert.Equal(t, "http", candidate.Scheme)
	assert.Equal(t, bridgeTestKey, candidate.APIKey,
		"the key rides the broker↔proxy link; its absence would break key-gated endpoints")

	slots := b.store.customSlots()
	require.Contains(t, slots, "custom:http://10.0.0.9:8000",
		"a healthy endpoint claims its store routing slot")
	assert.Equal(t, []string{"tinyllm"}, slots["custom:http://10.0.0.9:8000"],
		"the slot carries the served models for advertisement and ingress")
	assert.Empty(t, b.store.Snapshot(),
		"an endpoint is not a machine: it must never project as a discovery node")
	assert.NotContains(t, snapshotJSON(b), bridgeTestKey,
		"defense in depth: nothing on the discovery wire may carry the key")
}

// The credential is read from config at bridge time, so the edit flow (same
// key, swapped secret) re-issues the candidate with the NEW key and the old
// one cannot linger in the payload.
func TestCustomUpdated_ReissuedCandidateCarriesCurrentConfigKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "custom-endpoints.json")
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000", APIKey: "sk-old-key"},
	}))
	seen := make(chan bridgeFrame, 8)
	proxy := newCustomProxyFaker(t, seen)
	b := &Broker{
		codec:               NewCodec(rwDiscard{}),
		store:               newDiscoveryStore(),
		customEndpointsPath: path,
	}
	b.setEngineProxyHandle(customProxyProfile, proxy)

	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "m1"))
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000", APIKey: "sk-new-key"},
	}))
	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "m1", "m2"))

	frames := takeBridgeFrames(t, seen, 3*time.Second)
	require.Len(t, frames, 2, "each bridged state issues its own add")
	assert.Contains(t, frames[0].params, "sk-old-key")
	assert.Contains(t, frames[1].params, "sk-new-key")
	assert.NotContains(t, frames[1].params, "sk-old-key", "the superseded key must not ride the new frame")
}

// A healthy endpoint whose model list is empty is routable by nobody: it
// stays out of the store and out of the proxy candidate set.
func TestCustomUpdated_HealthyWithoutModelsIsNotRoutable(t *testing.T) {
	b, seen := customBridgeBroker(t, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000"},
	})

	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true))

	frames := takeBridgeFrames(t, seen, 1*time.Second)
	for _, frame := range frames {
		assert.NotEqual(t, "custom:node/add-manual", frame.method,
			"a model-less endpoint must never be a routing candidate")
	}
	assert.Empty(t, b.store.customSlots(),
		"a model-less endpoint must not claim a store routing slot")
}

// Health flips both ways drive the candidate: down must pull the stale
// candidate back out of the proxy (the manual-node auto-suppression style)
// and release its routing slot, and back-up must re-issue both — AddManual's
// probeMisses reset is what restores the 401×3 budget after a flap.
func TestCustomUpdated_FlappingEndpointCyclesCandidateAndBucket(t *testing.T) {
	b, seen := customBridgeBroker(t, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000", APIKey: bridgeTestKey},
	})

	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))
	require.NotEmpty(t, takeBridgeFrames(t, seen, 2*time.Second), "first up must bridge")
	require.Contains(t, b.store.customSlots(), "custom:http://10.0.0.9:8000",
		"the first up report claims the routing slot")

	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", false, "tinyllm"))
	downFrames := takeBridgeFrames(t, seen, 2*time.Second)
	require.Len(t, downFrames, 1, "expected one frame for the down transition")
	assert.Equal(t, "custom:node/remove-manual", downFrames[0].method,
		"a down endpoint must be pulled from routing, not left to fail per-request")
	assert.NotContains(t, b.store.customSlots(), "custom:http://10.0.0.9:8000",
		"a down endpoint must release its routing claim")

	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))
	upFrames := takeBridgeFrames(t, seen, 2*time.Second)
	require.Len(t, upFrames, 1, "expected one frame for the recovery transition")
	assert.Equal(t, "custom:node/add-manual", upFrames[0].method, "back-up re-issues the candidate")
	assert.Contains(t, b.store.customSlots(), "custom:http://10.0.0.9:8000",
		"recovery re-claims the routing slot")
	assert.Empty(t, b.store.Snapshot(),
		"no custom state — claimed or released — ever projects as a discovery node")
}

// A custom/removed for a configured endpoint drops its claims (the prober
// emits it on its own remove path; the broker's config-side remove may have
// already dropped them — this must stay idempotent, not error).
func TestCustomRemoved_DropsCandidateAndBucket(t *testing.T) {
	b, seen := customBridgeBroker(t, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000", APIKey: bridgeTestKey},
	})
	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))
	require.NotEmpty(t, takeBridgeFrames(t, seen, 2*time.Second), "setup add frame")

	b.manualNotificationHandler(b.manualGeneration)("custom/removed",
		json.RawMessage(`{"id":"custom:http://10.0.0.9:8000","baseUrl":"http://10.0.0.9:8000","healthy":false}`))

	frames := takeBridgeFrames(t, seen, 2*time.Second)
	require.Len(t, frames, 1)
	assert.Equal(t, "custom:node/remove-manual", frames[0].method)
	assert.NotContains(t, b.store.customSlots(), "custom:http://10.0.0.9:8000",
		"custom/removed releases the routing claim")

	// Idempotent second pass: nothing to remove is a no-op, not a second frame.
	b.manualNotificationHandler(b.manualGeneration)("custom/removed",
		json.RawMessage(`{"id":"custom:http://10.0.0.9:8000"}`))
	assert.Empty(t, takeBridgeFrames(t, seen, 1*time.Second))
}

// An update naming an endpoint the broker's config no longer has must not
// create a candidate (resurrection risk: an in-flight event racing a user
// remove). It resolves to the same drop as an unhealthy report.
func TestCustomUpdated_UnconfiguredEndpointCannotCreateCandidate(t *testing.T) {
	b, seen := customBridgeBroker(t, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000", APIKey: bridgeTestKey},
	})
	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))
	require.NotEmpty(t, takeBridgeFrames(t, seen, 2*time.Second), "setup add frame")

	require.NoError(t, writePersistedCustomEndpoints(b.customEndpointsPath, nil))
	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))

	frames := takeBridgeFrames(t, seen, 2*time.Second)
	require.Len(t, frames, 1, "a config-gone event must not re-issue the candidate")
	assert.Equal(t, "custom:node/remove-manual", frames[0].method)
	assert.NotContains(t, b.store.customSlots(), "custom:http://10.0.0.9:8000",
		"a config-gone event must not keep the routing claim")
}

// The config-side remove must drop the proxy candidate immediately rather
// than waiting for a prober event that may never arrive (prober down). This
// drives the real custom/remove handler, not the event path.
func TestCustomRemove_DropsCandidateWithoutWaitingForProberEvent(t *testing.T) {
	proberSeen := make(chan proberCall, 8)
	worker := newCustomProberFaker(t, proberSeen)
	clientBroker, clientSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientBroker.Close()
		_ = clientSide.Close()
	})
	path := filepath.Join(t.TempDir(), "configs", "custom-endpoints.json")
	require.NoError(t, writePersistedCustomEndpoints(path, []persistedCustomEndpoint{
		{BaseURL: "http://10.0.0.9:8000", APIKey: bridgeTestKey},
	}))
	proxySeen := make(chan bridgeFrame, 8)
	proxy := newCustomProxyFaker(t, proxySeen)
	b := &Broker{
		codec:               NewCodec(clientBroker),
		store:               newDiscoveryStore(),
		customEndpointsPath: path,
	}
	b.setManualNodes(worker)
	b.setEngineProxyHandle(customProxyProfile, proxy)

	// Establish the candidate, then remove through the client-facing method.
	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))
	require.NotEmpty(t, takeBridgeFrames(t, proxySeen, 2*time.Second), "setup add frame")

	resp := customCall(t, b, NewCodec(clientSide), "custom/remove", `{"id":"custom:http://10.0.0.9:8000"}`)
	require.Nil(t, resp.Error, "remove response")
	assert.Contains(t, string(resp.Result), `"removed":true`)

	// The fake prober answers and stays silent (no custom/removed follow-up),
	// so any remove-manual frame here can only come from the direct drop.
	removeFrames := takeBridgeFrames(t, proxySeen, 2*time.Second)
	require.Len(t, removeFrames, 1, "config remove must drop the candidate on its own")
	assert.Equal(t, "custom:node/remove-manual", removeFrames[0].method)
	assert.NotContains(t, b.store.customSlots(), "custom:http://10.0.0.9:8000",
		"config remove releases the routing claim")

	// If the prober later emits its own custom/removed, the bridge is a
	// no-op rather than a second frame.
	b.manualNotificationHandler(b.manualGeneration)("custom/removed",
		json.RawMessage(`{"id":"custom:http://10.0.0.9:8000"}`))
	assert.Empty(t, takeBridgeFrames(t, proxySeen, 1*time.Second), "late prober event must not re-drop")
}

// The discovery wire lists machines. A bridged endpoint keeps its store slot
// for the internal readers while healthy, but never joins the snapshot — and
// a real machine sharing the store must still project untouched. This is the
// regression pin for the loopback endpoint that surfaced in Add Node as
// "CUSTOM:HTTP://…": the endpoint is filtered at the wire boundary instead
// of every client (desktop, TUI, scheduler) learning to spot the custom:
// spelling of what is not a host.
func TestCustomBridgedEndpoint_KeepsRoutingSlotButNeverProjectsAsNode(t *testing.T) {
	b, seen := customBridgeBroker(t, []persistedCustomEndpoint{
		{Name: "lab", BaseURL: "http://10.0.0.9:8000", APIKey: bridgeTestKey},
	})
	b.store.Upsert(
		EnrichedNode{ID: "workstation", HostUUID: "uuid-workstation", Host: "workstation", Port: 14318},
		sourceScanner)

	b.manualNotificationHandler(b.manualGeneration)("custom/updated",
		updatedParams("custom:http://10.0.0.9:8000", "http://10.0.0.9:8000", true, "tinyllm"))
	require.NotEmpty(t, takeBridgeFrames(t, seen, 2*time.Second), "setup add frame")

	snap := b.store.Snapshot()
	require.Len(t, snap, 1, "the endpoint must not join the discovery snapshot: %v", snap)
	assert.Equal(t, "workstation", snap[0].ID, "the machine still projects")
	require.Contains(t, b.store.customSlots(), "custom:http://10.0.0.9:8000",
		"the routing slot stays while the internal readers need it")
}
