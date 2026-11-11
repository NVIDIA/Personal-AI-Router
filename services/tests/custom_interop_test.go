// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process e2e for the Custom engine chain: a user-configured external
// OpenAI-compatible endpoint becomes routable through the broker plane.
// custom/add persists on the broker, nvpair-manual-nodes probes the
// endpoint's /v1/models, the broker bridges a healthy result into the custom
// facade's routing set (carrying the endpoint key on that one local hop), and
// the nvpair-proxy custom facade dials the endpoint with the credential
// injected.
//
// Two behaviours, one per test:
//
//   - Configuration becomes routing: custom is fronted with no
//     --proxy-engines override on its declared port 6767, configured
//     endpoints join the facade's routing set, and both a keyless and a
//     key-gated endpoint route their exact model ids — while the discovery
//     wire, which lists machines, stays free of endpoint entries.
//   - Endpoint state drives routing: an outage pulls the endpoint after
//     three probe misses and a successful probe brings it back; a
//     custom/remove drops it immediately — inside the time three probe
//     cadences need — with nothing dialing the still-live endpoint once
//     routing is gone.
//
// Routability is read from inference rather than from a node list. An
// endpoint is not a machine, so it is projected on no wire: neither the
// discovery snapshot nor the facade's node reply carries it. A chat says the
// same thing anyway, and names the difference — "no available node advertises
// the requested model" is answered without dialing anyone, while a dial that
// hangs, fails or answers badly names the endpoint and means it is still
// routed. Waiting on the reply therefore needs no wire that lists endpoints,
// and disturbs no probe accounting: only a committed 401/403 counts against
// an endpoint, and none of these polls produces one.
//
// The flap test intentionally runs the production probe cadence (10 s) three
// times down and once up, so it is long by construction. Fake upstreams sit
// on high ephemeral ports: the endpoint surface refuses endpoints aimed at
// facade ports, and a facade whose default 6767 is already taken yields
// rather than evicting whatever holds it, so those hosts skip.
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/jsonrpc"
)

// --- fake external endpoint -------------------------------------------------

// fakeEndpoint is a minimal OpenAI-compatible "external" service: GET
// /v1/models answers exactly one model and POST /v1/chat/completions answers
// an empty completion. When requiredAuth is set, both routes answer 401
// unless the Authorization header matches it exactly, and every Authorization
// header seen is recorded so a test can prove which credential arrived (the
// probe path and the injected inference path share this check).
type fakeEndpoint struct {
	t            *testing.T
	port         int
	model        string
	requiredAuth string

	mu       sync.Mutex
	server   *http.Server
	authSeen []string
	chatHits atomic.Int32
}

func newFakeEndpoint(t *testing.T, port int, model, requiredAuth string) *fakeEndpoint {
	t.Helper()
	ep := &fakeEndpoint{t: t, port: port, model: model, requiredAuth: requiredAuth}
	ep.restart()
	t.Cleanup(ep.kill)
	return ep
}

// restart (re)binds the endpoint to its port. kill() unbinds it so probes
// and dials meet connection-refused — the outage state the probe cycle is
// supposed to notice. Rebinding the same port lets one endpoint identity
// live through an outage and its recovery.
func (ep *fakeEndpoint) restart() {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ep.port))
	require.NoError(ep.t, err, "listen on fake endpoint port %d", ep.port)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", ep.handleModels)
	mux.HandleFunc("/v1/chat/completions", ep.handleChat)
	server := &http.Server{Handler: mux}
	ep.mu.Lock()
	ep.server = server
	ep.mu.Unlock()
	go func() { _ = server.Serve(ln) }()
}

func (ep *fakeEndpoint) kill() {
	ep.mu.Lock()
	server := ep.server
	ep.server = nil
	ep.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
}

func (ep *fakeEndpoint) noteAuth(auth string) {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.authSeen = append(ep.authSeen, auth)
}

func (ep *fakeEndpoint) authorizations() []string {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return append([]string(nil), ep.authSeen...)
}

// rejected answers 401 when the endpoint is key-gated and the caller did not
// present the endpoint's own key.
func (ep *fakeEndpoint) rejected(w http.ResponseWriter, r *http.Request) bool {
	if ep.requiredAuth == "" {
		return false
	}
	auth := r.Header.Get("Authorization")
	ep.noteAuth(auth)
	if auth == ep.requiredAuth {
		return false
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	return true
}

// Write errors are deliberately unchecked in the handlers below: a peer may
// hang up during teardown, and reporting from a server goroutine would panic
// on a finished test rather than attribute a failure.

func (ep *fakeEndpoint) handleModels(w http.ResponseWriter, r *http.Request) {
	if ep.rejected(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+ep.model+`","object":"model"}]}`)
}

func (ep *fakeEndpoint) handleChat(w http.ResponseWriter, r *http.Request) {
	if ep.rejected(w, r) {
		return
	}
	ep.chatHits.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"choices":[]}`)
}

// --- broker-stream helpers --------------------------------------------------

// customRPCSeq hands this file's JSON-RPC calls distinct ids. Based at 70000
// to stay clear of other files' fixed ids and of the waitEngineProxyReady
// poll sequence (9000 + shared counter).
var customRPCSeq atomic.Int64

func nextCustomRPCID() int {
	return 70000 + int(customRPCSeq.Add(1))
}

// callCustomRPC writes one request frame and waits for the reply carrying its
// id (nil params sends the request with no params field, matching how the
// other suites call discovery:get-nodes).
func callCustomRPC(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, id int, method string, params any) jsonrpc.Message {
	t.Helper()
	var frame string
	if params == nil {
		frame = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, id, method)
	} else {
		data, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		})
		require.NoError(t, err, "marshal %s", method)
		frame = string(data)
	}
	writeRawFrame(t, stdin, frame)
	response := waitForResponseID(t, msgs, id, 15*time.Second)
	require.Nil(t, response.Error, "RPC %s returned an error", method)
	return response
}

// customNoRouteMarker is how the facade answers a chat whose model has no
// candidate. It names the routing set rather than a dial, which is what
// separates it from the similar-looking answer for a routed-but-down endpoint.
const customNoRouteMarker = "no available node advertises the requested model"

// customChatAttempt makes one chat through the custom facade and reports its
// answer without failing the test. Status 0 means the request never produced
// a response: a facade still routing to a down endpoint dials it and can
// spend longer on that than any client patience, which is a "still routed"
// answer rather than a defect in the wait.
func customChatAttempt(client *http.Client, port int, model string) (int, string) {
	response, err := client.Post(
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port),
		"application/json",
		bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)),
	)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, err.Error()
	}
	return response.StatusCode, string(body)
}

// waitForCustomRoute polls one chat through the custom facade until the facade
// answers the way it answers when model is (wantRoutable true) or is not
// (wantRoutable false) in the facade's routing set.
//
// The two states are distinguished by what the facade did rather than by what
// it listed, because listing is not available: an endpoint is not a machine,
// so it never appears on a node wire. Success means someone was dialed and
// answered; the marker means no candidate existed to dial. Everything else —
// a dial that hangs, fails, or answers badly — means a candidate still
// existed, so the wait keeps going. No poll answers for a pulled endpoint,
// and only a committed 401/403 counts against an endpoint, so waiting this
// way cannot pull one down earlier than its own probe cadence would.
func waitForCustomRoute(t *testing.T, client *http.Client, port int, model string, wantRoutable bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status, body := customChatAttempt(client, port, model)
		if wantRoutable && status == http.StatusOK {
			return
		}
		if !wantRoutable && status == http.StatusBadGateway && strings.Contains(body, customNoRouteMarker) {
			return
		}
		if !time.Now().Before(deadline) {
			require.FailNowf(t, "custom route wait",
				"timed out after %s waiting for %q to be routable=%v; last answer was %d %s",
				timeout, model, wantRoutable, status, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// postCustomChat sends an OpenAI-dialect chat request for the exact model id
// through the custom facade port and returns the status and body.
func postCustomChat(t *testing.T, client *http.Client, port int, model string) (int, string) {
	t.Helper()
	response, err := client.Post(
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port),
		"application/json",
		bytes.NewBufferString(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)),
	)
	require.NoError(t, err, "post chat for model %q through the custom facade", model)
	defer func() { require.NoError(t, response.Body.Close(), "close chat response") }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err, "read chat response body")
	return response.StatusCode, string(body)
}

// drainStderr collects every captured stderr line (the broker's own log plus
// the forwarded worker and proxy logs) and returns a snapshot function.
func drainStderr(lines <-chan string) func() string {
	var mu sync.Mutex
	var collected strings.Builder
	go func() {
		for line := range lines {
			mu.Lock()
			collected.WriteString(line)
			collected.WriteByte('\n')
			mu.Unlock()
		}
	}()
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return collected.String()
	}
}

func customCandidateID(port int) string {
	return fmt.Sprintf("custom:http://127.0.0.1:%d", port)
}

// --- tests ------------------------------------------------------------------

// TestCustomChain_ConfiguredEndpoints_RouteThroughTheCustomFacade drives the
// whole chain as a user would: configure two external endpoints through the
// broker (one open, one that demands the configured key) and call
// /v1/chat/completions through the facade like a stock OpenAI client. The
// key-gated endpoint doubles as plumbing proof — it 401s anything without
// its exact key, so a probe or inference call reaching it proves the key
// travelled, and every Authorization it ever observed must be that key.
func TestCustomChain_ConfiguredEndpoints_RouteThroughTheCustomFacade(t *testing.T) {
	if portBusy(6767) {
		t.Skip("custom facade default port 6767 already in use; skipping")
	}
	const (
		openModel  = "custom-e2e/open-model"
		gatedModel = "custom-e2e/gated-model"
		gatedKey   = "sk-custom-e2e-secret"
	)
	openPort := freePort(t)
	gatedPort := freePort(t)
	open := newFakeEndpoint(t, openPort, openModel, "")
	gated := newFakeEndpoint(t, gatedPort, gatedModel, "Bearer "+gatedKey)

	stdin, msgs, stderrLines, cleanup := startBrokerWith(t,
		"--manual-nodes-path", manualNodesBin,
		"--proxy-path", proxyBin,
	)
	t.Cleanup(cleanup)
	stderrSnapshot := drainStderr(stderrLines)

	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	// No --proxy-engines override: custom must be fronted by default, and on
	// its declared port rather than a fallback — the port is explicit policy,
	// never inferred.
	facadePort := waitEngineProxyReady(t, "custom-proxy", stdin, msgs, 20*time.Second)
	require.Equal(t, 6767, facadePort, "custom facade must default to port 6767")

	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	type customAddResult struct {
		BaseURL string `json:"baseUrl"`
		KeySet  bool   `json:"keySet"`
	}

	// Configure both endpoints. keySet reports key presence without naming
	// the key, and the whole response must be key-free.
	add := callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "custom/add",
		map[string]any{"name": "open-e2e", "url": "http://127.0.0.1", "port": openPort})
	var openAdd customAddResult
	require.NoError(t, json.Unmarshal(add.Result, &openAdd), "decode custom/add result")
	assert.Equal(t, fmt.Sprintf("http://127.0.0.1:%d", openPort), openAdd.BaseURL, "composed baseUrl")
	assert.False(t, openAdd.KeySet, "keyless endpoint must report keySet false")

	add = callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "custom/add",
		map[string]any{"name": "gated-e2e", "url": "http://127.0.0.1", "port": gatedPort, "apiKey": gatedKey})
	var gatedAdd customAddResult
	require.NoError(t, json.Unmarshal(add.Result, &gatedAdd), "decode keyed custom/add result")
	assert.True(t, gatedAdd.KeySet, "keyed endpoint must report keySet true")
	assert.NotContains(t, string(add.Result), gatedKey, "custom/add response echoed the endpoint key")

	t.Run("keyless endpoint routes by exact model id", func(t *testing.T) {
		// Reaching 200 requires the whole chain: the prober's /v1/models
		// probe, the change-gated status event, the broker's add-manual
		// bridge, and the facade dialing the result.
		waitForCustomRoute(t, client, facadePort, openModel, true, 30*time.Second)

		hitsBefore := open.chatHits.Load()
		status, body := postCustomChat(t, client, facadePort, openModel)
		assert.Equal(t, http.StatusOK, status, "chat through the custom facade: %s", body)
		assert.Equal(t, int32(1), open.chatHits.Load()-hitsBefore,
			"only the routed inference must reach the endpoint")
	})

	t.Run("key-gated endpoint routes with the endpoint key injected", func(t *testing.T) {
		waitForCustomRoute(t, client, facadePort, gatedModel, true, 30*time.Second)

		hitsBefore := gated.chatHits.Load()
		status, body := postCustomChat(t, client, facadePort, gatedModel)
		assert.Equal(t, http.StatusOK, status, "a key-gated endpoint must route with its key: %s", body)

		observed := gated.authorizations()
		require.NotEmpty(t, observed, "the gated endpoint never saw a probe or inference request")
		for _, auth := range observed {
			assert.Equal(t, "Bearer "+gatedKey, auth, "Authorization observed at the gated endpoint")
		}
		assert.Equal(t, int32(1), gated.chatHits.Load()-hitsBefore,
			"only the routed inference must reach the endpoint")
	})

	// Routing is proven by now, so this is the moment an endpoint projection
	// could have reached either machine list: the broker's discovery snapshot,
	// which is what peers and the Add Node search read, and the custom facade's
	// own node reply, which the desktop replays into that same node map. An
	// endpoint is not a machine, so neither may carry it — projected, it
	// surfaced in Add Node as an invitable "node" whose Invite would have
	// attempted pairing against a facade port. Peer-facing model attribution is
	// a different wire, the owner's engine-manager /v1/models, and carries the
	// model rather than a node.
	snapshot := callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "discovery:get-nodes", nil)
	// This is a machine list, so it may legitimately carry machines — on a
	// live network the host's own reflected record appears here. The pin is
	// therefore the endpoint shape itself: the ghost entered this wire keyed
	// "custom:http://…", which no machine id can collide with.
	for _, endpointID := range []string{customCandidateID(openPort), customCandidateID(gatedPort)} {
		assert.NotContains(t, string(snapshot.Result), endpointID,
			"endpoint projection leaked into the discovery snapshot: %s", string(snapshot.Result))
	}
	assert.NotContains(t, string(snapshot.Result), gatedKey, "discovery snapshot carried the endpoint key")

	nodeReply := callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "custom-proxy:nodes/list", nil)
	for _, id := range []string{customCandidateID(openPort), customCandidateID(gatedPort)} {
		assert.False(t, proxyNodesHas(t, nodeReply.Result, id),
			"endpoint projection leaked into the custom facade's node reply: %s", string(nodeReply.Result))
	}
	assert.NotContains(t, string(nodeReply.Result), gatedKey, "the node reply carried the endpoint key")

	// Give any trailing probe-cycle log lines a beat to arrive, then prove
	// the key never appeared in a log line — the captured stream carries the
	// broker's, the proxy's, and every worker's stderr.
	time.Sleep(2 * time.Second)
	assert.NotContains(t, stderrSnapshot(), gatedKey, "endpoint key appeared in captured stderr")
}

// TestCustomChain_EndpointStateChanges_FollowTheRoutingSet pins that there is
// no on/off switch: health and configuration alone decide routability. A
// three-miss outage pulls the endpoint from the routing set, one successful
// probe restores it, and a custom/remove drops it on its own — inside three
// probe cadences, so the drop cannot be the prober merely noticing.
func TestCustomChain_EndpointStateChanges_FollowTheRoutingSet(t *testing.T) {
	if portBusy(6767) {
		t.Skip("custom facade default port 6767 already in use; skipping")
	}
	const model = "custom-e2e/flap-model"
	upPort := freePort(t)
	upstream := newFakeEndpoint(t, upPort, model, "")

	stdin, msgs, stderrLines, cleanup := startBrokerWith(t,
		"--manual-nodes-path", manualNodesBin,
		"--proxy-path", proxyBin, "--proxy-engines", "custom",
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderrLines {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	facadePort := waitEngineProxyReady(t, "custom-proxy", stdin, msgs, 20*time.Second)
	require.Equal(t, 6767, facadePort, "custom facade must default to port 6767")

	client := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	endpointID := customCandidateID(upPort)
	callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "custom/add",
		map[string]any{"url": "http://127.0.0.1", "port": upPort})
	waitForCustomRoute(t, client, facadePort, model, true, 45*time.Second)
	status, body := postCustomChat(t, client, facadePort, model)
	require.Equal(t, http.StatusOK, status, "baseline routing through the custom facade: %s", body)

	t.Run("outage pulls the endpoint after three probe misses", func(t *testing.T) {
		upstream.kill()
		// Three consecutive failures on the 10 s cadence plus event and
		// bridge latency; the window is deliberately shorter than four
		// cadences so a missing retraction cannot hide as slowness. While
		// the endpoint is still routed every poll is dialed — the dial hangs
		// or fails and a poll may spend the whole client timeout on it — and
		// the wait is for the different answer, which says nobody was dialed
		// at all.
		waitForCustomRoute(t, client, facadePort, model, false, 75*time.Second)

		hitsBefore := upstream.chatHits.Load()
		status, body := postCustomChat(t, client, facadePort, model)
		assert.Equal(t, http.StatusBadGateway, status,
			"a down endpoint must fail fast rather than be dialed per request: %s", body)
		assert.NotContains(t, body, "upstream error", "the answer refuses to route; it does not report a dial")
		assert.Equal(t, hitsBefore, upstream.chatHits.Load(), "a pulled endpoint is not dialed per request")
	})

	t.Run("recovery restores routing on one successful probe", func(t *testing.T) {
		upstream.restart()
		// One successful cadence probe (≤10 s) flips health back and re-issues
		// the candidate with a fresh probe-failure budget. Until that lands
		// there is nothing to dial, so these polls cannot hold the endpoint
		// down past its own probe cycle; the first poll that reaches it
		// answers 200.
		waitForCustomRoute(t, client, facadePort, model, true, 40*time.Second)

		status, body := postCustomChat(t, client, facadePort, model)
		assert.Equal(t, http.StatusOK, status, "routing must return with health: %s", body)
	})

	t.Run("custom remove drops routing without waiting for the probe cycle", func(t *testing.T) {
		removed := callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "custom/remove",
			map[string]any{"id": endpointID})
		var removedResult struct {
			Removed bool `json:"removed"`
		}
		require.NoError(t, json.Unmarshal(removed.Result, &removedResult), "decode custom/remove result")
		assert.True(t, removedResult.Removed, "custom/remove must report the configured endpoint removed")

		// 15 s is shorter than the three-cadence probe path: the withdrawal
		// must come from the configuration change itself, so a down or
		// event-silent prober cannot strand routing to a removed endpoint.
		waitForCustomRoute(t, client, facadePort, model, false, 15*time.Second)

		// Counted from the wait's end rather than from the remove call. While
		// the retract is still in flight the endpoint is a live candidate and
		// dialing it is correct; what must stop is dialing it once nothing
		// claims to serve the model, and that is the window measured here.
		chatHitsBefore := upstream.chatHits.Load()
		status, body := postCustomChat(t, client, facadePort, model)
		assert.Equal(t, http.StatusBadGateway, status, "a removed endpoint must not stay routable: %s", body)
		assert.NotContains(t, body, "upstream error", "the answer refuses to route; it does not report a dial")
		assert.Equal(t, chatHitsBefore, upstream.chatHits.Load(), "no dial once the endpoint left the routing set")
	})
}
