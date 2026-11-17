// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Request-path contract for the Custom engine kind: user-configured external
// OpenAI-compatible endpoints.
//
// Four properties distinguish Custom from the ollama-class engines, and each
// is exercised both positively and against the neighboring behavior it must
// not disturb:
//
//   - Credential policy is per-class, not global. A Custom endpoint's API key
//     is scoped to the endpoint: the caller's own Authorization header is
//     dropped, and either the configured key or nothing is sent. Manual nodes
//     behind the managed engines keep today's credential pass-through, which
//     is why one of these tests pins that as the contrast.
//   - A persistent 401/403 is a probe failure, not just a bad answer: three
//     consecutive authorization refusals remove the endpoint from the routing
//     set and surface through the errors pipeline, mirroring the
//     nvpair-manual-nodes probe semantics.
//   - The federated model list reaches each candidate the same way inference
//     does, so a key-protected service's inventory is actually listable.
//   - An endpoint is routing, not a machine. The outward node wire — the
//     nodes/list reply and the node/* live events — lists machines: its
//     consumers offer invites, poll health, and pair against what it lists.
//     On the Custom facade the manual overlay holds endpoint projections, so
//     those entries stay internal bookkeeping and never reach that wire,
//     while machine peers on the subscribed overlay keep flowing. Managed
//     engines keep their manual nodes on the wire, pinned as the ollama
//     contrast.
//
// These tests drive the facade directly (handleHTTP), the same entry the
// shared failover suite uses, or the shared JSON-RPC dispatcher
// (handleMessage), and capture the JSON-RPC side through recRW.
// The package has no t.Parallel tests and the log-capture test is the only
// one that swaps slog.Default; sequential execution makes that safe.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"nvpair-shared/engines"
	"nvpair-shared/noderec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testEndpointKey = "sk-endpoint-secret"
	testClientCred  = "Bearer client-own-credential"
)

// customProfileOrFail builds a facade harness on the Custom profile. It fails
// rather than skips when the profile is absent: a Custom install without the
// profile has no way to route anything, so "missing" is exactly the state a
// test here must catch.
func customProfileOrFail(t *testing.T) engineProfile {
	t.Helper()
	p, ok := profileFor("custom")
	require.True(t, ok, "custom profile missing from the proxy")
	return p
}

// endpointNode builds the manual-node projection of a configured Custom
// endpoint: a routable Node pointing at an httptest upstream, optionally
// carrying the endpoint's API key. A key is a per-candidate secret and must
// never ride an outward notification; the assertions that pin that live with
// the notification call sites, not the routing table.
func endpointNode(t *testing.T, id, serverURL, model, key string) Node {
	t.Helper()
	var n Node
	if model == "" {
		n = nodeFor(t, id, serverURL)
	} else {
		n = nodeForModel(t, id, serverURL, model)
	}
	n.APIKey = key
	return n
}

// customChatRequest is an OpenAI-dialect inference call for the given model.
func customChatRequest(model string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q}`, model)))
}

// lockedBuffer is a bytes.Buffer safe for slog handlers, which may write
// from request goroutines while the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureDefaultLog redirects slog.Default for the duration of the test.
func captureDefaultLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestCustomFacade_SendsEndpointKeyInsteadOfClientCredentials: the caller's
// Authorization is the client's, not a credential for the endpoint — it is
// dropped and replaced with the configured key. Neither secret may appear in
// any log line or JSON-RPC frame produced while serving the request.
func TestCustomFacade_SendsEndpointKeyInsteadOfClientCredentials(t *testing.T) {
	logs := captureDefaultLog(t)

	authAtUpstream := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authAtUpstream <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"done":true}`)
		assert.NoError(t, err)
	}))
	defer upstream.Close()

	disc := NewDiscovery()
	disc.AddManual(endpointNode(t, "custom-1", upstream.URL, "my-model", testEndpointKey))
	events := &recRW{}
	p := newTestProxy(customProfileOrFail(t), NewCodec(events), disc, 8000)

	rec := httptest.NewRecorder()
	req := customChatRequest("my-model")
	req.Header.Set("Authorization", testClientCred)
	p.soleFacade().handleHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "status")
	var got string
	select {
	case got = <-authAtUpstream:
	default:
		require.FailNow(t, "endpoint never received the request")
	}
	assert.Equal(t, "Bearer "+testEndpointKey, got, "Authorization sent to the endpoint")

	telemetry := logs.String() + "\n" + events.String()
	assert.NotContains(t, telemetry, testEndpointKey, "endpoint key leaked into logs or JSON-RPC frames")
	assert.NotContains(t, telemetry, "client-own-credential", "client credential leaked into logs or JSON-RPC frames")
}

// TestCustomFacade_ForwardsNoCredentialsWithoutEndpointKey pins the
// credential policy in both directions it needs pinning. On a Custom facade,
// neither the client's credentials nor any other secret is forwarded to an
// endpoint without a configured key. On the managed engines, an explicit
// manual node keeps the pass-through it has always had: the stripping is
// scoped to the unmanaged (external-endpoint) class, and widening it to
// ollama-class manual nodes would break users who authenticate against their
// own Ollama servers.
func TestCustomFacade_ForwardsNoCredentialsWithoutEndpointKey(t *testing.T) {
	t.Run("custom facade strips client credentials", func(t *testing.T) {
		authAtUpstream := make(chan string, 1)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authAtUpstream <- r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"done":true}`)
			assert.NoError(t, err)
		}))
		defer upstream.Close()

		disc := NewDiscovery()
		disc.AddManual(endpointNode(t, "open-endpoint", upstream.URL, "my-model", ""))
		p := testProxy(customProfileOrFail(t), disc, 8000)

		rec := httptest.NewRecorder()
		req := customChatRequest("my-model")
		req.Header.Set("Authorization", testClientCred)
		p.soleFacade().handleHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, "status")
		var got string
		select {
		case got = <-authAtUpstream:
		default:
			require.FailNow(t, "endpoint never received the request")
		}
		assert.Equal(t, "", got, "client credentials leaked to an external endpoint")
	})

	t.Run("ollama-class manual node keeps credential pass-through", func(t *testing.T) {
		tc := ollamaCase(t)
		authAtUpstream := make(chan string, 1)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authAtUpstream <- r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"done":true}`)
			assert.NoError(t, err)
		}))
		defer upstream.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "ollama-manual", upstream.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		req := tc.inferenceRequest()
		req.Header.Set("Authorization", testClientCred)
		p.soleFacade().handleHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, "status")
		var got string
		select {
		case got = <-authAtUpstream:
		default:
			require.FailNow(t, "manual node never received the request")
		}
		assert.Equal(t, testClientCred, got, "manual node lost the credential pass-through managed engines rely on")
	})
}

// TestCustomFacade_ThreeUnauthorizeResponsesRemoveEndpoint: a persistent 401
// means the endpoint's key is wrong, not that the endpoint is busy. Retrying
// elsewhere would return the same answer, so each 401 passes through — but
// after three consecutive authorization refusals the endpoint is removed from
// the routing set (the manual-nodes probe-failure rule) and the condition is
// surfaced through the errors pipeline rather than dropped. Re-adding the
// node re-enables routing; in production that re-add comes from a successful
// re-probe by nvpair-manual-nodes (Task 4).
func TestCustomFacade_ThreeUnauthorizeResponsesRemoveEndpoint(t *testing.T) {
	var hits atomic.Int32
	unauthorized := atomic.Bool{}
	unauthorized.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if unauthorized.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, err := io.WriteString(w, `{"error":"unauthorized"}`)
			assert.NoError(t, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"done":true}`)
		assert.NoError(t, err)
	}))
	defer upstream.Close()

	disc := NewDiscovery()
	node := endpointNode(t, "locked", upstream.URL, "my-model", "")
	disc.AddManual(node)
	events := &recRW{}
	p := newTestProxy(customProfileOrFail(t), NewCodec(events), disc, 8000)
	f := p.soleFacade()
	f.SetSelected("locked")

	serveChat := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.handleHTTP(rec, customChatRequest("my-model"))
		return rec
	}

	for i := 1; i <= 3; i++ {
		rec := serveChat()
		require.Equal(t, http.StatusUnauthorized, rec.Code, "request %d must pass the 401 through", i)
		require.Equal(t, int32(i), hits.Load(), "endpoint dials through request %d", i)
	}
	require.False(t, disc.IsManual("locked"), "endpoint still routable after three consecutive 401s")

	// The removal surfaces through the errors pipeline rather than silently
	// shrinking the candidate set: facade-addressed, per the emit precedent in
	// facade.go for upstream-unreachable.
	require.Contains(t, events.String(), `"custom:errors:report"`, "no errors:report frame after probe failure")
	assert.Contains(t, events.String(), `"custom-proxy:probe-failed:locked"`, "probe-failed error id")

	// With the endpoint gone the model has no owner, and the request fails
	// immediately rather than re-dialing the removed endpoint.
	rec := serveChat()
	require.Equal(t, http.StatusBadGateway, rec.Code, "status after removal")
	assert.Contains(t, rec.Body.String(), "no available node advertises the requested model")
	require.Equal(t, int32(3), hits.Load(), "the removed endpoint was dialed again")

	// A re-add — the local stand-in for a successful re-probe — restores
	// routing on the same endpoint.
	unauthorized.Store(false)
	require.True(t, disc.AddManual(node), "re-adding a removed endpoint must re-route")
	rec = serveChat()
	require.Equal(t, http.StatusOK, rec.Code, "status after re-add")
	require.Equal(t, int32(4), hits.Load(), "endpoint dials after re-add")
}

// TestCustomFacade_ModelListFanoutInjectsEndpointKeys: the federated
// /v1/models reaches each candidate the same way inference does. Without key
// injection the key-protected service answers 401, its models silently
// vanish from the merged list, and the client cannot tell "no models" from
// "wrong key" — which is the failure the mandatory-key design exists to fix.
func TestCustomFacade_ModelListFanoutInjectsEndpointKeys(t *testing.T) {
	var mu sync.Mutex
	seenAuth := make(map[string]string, 2)
	recordAuth := func(node, auth string) {
		mu.Lock()
		defer mu.Unlock()
		seenAuth[node] = auth
	}

	keyed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		recordAuth("keyed", auth)
		if auth != "Bearer "+testEndpointKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, err := io.WriteString(w, `{"error":"unauthorized"}`)
			assert.NoError(t, err)
			return
		}
		_, err := io.WriteString(w, `{"object":"list","data":[{"id":"private-model","owned_by":"endpoint"}]}`)
		assert.NoError(t, err)
	}))
	defer keyed.Close()

	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		recordAuth("open", auth)
		_, err := io.WriteString(w, `{"object":"list","data":[{"id":"public-model","owned_by":"open"}]}`)
		assert.NoError(t, err)
	}))
	defer open.Close()

	disc := NewDiscovery()
	disc.AddManual(endpointNode(t, "keyed", keyed.URL, "", testEndpointKey))
	disc.AddManual(endpointNode(t, "open", open.URL, "", ""))
	p := testProxy(customProfileOrFail(t), disc, 8000)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", testClientCred)
	p.soleFacade().handleHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "status")
	body := rec.Body.String()
	assert.Contains(t, body, "private-model", "key-protected models missing: endpoint key was not injected")
	assert.Contains(t, body, "public-model", "open endpoint's models missing")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Bearer "+testEndpointKey, seenAuth["keyed"], "Authorization sent to the key-protected endpoint")
	assert.Equal(t, "", seenAuth["open"], "credentials leaked to the open endpoint")
}

// TestCustomProfile_ClassifiesOpenAIRoutesWithExactIDs: the route table is
// the classification, so it is asserted here rather than inferred from
// request-path tests alone — an unlisted path is forwarded verbatim with no
// model routing, and a misclassified list path would fall through the
// inference dispatch loop instead of the fan-out. Identity values come from
// the shared engines table; the profile must consume them unchanged.
func TestCustomProfile_ClassifiesOpenAIRoutesWithExactIDs(t *testing.T) {
	p := customProfileOrFail(t)

	require.Equal(t, 6767, p.StandalonePort, "StandalonePort")
	require.Equal(t, exactID, p.ModelNaming, "ModelNaming")
	require.True(t, p.Unmanaged, "custom must stay declared Unmanaged so nothing tries to launch it")
	require.Equal(t, noderec.ServiceCustom, p.DiscoveryService, "DiscoveryService")

	listRole, routed := p.roleFor(http.MethodGet, "/v1/models")
	require.True(t, routed, "/v1/models classification")
	require.Equal(t, roleModelListOpenAIGET, listRole, "/v1/models role")
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/responses"} {
		role, routed := p.roleFor(http.MethodPost, path)
		require.True(t, routed, "%s classification", path)
		require.Equal(t, roleInferencePOST, role, "%s role", path)
	}
}

// nodeParams marshals a value into an RPC params payload so handler tests
// take the same JSON decode a broker call takes rather than injecting a
// pre-built struct.
func nodeParams(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// TestCustomNodesList_ListsMachinesNotEndpointCandidates: an endpoint is not
// a machine. The Custom facade's manual-overlay entries are endpoint
// projections — routing bookkeeping — and the nodes/list reply is read as a
// machine list: the desktop replays it into the node map that feeds Add
// Node, and the engine-status sweeps pair against every id it lists. So the
// endpoint entries stay out of the reply while machine peers on the
// subscribed overlay keep flowing. With endpoints gone from this wire the
// credential question answers itself here; the key-strip rule that used to
// be pinned on the Custom reply moves to ollama, where a manual node's key
// is still observable.
func TestCustomNodesList_ListsMachinesNotEndpointCandidates(t *testing.T) {
	disc := NewDiscovery()
	disc.AddManual(endpointNode(t, "custom-keyed", "http://127.0.0.1:1", "", testEndpointKey))
	disc.AddManual(endpointNode(t, "custom-open", "http://127.0.0.1:2", "public-model", ""))
	disc.SetSubscribed([]Node{{
		ID: "peer-machine", Host: "peer.local", Port: 8000,
		Addresses: []string{"10.0.0.9"}, IP: "10.0.0.9",
	}})
	events := &recRW{}
	p := newTestProxy(customProfileOrFail(t), NewCodec(events), disc, 8000)

	id := json.RawMessage(`1`)
	p.handleMessage(&Message{
		Method: engines.AddressMethod("custom", "nodes/list"),
		ID:     &id,
	})

	reply := events.String()
	require.True(t, strings.Contains(reply, "peer-machine"),
		"machine peers must stay on the custom nodes/list wire")
	assert.NotContains(t, reply, "custom-keyed", "endpoint projection leaked into the nodes/list reply")
	assert.NotContains(t, reply, "custom-open", "endpoint projection leaked into the nodes/list reply")
	assert.NotContains(t, reply, testEndpointKey, "endpoint key echoed in the nodes/list reply")
}

// TestCustomAddManual_KeepsEndpointRoutable: the projection filter is about
// the outward node wire only. The dial path resolves candidates from the
// stored set, so an endpoint added through node/add-manual — the exact call
// the custom bridge makes — must still serve inference from the same handler
// that goes quiet on node/*. This pins that a future change cannot "fix"
// the wire by not storing the candidate, which would trade a UI ghost for
// a dead endpoint.
func TestCustomAddManual_KeepsEndpointRoutable(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"done":true}`)
		assert.NoError(t, err)
	}))
	defer upstream.Close()

	events := &recRW{}
	disc := NewDiscovery()
	p := newTestProxy(customProfileOrFail(t), NewCodec(events), disc, 8000)

	id := json.RawMessage(`1`)
	p.handleMessage(&Message{
		Method: engines.AddressMethod("custom", "node/add-manual"),
		ID:     &id,
		Params: nodeParams(t, endpointNode(t, "endpoint-1", upstream.URL, "served-model", testEndpointKey)),
	})
	require.True(t, disc.IsManual("endpoint-1"), "the endpoint must stay in the routing set")

	rec := httptest.NewRecorder()
	p.soleFacade().handleHTTP(rec, customChatRequest("served-model"))
	require.Equal(t, http.StatusOK, rec.Code, "status")
	require.Equal(t, int32(1), hits.Load(), "endpoint never dialed")

	assert.NotContains(t, events.String(), "node/discovered", "the event wire stayed quiet for the endpoint")
}

// TestCustomAddRemoveManual_KeepsEndpointsOffTheNodeEventWire: the same
// class rule on the live-event path. A Custom endpoint that flaps in and
// out of the routing set must not chirp node/discovered|updated|removed,
// for the same reason it stays out of the reply: every consumer of that
// stream treats its entries as machines. The replies keep their shape; only
// the broadcast is scoped out.
func TestCustomAddRemoveManual_KeepsEndpointsOffTheNodeEventWire(t *testing.T) {
	events := &recRW{}
	disc := NewDiscovery()
	p := newTestProxy(customProfileOrFail(t), NewCodec(events), disc, 8000)

	add := func(id json.RawMessage, node Node) {
		p.handleMessage(&Message{
			Method: engines.AddressMethod("custom", "node/add-manual"),
			ID:     &id,
			Params: nodeParams(t, node),
		})
	}
	endpoint := endpointNode(t, "custom-keyed", "http://127.0.0.1:1", "served-model", testEndpointKey)

	add(json.RawMessage(`1`), endpoint)
	require.True(t, disc.IsManual("custom-keyed"), "add-manual must keep the endpoint routable")
	require.True(t, strings.Contains(events.String(), `"added":true`), "add reply lost its result")

	// A same-id re-add is the bridge's post-flap update path; it stays
	// silent on the event wire too.
	add(json.RawMessage(`2`), endpoint)
	require.True(t, strings.Contains(events.String(), `"added":false`), "update reply lost its result")

	remove := json.RawMessage(`3`)
	p.handleMessage(&Message{
		Method: engines.AddressMethod("custom", "node/remove-manual"),
		ID:     &remove,
		Params: json.RawMessage(`{"id":"custom-keyed"}`),
	})
	require.True(t, strings.Contains(events.String(), `"removed":true`), "remove reply lost its result")

	wire := events.String()
	assert.NotContains(t, wire, "node/discovered", "endpoint add announced a machine discovery")
	assert.NotContains(t, wire, "node/updated", "endpoint update announced a machine update")
	assert.NotContains(t, wire, "node/removed", "endpoint removal announced a machine removal")
	assert.NotContains(t, wire, testEndpointKey, "endpoint key reached the JSON-RPC stream")
}

// TestOllamaNodesList_KeepsManualNodesAndStripsKeys: the projection filter
// is class-scoped to Unmanaged facades. An ollama-class manual node is a
// machine the consumer can invite and poll, so it stays on the managed
// engines' reply — hiding it there would hide real machines. The credential
// strip, the rule this file's reply test inherited, stays observable here.
func TestOllamaNodesList_KeepsManualNodesAndStripsKeys(t *testing.T) {
	tc := ollamaCase(t)
	disc := NewDiscovery()
	disc.AddManual(endpointNode(t, "ollama-manual", "http://127.0.0.1:11434", tc.advertisedModel, testEndpointKey))
	events := &recRW{}
	p := newTestProxy(tc.profile, NewCodec(events), disc, tc.profile.FacadePort)

	id := json.RawMessage(`1`)
	p.handleMessage(&Message{
		Method: engines.AddressMethod(tc.profile.Name, "nodes/list"),
		ID:     &id,
	})

	reply := events.String()
	require.True(t, strings.Contains(reply, "ollama-manual"), "managed engines keep manual nodes on nodes/list")
	assert.NotContains(t, reply, testEndpointKey, "endpoint key echoed in the nodes/list reply")
	assert.NotContains(t, reply, "apiKey", "the reply still serializes the credential field")
}

// TestOllamaAddManual_StillAnnouncesManualNodes: the contrast that gives
// the Custom event-wire silence its shape — on a managed engine, adding a
// manual node still announces node/discovered (and announces it with the
// credential stripped), because there a manual entry is a machine.
func TestOllamaAddManual_StillAnnouncesManualNodes(t *testing.T) {
	tc := ollamaCase(t)
	disc := NewDiscovery()
	events := &recRW{}
	p := newTestProxy(tc.profile, NewCodec(events), disc, tc.profile.FacadePort)

	id := json.RawMessage(`1`)
	p.handleMessage(&Message{
		Method: engines.AddressMethod(tc.profile.Name, "node/add-manual"),
		ID:     &id,
		Params: nodeParams(t, endpointNode(t, "ollama-manual", "http://127.0.0.1:11434", tc.advertisedModel, testEndpointKey)),
	})

	wire := events.String()
	require.True(t, strings.Contains(wire, "node/discovered"), "managed engines keep announcing manual nodes")
	assert.NotContains(t, wire, testEndpointKey, "the notification leaked the endpoint key")
}
