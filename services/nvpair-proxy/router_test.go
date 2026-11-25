// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The cluster endpoint's request path: what it answers itself, which providers
// it picks out of the cluster, what it sends each one, and what it publishes
// about the job while it runs.
//
// Manual nodes carry the cluster picture. A manual entry is dialed over plain
// HTTP and is scoped to the engine it was registered under, so a mock upstream
// on this Mac can stand in for "the Ollama host that has qwen3" or "the vLLM
// host that has deepseek" — which is what lets one machine hold a multi-engine
// cluster, and lets these tests say which engine serves which name without a
// real engine or a second host.
//
// Most of this file does not bind the router's own port: the listener is
// loopback-only and carries no branches, so the handler is the surface under
// test and requests arrive through httptest.NewRequest. The tests that need a
// live socket are the exception — the bound-state section at the end, where
// the reported port is the fact under test, and the one alias-lifecycle test,
// where the fact under test (an endpoint that routes this node's names from
// its first request) only exists once a client can knock on a real listener.
// Each of those binds asks the operating system for a free port, so no test
// depends on 9999 being unclaimed. Where an answer depends on the catalog clock
// the test passes its own time (see catalogNow), and where it depends on how
// busy a node looks the test books that load through the same ledger the
// facades use.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	"nvpair-shared/noderec"
	"nvpair-shared/schedulerwire"
)

// routerUpstream is a mock engine endpoint. It records every request it
// receives so "which hosts were dialed, how many times, and with what model"
// is a readable assertion — that question is the whole content of the failover
// and provider-set decisions, and it cannot be answered from the client's
// answer alone.
type routerUpstream struct {
	server *httptest.Server
	port   int

	mu            sync.Mutex
	requests      int
	authorization string
	model         string
	// refuseStatus, when non-zero, makes every answer this endpoint gives be
	// that status instead of serving the request. It answers with a body even
	// then, because "the provider said 503" only reaches the client if the
	// provider actually said something — an empty body is a stalled stream,
	// which is a different behavior the failover path treats differently.
	refuseStatus int
	// holdGate, when non-nil, makes every answer wait until the gate is
	// closed or the request is cancelled. Some tests need a job that is
	// mid-flight at the moment a listener closes under it; a held answer is
	// the reliable way to pin that moment, where a fixed sleep is not.
	holdGate chan struct{}
}

// holdResponsesUntilRelease makes every answer from this endpoint wait at a
// gate until the returned release runs. The wait also ends when the request's
// context is cancelled, and the gate is closed by test cleanup either way,
// so a failing test cannot strand the mock server's Close.
func (u *routerUpstream) holdResponsesUntilRelease(t *testing.T) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	u.mu.Lock()
	u.holdGate = gate
	u.mu.Unlock()
	t.Cleanup(release)
	return release
}

func newRouterUpstream(t *testing.T) *routerUpstream {
	t.Helper()
	upstream := &routerUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "mock upstream could not read the body", http.StatusBadRequest)
			return
		}
		var received struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &received); err != nil {
			http.Error(w, "mock upstream was forwarded a body that is not JSON", http.StatusBadRequest)
			return
		}

		upstream.mu.Lock()
		upstream.requests++
		upstream.authorization = r.Header.Get("Authorization")
		upstream.model = received.Model
		attempt := upstream.requests
		refuse := upstream.refuseStatus
		hold := upstream.holdGate
		upstream.mu.Unlock()

		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if refuse != 0 {
			w.WriteHeader(refuse)
			_, _ = w.Write([]byte(`{"error":"mock upstream is refusing"}`))
			return
		}
		// The answer echoes the model this endpoint was asked to run, so the
		// client's view of it is what proves the router does not edit
		// responses: whatever identifier actually ran the job is what comes
		// back.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"attempt": attempt,
			"model":   received.Model,
		})
	}))
	t.Cleanup(upstream.server.Close)

	address, ok := upstream.server.Listener.Addr().(*net.TCPAddr)
	require.Truef(t, ok, "mock upstream listens on a %T, want *net.TCPAddr", upstream.server.Listener.Addr())
	upstream.port = address.Port
	return upstream
}

func (u *routerUpstream) refuse(code int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refuseStatus = code
}

func (u *routerUpstream) requestCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests
}

func (u *routerUpstream) lastModel() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.model
}

func (u *routerUpstream) lastAuthorization() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.authorization
}

// routerFixture names one provider: which engine on which node, advertising
// these native identifiers.
type routerFixture struct {
	engine string
	nodeID string
	models []string
}

func routerEngineServes(engine, nodeID string, models ...string) routerFixture {
	return routerFixture{engine: engine, nodeID: nodeID, models: models}
}

// routerHarness is a process hosting every engine, one mock upstream per
// fixture, and the cluster endpoint over them.
type routerHarness struct {
	proxy    *Proxy
	router   *clusterRouter
	events   *recRW
	upstream map[string]*routerUpstream
}

// routerTestCluster builds the process and its cluster picture.
//
// The router is never started: the binding is a policy its start method
// documents rather than behavior with branches. Every engine gets a facade
// because the router indexes whatever this process hosts, and a fixture only
// registers the nodes its scenario is about — an engine with no registered
// node contributes no providers, which is what a cluster that does not run it
// looks like from here.
func routerTestCluster(t *testing.T, fixtures ...routerFixture) *routerHarness {
	t.Helper()

	redirectConfigDir(t)
	recorder := &recRW{}
	p := NewProxy(NewCodec(recorder))
	p.runID = "run-under-test"
	p.serveCtx = t.Context()
	for _, e := range engines.All() {
		enableOnFreePort(t, p, e.Name)
	}
	t.Cleanup(func() { p.shutdown(t.Context()) })

	upstreams := make(map[string]*routerUpstream, len(fixtures))
	for _, fixture := range fixtures {
		f := p.facadeFor(fixture.engine)
		require.NotNilf(t, f, "engine %q has no facade, so no node can run it", fixture.engine)
		upstream := newRouterUpstream(t)
		upstreams[fixture.nodeID] = upstream
		// Manual because that is how a user points PAIR at an engine discovery
		// cannot see, and the router is expected to honor it: the entry is
		// dialed over plain HTTP and is scoped to this one engine.
		f.discovery.AddManual(Node{
			ID:        fixture.nodeID,
			Host:      "127.0.0.1",
			Addresses: []string{"127.0.0.1"},
			Port:      upstream.port,
			Models:    fixture.models,
		})
	}

	return &routerHarness{
		proxy:    p,
		router:   newClusterRouter(p, 0),
		events:   recorder,
		upstream: upstreams,
	}
}

// upstream returns the mock endpoint standing in for a fixture's node.
func (h *routerHarness) provider(t *testing.T, nodeID string) *routerUpstream {
	t.Helper()
	upstream, ok := h.upstream[nodeID]
	require.Truef(t, ok, "no mock upstream was built for node %q", nodeID)
	return upstream
}

// serve answers one request from the cluster endpoint without a socket.
func (h *routerHarness) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.router.handle(rec, req)
	return rec
}

// request builds a call to the cluster endpoint. The returned request is the
// caller's to set headers on before serving it.
func (h *routerHarness) request(method, path, body string) *http.Request {
	return httptest.NewRequest(method, path, strings.NewReader(body))
}

func (h *routerHarness) do(method, path, body string) *httptest.ResponseRecorder {
	return h.serve(h.request(method, path, body))
}

// hops resolves a name the way the request path does and keeps only the
// providers this process could dial right now.
func (h *routerHarness) hops(t *testing.T, model string) []routerHop {
	t.Helper()
	catalog := h.router.catalogNow(time.Now())
	return h.router.dialableHops(catalog.resolveProviders(model))
}

// workloadEvent is one workload notification as the broker receives it.
type workloadEvent struct {
	Method string `json:"method"`
	Params struct {
		WorkloadInfo Workload `json:"workloadInfo"`
	} `json:"params"`
}

// workloadEvents decodes the job events this process has published, in the
// order they were written. A job is stored under (originatedFrom, engine,
// runId, id), so those fields are read off the frames rather than inferred
// from a count: an id that collided would overwrite a job card and look like
// a shorter sequence.
func workloadEvents(t *testing.T, recorder *recRW) []workloadEvent {
	t.Helper()
	var events []workloadEvent
	for _, line := range strings.Split(recorder.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event workloadEvent
		require.NoErrorf(t, json.Unmarshal([]byte(line), &event), "decode (%s)", line)
		switch event.Method {
		case workloadSubmittedMethod, workloadStartedMethod, workloadCompletedMethod, workloadErroredMethod:
			events = append(events, event)
		}
	}
	return events
}

func eventMethods(events []workloadEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.Method)
	}
	return out
}

// distinctWorkloadIDs returns the job ids an event stream names, each once,
// in first-seen order. More than one means one request published one card per
// attempt, which every scheduler downstream would count separately.
func distinctWorkloadIDs(events []workloadEvent) []string {
	var out []string
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		id := event.Params.WorkloadInfo.ID
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// decodedErrorCode reads the "code" field out of the endpoint's own JSON error
// body, so a test can assert which local verdict the caller received without
// matching prose.
func decodedErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var fields struct {
		Code string `json:"code"`
	}
	require.NoErrorf(t, json.Unmarshal(body, &fields), "decode (%s)", body)
	return fields.Code
}

// A request's dialect decides which engines may be asked to run it: the shared
// OpenAI and Anthropic surfaces belong to the engines that declare them, and
// an engine-native path belongs to that engine alone.
func TestRouterInferenceEngines_ScopesARequestToTheEnginesThatDeclareItsPath(t *testing.T) {
	t.Run("the OpenAI chat surface belongs to every engine", func(t *testing.T) {
		assert.Equal(t, []string{"ollama", "lmstudio", "llamacpp", "vllm"},
			routerInferenceEngines("/v1/chat/completions"))
	})

	// llama.cpp and vLLM do not declare the Anthropic surface, so an agent
	// calling it cannot be sent to them: the request would arrive at an
	// endpoint that is not there.
	t.Run("the Anthropic surface belongs to the two engines that declare it", func(t *testing.T) {
		assert.Equal(t, []string{"ollama", "lmstudio"}, routerInferenceEngines("/v1/messages"))
	})

	t.Run("an engine-native path belongs to that engine alone", func(t *testing.T) {
		assert.Equal(t, []string{"ollama"}, routerInferenceEngines("/api/chat"))
	})

	// A model lookup is not inference. An empty answer means "this endpoint
	// does not route this path" — not an invitation to forward it anyway.
	t.Run("a model lookup names no engine", func(t *testing.T) {
		assert.Empty(t, routerInferenceEngines("/api/show"))
		assert.Empty(t, routerInferenceEngines("/api/tags"))
		assert.Empty(t, routerInferenceEngines("/v1/models"))
	})
}

// The router forwards a request for the provider's own identifier, because
// that is the string the engine matches on. Everything else about the body
// travels unchanged.
func TestRewriteRouterModel_SetsTheProvidersOwnSpellingOnTheForwardedBody(t *testing.T) {
	got, changed := rewriteRouterModel(
		[]byte(`{"model":"qwen3","messages":[{"role":"user","content":"héllo"}],"stream":true}`),
		"qwen3:latest")
	require.True(t, changed, "nothing was rewritten")

	var fields map[string]json.RawMessage
	require.NoErrorf(t, json.Unmarshal(got, &fields), "decode (%s)", got)
	assert.Equal(t, `"qwen3:latest"`, string(fields["model"]), "the model field was not replaced")
	assert.Equal(t, `true`, string(fields["stream"]), "a sibling field was lost")
	assert.Contains(t, string(fields["messages"]), "héllo", "a sibling field was damaged")
}

// A body the router finds no replaceable model field in is forwarded untouched:
// adding a field would change the request in a way the provider has no way to
// expect, and a body that already names the provider's own spelling needs no
// edit to begin with.
func TestRewriteRouterModel_LeavesAnythingWithoutAStringModelFieldUntouched(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty body", body: ``},
		{name: "not JSON", body: `model=qwen3`},
		{name: "JSON array", body: `[{"model":"qwen3"}]`},
		{name: "no model field", body: `{"messages":[]}`},
		{name: "model is not a string", body: `{"model":{"name":"qwen3"}}`},
		{name: "model already the provider's spelling", body: `{"model":"qwen3:latest"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := rewriteRouterModel([]byte(tc.body), "qwen3:latest")
			assert.False(t, changed, "the body was rewritten to %s", got)
			assert.Nil(t, got)
		})
	}
}

// A provider set is not a dial list until each entry can be reached from here.
// Each of these is a host the catalog says serves the model and this process
// must therefore refuse: an unpaired peer, a stale manual record, this node's
// own endpoint without a backend behind it, an engine no longer hosted here.
// In each case the catalog still resolves the name — the refusal is the dial
// decision, made per walk against live state, not the resolution.
func TestDialableHops_DropsProvidersThisProcessCannotDial(t *testing.T) {
	t.Run("an unpaired peer is refused even while its record is still listed", func(t *testing.T) {
		h := routerTestCluster(t)
		// A snapshot taken while this peer was in the cluster. There is no
		// mesh in this process at all, which is the same answer as a pin
		// removed: cluster mTLS requires a live pin, and nothing else is left
		// to justify dialing a stranger.
		entries := []routerEndpoint{{
			nodeID: "host-a", engine: "ollama", host: "10.0.0.9",
			addresses: []string{"10.0.0.9"}, port: 11434,
			peerUUID: "gone-from-cluster",
			models:   []string{"qwen3:latest"},
		}}
		require.NotEmpty(t, newRouterCatalog(entries, nil).resolveProviders("qwen3:latest"),
			"the fixture resolves to nothing, so the assertion below proves nothing")

		assert.Empty(t, h.router.dialableHops(
			newRouterCatalog(entries, nil).resolveProviders("qwen3:latest")))
	})

	t.Run("a manual record the engine no longer holds is not a plain-HTTP bypass", func(t *testing.T) {
		h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "qwen3:latest"))
		catalog := h.router.catalogNow(time.Now())
		require.NotEmpty(t, catalog.resolveProviders("qwen3:latest"),
			"the snapshot does not carry the manual node, so the assertion below proves nothing")

		// The snapshot says manual, and the facade's live overlay has been
		// cleared since: trusting the snapshot would dial a stranger over
		// plaintext on the strength of a stale membership answer.
		require.True(t, h.proxy.facadeFor("ollama").discovery.RemoveManual("host-a"))

		assert.Empty(t, h.hops(t, "qwen3:latest"))
	})

	t.Run("this node's own endpoint is refused when no local backend is registered", func(t *testing.T) {
		h := routerTestCluster(t)
		ownPort, _ := h.proxy.facadeFor("ollama").selfAddresses()
		entries := []routerEndpoint{{
			nodeID: "this-node", engine: "ollama", host: "127.0.0.1",
			addresses: []string{"127.0.0.1"}, port: ownPort,
			manual: true,
			models: []string{"qwen3:latest"},
		}}

		assert.Empty(t, h.router.dialableHops(
			newRouterCatalog(entries, nil).resolveProviders("qwen3:latest")),
			"the route would forward to this proxy's own listener")
	})

	t.Run("an engine this process no longer hosts is refused", func(t *testing.T) {
		h := routerTestCluster(t)
		entries := []routerEndpoint{{
			nodeID: "host-a", engine: "lmstudio", host: "127.0.0.1",
			addresses: []string{"127.0.0.1"}, port: 40111,
			manual: true,
			models: []string{"qwen3-8b"},
		}}
		// A picture taken while LM Studio was hosted here: no facade is left
		// to answer for it, so nothing knows its dialect or its trust
		// boundary, and even the manual bypass has no discovery to check.
		h.proxy.facadeMu.Lock()
		delete(h.proxy.facades, "lmstudio")
		h.proxy.facadeMu.Unlock()

		assert.Empty(t, h.router.dialableHops(
			newRouterCatalog(entries, nil).resolveProviders("qwen3-8b")),
			"a provider with no facade here was treated as dialable")
	})
}

// A manual entry is dialed plain HTTP because that is the bypass a user asked
// for, and it is scoped to the engine it was registered under: the same node
// id on another engine is still a stranger.
func TestDialableHops_DialsAManualEndpointAsPlainHTTPForItsOwnEngineOnly(t *testing.T) {
	const manualPort = 40122
	manual := routerEndpoint{
		nodeID: "manual-node", engine: "ollama", host: "127.0.0.1",
		addresses: []string{"127.0.0.1"}, port: manualPort,
		manual: true,
		models: []string{"qwen3:latest"},
	}

	t.Run("plain HTTP for the engine it was added under", func(t *testing.T) {
		h := routerTestCluster(t)
		h.proxy.facadeFor("ollama").discovery.AddManual(Node{
			ID: "manual-node", Host: "127.0.0.1", Addresses: []string{"127.0.0.1"}, Port: manualPort,
		})

		hops := h.router.dialableHops(newRouterCatalog([]routerEndpoint{manual}, nil).resolveProviders("qwen3:latest"))
		require.Len(t, hops, 1)
		assert.Equal(t, "http", hops[0].cand.url.Scheme)
		assert.Equal(t, "127.0.0.1:40122", hops[0].cand.url.Host)
		assert.Equal(t, "", hops[0].cand.peerUUID, "a manual bypass must not be dialed with a cluster principal")
	})

	t.Run("refused for a different engine claiming the same node id", func(t *testing.T) {
		h := routerTestCluster(t)
		h.proxy.facadeFor("ollama").discovery.AddManual(Node{
			ID: "manual-node", Host: "127.0.0.1", Addresses: []string{"127.0.0.1"}, Port: manualPort,
		})
		mislabelled := manual
		mislabelled.engine = "lmstudio"

		// The manual entry lives in the Ollama overlay. The LM Studio facade
		// does not know this node, so there is no bypass registered for it
		// under this engine — and a manual entry for one engine must not
		// launder plaintext traffic to another engine's port.
		assert.Empty(t, h.router.dialableHops(
			newRouterCatalog([]routerEndpoint{mislabelled}, nil).resolveProviders("qwen3:latest")))
	})
}

// A provider naming this node's own endpoint is served from the local backend
// the broker registered, never from this process's own listener: forwarding to
// itself is the self-forward loop the guard exists to break, and it would turn
// one request into two.
func TestDialableHops_RoutesThisNodesOwnEndpointToItsLocalBackend(t *testing.T) {
	backend := newRouterUpstream(t)
	h := routerTestCluster(t)
	f := h.proxy.facadeFor("ollama")
	ownPort, _ := f.selfAddresses()
	require.NoErrorf(t, f.setLocalBackend(localBackend{
		Engine: "ollama", Host: "127.0.0.1", Port: backend.port, Healthy: true,
	}), "local backend (%v)", backend.port)

	// The advertised picture of this machine: what this node announces is its
	// own facade port, so that is what every other host routes to — and what
	// this process must refuse to dial itself.
	f.discovery.AddManual(Node{
		ID: "this-node", Host: "127.0.0.1", Addresses: []string{"127.0.0.1"},
		Port:   ownPort,
		Models: []string{"qwen3:latest"},
	})

	hops := h.hops(t, "qwen3:latest")
	require.Len(t, hops, 1)
	assert.Equal(t, backend.port, hopPort(t, hops[0]),
		"the own-endpoint provider was not replaced with the registered backend")

	rec := h.do(http.MethodPost, "/api/chat", `{"model":"qwen3"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, backend.requestCount())
	assert.Equal(t, "qwen3:latest", backend.lastModel(),
		"the local backend was sent the caller's spelling, not its own")
}

// hopPort returns the port a hop would dial.
func hopPort(t *testing.T, hop routerHop) int {
	t.Helper()
	_, port, err := net.SplitHostPort(hop.cand.url.Host)
	require.NoErrorf(t, err, "split (%s)", hop.cand.url.Host)
	code, err := strconv.Atoi(port)
	require.NoErrorf(t, err, "port (%s)", port)
	return code
}

// The provider a cluster route tries first is the one the scheduler ranks
// first — the same ranking the dedicated facades follow.
func TestClusterRouterTriesTheProviderSchedulerPriorityRanksFirst(t *testing.T) {
	h := routerTestCluster(t,
		// Node ids in the opposite order of the priority list, so a route that
		// ignored the list would land on host-a.
		routerEngineServes("ollama", "host-a", "ranked-model:latest"),
		routerEngineServes("ollama", "host-b", "ranked-model:latest"),
	)
	h.proxy.SetPrioritySnapshot(schedulerwire.Priority{
		Generation: 1,
		Nodes:      []string{"host-b", "host-a"},
		Ranks:      []schedulerwire.NodeRank{{ID: "host-b"}, {ID: "host-a"}},
	})

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/chat/completions", `{"model":"ranked-model"}`).Code)
	assert.Equal(t, 1, h.provider(t, "host-b").requestCount(), "the request went to the provider priority ranks second")
	assert.Equal(t, 0, h.provider(t, "host-a").requestCount(), "a provider left untried was dialed")
}

// The router books and reads the same capacity ledger the dedicated facades
// use, so load this process already counts — however it was booked — moves the
// first attempt of a cluster route. Without that, the router would be the path
// that routes a burst onto the node everything else just backed away from.
func TestClusterRouterRoutesAroundLoadBookedThroughTheSharedLedger(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("ollama", "host-a", "busy-model:latest"),
		routerEngineServes("ollama", "host-b", "busy-model:latest"),
	)
	h.proxy.SetPrioritySnapshot(schedulerwire.Priority{
		Generation: 1,
		Nodes:      []string{"host-a", "host-b"},
		Ranks:      []schedulerwire.NodeRank{{ID: "host-a"}, {ID: "host-b"}},
	})

	// A request that is not this route's: a dispatch through a dedicated
	// facade has host-a in flight.
	_, held := h.proxy.reserveCandidate("", reservationCandidates("host-a", "host-b"))
	require.True(t, held.held, "the ledger took no reservation from the scheduler snapshot")
	require.Equal(t, "host-a", held.nodeID)
	t.Cleanup(func() { h.proxy.releaseReservation(held) })

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/chat/completions", `{"model":"busy-model"}`).Code)
	assert.Equal(t, 1, h.provider(t, "host-b").requestCount(),
		"the router routed onto a host this process already counts as loaded")
	assert.Equal(t, 0, h.provider(t, "host-a").requestCount(),
		"a provider left untried was dialed while the other host was idle")
}

// A provider that cannot take the work is worth asking again somewhere else; a
// client error is the request's own fault, and replaying it spends a dispatch
// to collect the same answer.
func TestClusterRouterFailsOverForStatusesAnotherProviderMayAnswer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		wantStatus  int
		firstCount  int
		secondCount int
	}{
		{name: "no capacity on the first provider", status: http.StatusServiceUnavailable, wantStatus: http.StatusOK, firstCount: 1, secondCount: 1},
		{name: "too many requests from the first provider", status: http.StatusTooManyRequests, wantStatus: http.StatusOK, firstCount: 1, secondCount: 1},
		{name: "a gateway error from the first provider", status: http.StatusBadGateway, wantStatus: http.StatusOK, firstCount: 1, secondCount: 1},
		// The inventory a provider is listed from can go stale, so a missing
		// model is worth trying elsewhere; a client error is about the request
		// and reaches the caller unchanged.
		{name: "not found on the first provider", status: http.StatusNotFound, wantStatus: http.StatusOK, firstCount: 1, secondCount: 1},
		{name: "bad request from the first provider", status: http.StatusBadRequest, wantStatus: http.StatusBadRequest, firstCount: 1, secondCount: 0},
		{name: "unauthorized from the first provider", status: http.StatusUnauthorized, wantStatus: http.StatusUnauthorized, firstCount: 1, secondCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both engines advertise the same exact identifier, so the
			// provider set is genuinely two hosts (an exact match short-
			// circuits the naming passes, and nothing here is asking the
			// router to translate).
			h := routerTestCluster(t,
				routerEngineServes("ollama", "host-a", "failover-model"),
				routerEngineServes("lmstudio", "host-b", "failover-model"),
			)
			h.provider(t, "host-a").refuse(tc.status)

			rec := h.do(http.MethodPost, "/v1/chat/completions", `{"model":"failover-model"}`)

			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			assert.Equal(t, tc.firstCount, h.provider(t, "host-a").requestCount())
			assert.Equal(t, tc.secondCount, h.provider(t, "host-b").requestCount())
		})
	}
}

// When a request fails over, the job the broker is shown stays one job, and it
// ends on the provider that actually served. Two cards for one request would
// count twice against a host in every scheduler's view, and a card naming the
// provider that refused would blame the wrong machine for the wait.
func TestClusterRouterPublishesOneJobPlacedOnTheProviderThatServed(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("lmstudio", "host-a", "job-model"),
		routerEngineServes("ollama", "host-b", "job-model"),
	)
	h.provider(t, "host-a").refuse(http.StatusServiceUnavailable)

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/v1/chat/completions", `{"model":"job-model"}`).Code)

	events := workloadEvents(t, h.events)
	require.NotEmpty(t, events, "the walk published no job events")
	assert.Equal(t, []string{"1"}, distinctWorkloadIDs(events),
		"one request published more than one job, so the broker's store holds a card per attempt")

	// queued unplaced at admission, queued on the provider tried first, queued
	// on the provider that took over after the refusal, running, done. Nothing
	// else: a second terminal event would reopen a job the broker closed, and
	// the walk has a sync.Once in front of that.
	assert.Equal(t, []string{
		workloadSubmittedMethod, workloadSubmittedMethod, workloadSubmittedMethod,
		workloadStartedMethod, workloadCompletedMethod,
	}, eventMethods(events))

	for _, event := range events {
		info := event.Params.WorkloadInfo
		assert.Equal(t, routerClusterEngine, info.Engine, "%s named a real engine", event.Method)
		assert.Equal(t, "run-under-test", info.RunID, "%s did not carry this process's run id", event.Method)
		assert.Equal(t, "job-model", info.Model, "%s named something other than what the caller asked for", event.Method)
	}
	assert.Equal(t, "", events[0].Params.WorkloadInfo.ScheduledOn,
		"the job was placed on a node before any provider was picked")

	lastPlaced := events[2].Params.WorkloadInfo
	assert.Equal(t, "host-b", lastPlaced.ScheduledOn,
		"the placement did not follow the dispatch to the provider that took over")
	started := events[3].Params.WorkloadInfo
	assert.Equal(t, "host-b", started.ScheduledOn, "running was reported against another node than served")

	terminal := events[len(events)-1].Params.WorkloadInfo
	assert.Equal(t, "completed", terminal.State)
	assert.Nil(t, terminal.Error)
}

// A request that was never admitted is not a job: nothing was booked, so there
// is nothing for the broker to show or a scheduler to count. This is the
// facade path's rule, and the router has to keep it too or every rejected
// probe would appear in the cluster's load.
func TestClusterRouterPublishesNoJobForARequestItNeverAdmitted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{name: "names no model", path: "/v1/chat/completions", body: `{"messages":[]}`, wantStatus: http.StatusBadRequest, wantCode: "no-model"},
		{name: "names a model nothing serves", path: "/v1/chat/completions", body: `{"model":"absent-model"}`, wantStatus: http.StatusBadGateway, wantCode: "no-provider"},
		// The model exists, but only on an engine that does not declare this
		// dialect: the request cannot be carried to any of its providers.
		{name: "names a model no provider reachable in this dialect serves", path: "/api/chat", body: `{"model":"lmstudio-only"}`, wantStatus: http.StatusBadGateway, wantCode: "no-provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := routerTestCluster(t, routerEngineServes("lmstudio", "host-a", "lmstudio-only"))

			rec := h.do(http.MethodPost, tc.path, tc.body)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantCode, decodedErrorCode(t, rec.Body.Bytes()))
			assert.Empty(t, workloadEvents(t, h.events), "a rejected request published a job")
			assert.Equal(t, 0, h.provider(t, "host-a").requestCount(), "a refused request was still forwarded")
		})
	}
}

// The cluster endpoint shares the facade's ingress contract, and the rebase
// onto develop pulled that contract's body cap into the shared
// bufferBodyAndModel: a body over the cap is refused before any provider is
// consulted, so a 32 MiB blob cannot become anyone's job.
func TestClusterRouter_RejectsABodyOverTheIngressCap(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "capped-model"))

	pad := strings.Repeat("x", int(maxInferenceBodyBytes)+1)
	rec := h.do(http.MethodPost, "/v1/chat/completions",
		`{"model":"capped-model","messages":[{"role":"user","content":"`+pad+`"}]}`)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "request-body", decodedErrorCode(t, rec.Body.Bytes()))
	assert.Empty(t, workloadEvents(t, h.events), "a body refused at the cap published a job")
	assert.Equal(t, 0, h.provider(t, "host-a").requestCount(), "a body refused at the cap was still forwarded")
}

// The provider set a request resolves to is the boundary failover may never
// widen past, and the dispatch budget does not move it: when the budget stops
// the walk with providers untried, the caller is told nobody answered rather
// than being shown the fifth refusal as the cluster's verdict, and the hosts
// that were never asked stay untouched.
func TestClusterRouterStopsTheWalkAtTheDispatchBudgetWithoutWideningTheProviderSet(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("ollama", "host-a", "crowded-model:latest"),
		routerEngineServes("ollama", "host-b", "crowded-model:latest"),
		routerEngineServes("ollama", "host-c", "crowded-model:latest"),
		routerEngineServes("ollama", "host-d", "crowded-model:latest"),
		routerEngineServes("ollama", "host-e", "crowded-model:latest"),
		// A sixth provider, past the budget, and a seventh host that does not
		// serve this name at all.
		routerEngineServes("ollama", "host-f", "crowded-model:latest"),
		routerEngineServes("ollama", "host-g", "other-model:latest"),
	)
	for _, nodeID := range []string{"host-a", "host-b", "host-c", "host-d", "host-e", "host-f"} {
		h.provider(t, nodeID).refuse(http.StatusServiceUnavailable)
	}

	rec := h.do(http.MethodPost, "/v1/chat/completions", `{"model":"crowded-model"}`)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "exhausted", decodedErrorCode(t, rec.Body.Bytes()))

	dialed := 0
	for _, nodeID := range []string{"host-a", "host-b", "host-c", "host-d", "host-e"} {
		assert.Equal(t, 1, h.provider(t, nodeID).requestCount(), "%s was not asked exactly once", nodeID)
		dialed += h.provider(t, nodeID).requestCount()
	}
	assert.Equal(t, maxDispatchAttempts, dialed,
		"the walk spent a different number of dispatches than the budget allows")
	assert.Equal(t, 0, h.provider(t, "host-f").requestCount(), "a provider left untried by the budget was dialed anyway")
	assert.Equal(t, 0, h.provider(t, "host-g").requestCount(), "a host that does not serve the model was dialed")

	// The job that was admitted still gets closed: an unfinished card counts
	// against its node in every scheduler for the life of the process.
	events := workloadEvents(t, h.events)
	require.NotEmpty(t, events)
	assert.Equal(t, []string{"1"}, distinctWorkloadIDs(events))
	last := events[len(events)-1].Params.WorkloadInfo
	assert.Equal(t, "failed", last.State)
	assert.NotNil(t, last.Error)
}

// A model provided by exactly one host is that host's answer to give,
// including when that answer is that it no longer has the model. Retrying a
// single provider's refusal elsewhere would invent a second opinion, and
// rewriting it into a gateway error would hide which host said so.
func TestClusterRouterPassesTheOnlyProvidersOwnAnswerThrough(t *testing.T) {
	for _, tc := range []struct {
		name       string
		refuse     int
		wantStatus int
	}{
		{name: "the only provider no longer has the model", refuse: http.StatusNotFound, wantStatus: http.StatusNotFound},
		{name: "the only provider is at capacity", refuse: http.StatusServiceUnavailable, wantStatus: http.StatusServiceUnavailable},
		{name: "the only provider serves the request", refuse: 0, wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "sole-model:latest"))
			if tc.refuse != 0 {
				h.provider(t, "host-a").refuse(tc.refuse)
			}

			rec := h.do(http.MethodPost, "/v1/chat/completions", `{"model":"sole-model"}`)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, 1, h.provider(t, "host-a").requestCount())
		})
	}
}

// The cluster inventory is generated from the same provider sets routing
// reads, so a name is listed exactly when it routes: one row per name, and no
// row for a name whose providers cannot be dialed from here right now.
func TestClusterRouterListsOnlyNamesSomeProviderCurrentlyRoutes(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("ollama", "host-a", "listed-model:latest"),
		// The same identifier on a second engine: one row either way.
		routerEngineServes("llamacpp", "host-b", "listed-model:latest"),
	)

	listModelIDs := func(t *testing.T) []string {
		t.Helper()
		rec := h.do(http.MethodGet, "/v1/models", ``)
		require.Equal(t, http.StatusOK, rec.Code)
		var listing struct {
			Object string `json:"object"`
			Data   []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		require.NoErrorf(t, json.Unmarshal(rec.Body.Bytes(), &listing), "decode (%s)", rec.Body.String())
		assert.Equal(t, "list", listing.Object)
		ids := make([]string, 0, len(listing.Data))
		for _, row := range listing.Data {
			assert.Equal(t, "model", row.Object)
			// owned_by names the routing class because the answering engine is
			// not decided at list time; an engine's name here would read as a
			// routing promise the next load-based choice could break.
			assert.Equal(t, routerClusterEngine, row.OwnedBy)
			ids = append(ids, row.ID)
		}
		return ids
	}

	assert.Equal(t, []string{"listed-model:latest"}, listModelIDs(t),
		"a name was listed twice, or an untagged spelling of a listed name was added as a second row")

	// A second provider for a new name, but one this process cannot dial: a
	// cluster peer with no pin (this process is not in a cluster at all). The
	// catalog carries it — it is the picture of the cluster — and the dial gate
	// refuses it, so the listing must too.
	h.proxy.facadeFor("vllm").discovery.SetSubscribed([]Node{{
		ID: "host-c", Host: "10.9.9.9", Addresses: []string{"10.9.9.9"}, Port: 8000,
		Models: []string{"unreachable-model"}, ClusterUUID: "unpaired-peer",
	}})
	// The router caches its catalog briefly; drop the cached projection so the
	// next request re-reads the cluster, which is what a join looks like.
	h.router.catalog = nil

	assert.Equal(t, []string{"listed-model:latest"}, listModelIDs(t),
		"a name whose only provider cannot be dialed from here was listed")
}

// A provider is asked to run the model it advertises, not the name the caller
// used, and the answer it gives about that model reaches the caller unedited.
func TestClusterRouterAsksEachProviderForTheModelItAdvertises(t *testing.T) {
	// Both engines hold the same weights under different spellings.
	serveBothSpellings := func(t *testing.T) *routerHarness {
		t.Helper()
		return routerTestCluster(t,
			routerEngineServes("ollama", "host-a", "qwen3-8b:latest"),
			routerEngineServes("vllm", "host-b", "Qwen/Qwen3-8B"),
		)
	}

	t.Run("an untagged Ollama name is sent tagged", func(t *testing.T) {
		h := serveBothSpellings(t)

		rec := h.do(http.MethodPost, "/api/chat", `{"model":"qwen3-8b"}`)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "qwen3-8b:latest", h.provider(t, "host-a").lastModel(),
			"the provider was sent the caller's spelling")
		assert.Equal(t, 0, h.provider(t, "host-b").requestCount(),
			"the request outlived the dialect the client used")
		// The provider's own echo, unchanged: the router does not edit
		// responses, so the caller sees which identifier actually ran the job.
		assert.Contains(t, rec.Body.String(), "qwen3-8b:latest")
	})

	t.Run("a name only one engine spells exactly is not offered to the other", func(t *testing.T) {
		h := serveBothSpellings(t)

		rec := h.do(http.MethodPost, "/v1/chat/completions", `{"model":"Qwen/Qwen3-8B"}`)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "Qwen/Qwen3-8B", h.provider(t, "host-b").lastModel(),
			"an exact identifier was rewritten as if it needed translating")
		assert.Equal(t, 0, h.provider(t, "host-a").requestCount(),
			"an exact match gained a provider the caller never named")
	})
}

// Inference carries the caller's credential through, as a dedicated engine
// endpoint does: whoever can reach this loopback port can already reach each
// engine's own port with the same token. The cluster inventory is the opposite
// case — it is generated here, so no upstream is asked and no credential
// travels anywhere.
func TestClusterRouterCarriesTheCallersOwnCredentialToTheProvider(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "credentialed-model:latest"))

	req := h.request(http.MethodPost, "/api/chat", `{"model":"credentialed-model"}`)
	req.Header.Set("Authorization", "Bearer caller-token")
	require.Equal(t, http.StatusOK, h.serve(req).Code)
	assert.Equal(t, "Bearer caller-token", h.provider(t, "host-a").lastAuthorization())

	dialedByInference := h.provider(t, "host-a").requestCount()
	require.Equal(t, http.StatusOK, h.do(http.MethodGet, "/v1/models", ``).Code)
	assert.Equal(t, dialedByInference, h.provider(t, "host-a").requestCount(),
		"listing asked a provider for its inventory and carried the caller's credential with it")
}

// A request whose provider set is empty — because nothing serves the name, or
// because no provider can be reached from here — is answered locally with
// nothing dialed. "I cannot reach anybody who serves this" must never become
// "let me try whatever is up".
func TestClusterRouterRejectsARequestNobodyInItsProviderSetCanAnswer(t *testing.T) {
	t.Run("no host serves the name", func(t *testing.T) {
		h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "other-model:latest"))

		rec := h.do(http.MethodPost, "/v1/chat/completions", `{"model":"absent-model"}`)

		assert.Equal(t, http.StatusBadGateway, rec.Code)
		assert.Equal(t, "no-provider", decodedErrorCode(t, rec.Body.Bytes()))
		assert.Equal(t, 0, h.provider(t, "host-a").requestCount(),
			"a host that does not serve the model was dialed after the set came up empty")
	})

	t.Run("every provider speaks another dialect", func(t *testing.T) {
		h := routerTestCluster(t, routerEngineServes("vllm", "host-a", "shared-model"))

		// /api/chat is Ollama's surface. The model exists on a vLLM host, and
		// that host stays untouched: the request cannot be spoken there.
		rec := h.do(http.MethodPost, "/api/chat", `{"model":"shared-model"}`)

		assert.Equal(t, http.StatusBadGateway, rec.Code)
		assert.Equal(t, "no-provider", decodedErrorCode(t, rec.Body.Bytes()))
		assert.Equal(t, 0, h.provider(t, "host-a").requestCount(),
			"an engine that does not declare this path was sent a request for it")
	})

	t.Run("no provider can be dialed from here", func(t *testing.T) {
		h := routerTestCluster(t)
		// The record is in the overlay and the engine is hosted here, but the
		// node is an unpaired peer, so there is no way to reach it that this
		// process trusts.
		h.proxy.facadeFor("vllm").discovery.SetSubscribed([]Node{{
			ID: "host-a", Host: "10.9.9.9", Addresses: []string{"10.9.9.9"}, Port: 8000,
			Models: []string{"unreachable-model"}, ClusterUUID: "unpaired-peer",
		}})
		catalog := h.router.catalogNow(time.Now())
		require.NotEmpty(t, catalog.resolveProviders("unreachable-model"),
			"a provider exists and is listed; the refusal below must be the dial decision")

		rec := h.do(http.MethodPost, "/v1/chat/completions", `{"model":"unreachable-model"}`)

		assert.Equal(t, http.StatusBadGateway, rec.Code)
		assert.Equal(t, "no-provider", decodedErrorCode(t, rec.Body.Bytes()))
	})
}

// Engine-manager probes a port to tell an engine from a compatibility facade,
// and the cluster endpoint is neither: it refuses, because answering with a
// forwarded response would report an engine present where only a router
// listens.
func TestClusterRouterRefusesTheEngineIdentityProbeWithoutRouting(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "probed-model:latest"))

	req := h.request(http.MethodPost, "/v1/chat/completions", `{"model":"probed-model"}`)
	req.Header.Set(engineIdentityProbeHeader, "1")
	rec := h.serve(req)

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "router-not-engine", decodedErrorCode(t, rec.Body.Bytes()))
	assert.Equal(t, 0, h.provider(t, "host-a").requestCount(), "a probe was answered by running somebody's job")
}

// What the endpoint does not route is refused locally: an unlisted path has no
// provider to answer for it, and a request whose method does not fit the path
// is not a request this endpoint received correctly.
func TestClusterRouterRefusesPathsOutsideItsSurface(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "surface-model:latest"))

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "an engine control path", method: http.MethodPost, path: "/api/pull", body: `{"name":"qwen3"}`},
		{name: "an engine list path", method: http.MethodGet, path: "/api/tags", body: ``},
		{name: "a path no engine declares", method: http.MethodGet, path: "/health", body: ``},
		{name: "the inventory asked for with POST", method: http.MethodPost, path: "/v1/models", body: ``},
		{name: "inference asked for with GET", method: http.MethodGet, path: "/v1/chat/completions", body: ``},
		{name: "a model lookup", method: http.MethodPost, path: "/api/show", body: `{"name":"qwen3"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.Equal(t, "not-routable", decodedErrorCode(t, rec.Body.Bytes()))
		})
	}
	assert.Equal(t, 0, h.provider(t, "host-a").requestCount(), "a refused request reached a provider")
}

// The catalog is rebuilt on a clock, so a burst of requests pays for one
// projection of the cluster instead of one each — and a node that joined is
// picked up when the projection ages out. The test drives the clock through
// the same parameter the request path passes.
func TestClusterRouterRebuildsItsCatalogAfterTheTTLAndReusesItBefore(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "early-model:latest"))

	start := time.Now()
	first := h.router.catalogNow(start)
	assert.Empty(t, first.resolveProviders("later-model:latest"),
		"the projection taken before the join already sees it, so the rebuild check proves nothing")

	// A node joins after that projection was taken.
	h.proxy.facadeFor("ollama").discovery.AddManual(Node{
		ID: "host-b", Host: "127.0.0.1", Addresses: []string{"127.0.0.1"}, Port: newRouterUpstream(t).port,
		Models: []string{"later-model:latest"},
	})

	reused := h.router.catalogNow(start.Add(routerCatalogTTL / 2))
	assert.Same(t, first, reused,
		"the catalog was rebuilt inside the TTL, so a burst of requests paid for one projection each")

	replaced := h.router.catalogNow(start.Add(routerCatalogTTL + time.Millisecond))
	assert.NotSame(t, first, replaced,
		"the catalog was never rebuilt, so a node that joined is still invisible")
	assert.NotEmpty(t, replaced.resolveProviders("later-model:latest"),
		"the rebuilt catalog still does not resolve the model the new node advertises")
}

// TestClusterRouterSetAliases_MakesTheAliasLiveOnTheNextRead: setAliases is
// the broker's injection point for configured aliases, and configuration
// does not age in on a cache timer — the alias must be listed and routable
// on the very next request, not after the catalog TTL.
func TestClusterRouterSetAliases_MakesTheAliasLiveOnTheNextRead(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))

	before := h.do(http.MethodGet, "/v1/models", "")
	require.Equal(t, http.StatusOK, before.Code)
	assert.NotContains(t, before.Body.String(), "quick-chat",
		"an unconfigured alias was listed")

	h.router.setAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}})

	listed := h.do(http.MethodGet, "/v1/models", "")
	require.Equal(t, http.StatusOK, listed.Code)
	assert.Contains(t, listed.Body.String(), "quick-chat",
		"a configured synonym for a model a live endpoint serves stayed out of the cached catalog")

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "llama3.2:1b", h.provider(t, "host-a").lastModel(),
		"the provider was asked to run the alias name instead of its own identifier")
}

// TestClusterRouterSetAliases_ReplacementRetiresThePreviousList: the list is
// replaced whole, so a dropped alias stops being listed and stops routing
// immediately — an editor's delete must be as live as its add.
func TestClusterRouterSetAliases_ReplacementRetiresThePreviousList(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	h.router.setAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}})
	require.Contains(t, h.do(http.MethodGet, "/v1/models", "").Body.String(), "quick-chat")

	h.router.setAliases([]noderec.ModelAlias{{Name: "other-name", Model: "llama3.2:1b"}})

	listing := h.do(http.MethodGet, "/v1/models", "").Body.String()
	assert.NotContains(t, listing, "quick-chat", "a deleted alias stayed listed (%s)", listing)
	assert.Contains(t, listing, "other-name", "the replacing alias did not arrive")

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code, "a deleted alias still routed (%s)", rec.Body.String())
	assert.Equal(t, "no-provider", decodedErrorCode(t, rec.Body.Bytes()))
}

// The proxy request surface for alias configuration: the document is stored
// on the process whether or not this process hosts a router, and a live
// router also takes the push immediately. Storing without an endpoint is
// deliberate — a refusal is not a neutral answer, because the broker couriers
// the document once per consumer bring-up and again on change, so refusing
// would leave an endpoint started later routing only exact native names.
func TestClusterSetAliases_StoresTheDocumentWhenNoEndpointIsUp(t *testing.T) {
	rec := &recRW{}
	p := NewProxy(NewCodec(rec))
	require.Nil(t, p.clusterRouter, "the case under test is a process with no cluster endpoint")

	id := json.RawMessage(`3`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "cluster/set-aliases",
		Params:  json.RawMessage(`{"aliases":[{"name":"quick-chat","model":"llama3.2:1b"}]}`),
	})

	frame := replyFrame(t, rec.String())
	require.Nil(t, frame.Error, "a router-less proxy refused an alias push")
	var reply map[string]bool
	require.NoError(t, json.Unmarshal(frame.Result, &reply))
	assert.Equal(t, map[string]bool{"ok": true}, reply)

	require.Len(t, p.clusterAliasDoc, 1, "the accepted push was dropped")
	assert.Equal(t, noderec.ModelAlias{Name: "quick-chat", Model: "llama3.2:1b"}, p.clusterAliasDoc[0],
		"the kept document is not the pair of names as sent")
}

func TestClusterSetAliases_StoresTheListOnTheEnabledRouter(t *testing.T) {
	rec := &recRW{}
	p := NewProxy(NewCodec(rec))
	router := newClusterRouter(p, 0)
	p.clusterRouter = router

	id := json.RawMessage(`4`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "cluster/set-aliases",
		Params:  json.RawMessage(`{"aliases":[{"name":"quick-chat","model":"llama3.2:1b"}]}`),
	})

	require.Contains(t, rec.String(), `"ok":true`, "the push was refused")
	router.mu.Lock()
	stored := router.aliases
	router.mu.Unlock()
	// The stored entry is the two names as sent, which is what lets one list
	// configured on this machine mean the same thing on every other one.
	require.Len(t, stored, 1)
	assert.Equal(t, noderec.ModelAlias{Name: "quick-chat", Model: "llama3.2:1b"}, stored[0])
}

// TestRouterSetEnabled_RoutesTheAliasesStoredWhileTheEndpointWasDown drives
// the deployment's real arc: the courier's push lands while no endpoint
// exists (switch off, or the configured port taken at process start), the
// operator then turns the endpoint on, and the first requests must already
// route this node's names. It runs over a real socket and a real bring-up —
// unlike the handler-only sections — because "a router that comes up nameless"
// is a client-observable defect: a check of the router's alias field could
// pass while every request 502s.
func TestRouterSetEnabled_RoutesTheAliasesStoredWhileTheEndpointWasDown(t *testing.T) {
	p, rec := lifecycleProxy(t)

	const providerModel = "llama3.2:1b"
	upstream := newRouterUpstream(t)
	enableOnFreePort(t, p, "ollama")
	p.facadeFor("ollama").discovery.AddManual(Node{
		ID:        "host-a",
		Host:      "127.0.0.1",
		Addresses: []string{"127.0.0.1"},
		Port:      upstream.port,
		Models:    []string{providerModel},
	})

	frame := callControl(t, p, rec, "cluster/set-aliases",
		`{"aliases":[{"name":"quick-chat","model":"llama3.2:1b"}]}`)
	require.Nil(t, frame.Error, "the alias push was refused while no endpoint was up")

	port := freeTCPPort(t)
	frame = callControl(t, p, rec, "router/set-enabled", `{"enabled":true,"port":`+strconv.Itoa(port)+`}`)
	status := replyStatus(t, frame)
	require.True(t, status.Running, "the enable reported no endpoint")

	code, listing := getEndpoint(t, port)
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, listing, "quick-chat",
		"the endpoint came up nameless: the alias stored while it was down was not installed")

	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(
		"http://127.0.0.1:"+strconv.Itoa(port)+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"quick-chat","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	assert.Equal(t, providerModel, upstream.lastModel(),
		"the provider was asked to run the alias name instead of its own identifier")
}

// --- the endpoint's bound state ---------------------------------------------
//
// Whether this process has a cluster endpoint, and on which port, is answered
// from the live listener rather than from the port the desktop was configured
// with, and a port that cannot be bound costs the endpoint and
// not the process. Both are only observable through a real
// listener, so these are the tests that bind one — each through port 0.

// replyFrame decodes the codec's recorded output as the one JSON-RPC response
// it holds, so a test asserts the reply as data instead of as a substring of
// the frame. Other frames may sit in the recording — a process-scoped
// notification the bring-up emitted before the reply, for instance — so the
// response is selected by shape (a reply carries an id), and a recording with
// none is a failure rather than a silent zero-value frame.
func replyFrame(t *testing.T, recorded string) Message {
	t.Helper()
	for _, line := range strings.Split(recorded, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var frame Message
		require.NoError(t, json.Unmarshal([]byte(line), &frame), "malformed codec frame %q", line)
		if frame.ID != nil {
			return frame
		}
	}
	t.Fatal("the codec recorded no response frame")
	return Message{}
}

// TestBringUpRouter_FailsWhenThePortIsTaken covers what happens when the bind
// fails. One nvpair-proxy process hosts every engine facade, so a
// router that exits over a bind failure would take inference down on every
// engine of that machine — and the port is a number the operator can change in
// Cluster Settings, so a collision is an everyday input rather than a fault.
// The refusal must arrive as an error the caller can answer with: at process
// start it costs a log, at runtime it costs a -32011 reply, and in both cases
// whatever already served keeps serving.
func TestBringUpRouter_FailsWhenThePortIsTaken(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()

	_, occupiedPort, err := net.SplitHostPort(occupied.Addr().String())
	require.NoError(t, err)
	takenPort, err := strconv.Atoi(occupiedPort)
	require.NoError(t, err)

	p := NewProxy(NewCodec(&recRW{}))
	router, err := p.bringUpRouter(t.Context(), takenPort)

	require.ErrorIs(t, err, errRouterBindFailed,
		"a taken port must come back as the bind-failure sentinel")
	assert.Nil(t, router,
		"a cluster endpoint was brought up on port %d, which this test had already taken", takenPort)
	assert.Contains(t, err.Error(), strconv.Itoa(takenPort),
		"the refusal must name the port that could not be bound")
}

// TestRouterStatus_ReportsNoEndpointForARouterlessProcess is what a desktop sees
// on every machine where the switch is off: an absent endpoint is a normal
// configuration, so the answer is a result and not an error. A modal that had
// to catch an exception to learn "nothing here" would show a failure on a
// correctly configured machine.
func TestRouterStatus_ReportsNoEndpointForARouterlessProcess(t *testing.T) {
	rec := &recRW{}
	p := NewProxy(NewCodec(rec))

	id := json.RawMessage(`5`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "router/status",
	})

	frame := replyFrame(t, rec.String())
	require.Nil(t, frame.Error, "a router-less proxy refused the status read")

	var status routerStatusResult
	require.NoError(t, json.Unmarshal(frame.Result, &status))
	assert.Equal(t, routerStatusResult{Running: false, Port: 0}, status)
}

// TestRouterStatus_ReportsTheEndpointThatActuallyAnswers is the trust decision
// in one test: the port the read gives back is the port serving /v1/models.
// Echoing the configured port instead would advertise whatever else happens to
// listen there, which is exactly how a stranger's server on the user's chosen
// port would come to be listed as "the cluster endpoint".
func TestRouterStatus_ReportsTheEndpointThatActuallyAnswers(t *testing.T) {
	rec := &recRW{}
	p := NewProxy(NewCodec(rec))
	router, err := p.bringUpRouter(t.Context(), 0)
	require.NoError(t, err, "no cluster endpoint came up on a free port")
	p.clusterRouter = router

	id := json.RawMessage(`6`)
	p.handleMessage(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "router/status",
	})

	frame := replyFrame(t, rec.String())
	require.Nil(t, frame.Error, "the status read failed")
	var status routerStatusResult
	require.NoError(t, json.Unmarshal(frame.Result, &status))
	require.True(t, status.Running, "a live endpoint was reported as down")
	require.Positive(t, status.Port, "a live endpoint was reported without a port")

	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(status.Port) + "/v1/models")
	require.NoError(t, err, "the read reported port %d, which does not answer", status.Port)
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.JSONEq(t, `{"object":"list","data":[]}`, string(body),
		"whatever answered port %d is not the cluster router", status.Port)
}

// --- peer-announced aliases -------------------------------------------------
//
// An alias configured on another node does not arrive through setAliases. It
// rides the relay's process-scoped alias feed — a cluster:peer-aliases
// snapshot the router keeps as peerAliasNodes — and the catalog build merges
// it under the locally configured list. A synonym is two names, so arriving
// here is not yet what makes it route: what a node does with it depends on
// which engines that node can see and what they hold. These tests build the
// cluster picture first (a joined node is invisible until the catalog ages
// past its TTL, so every scenario starts complete) and then ask what this
// node's router lists and routes.
//
// The feed is fed through replacePeerAliases — the same handler the
// cluster:peer-aliases notification dispatches to — rather than through an
// engine overlay, because that is the whole shape: an announcement's weight
// comes from who wrote it (pinned and trusted), not from whether the writer
// fronts an engine.

// peerAnnounceSnapshots replaces the alias feed with exactly these peer
// records, mirroring how a real snapshot replaces the set wholesale.
func peerAnnounceSnapshots(t *testing.T, h *routerHarness, nodes ...noderec.DirectoryNode) {
	params, err := json.Marshal(noderec.GetNodesResult{Nodes: nodes})
	require.NoError(t, err)
	h.router.replacePeerAliases(params)
}

// peerAnnounceNode announces these aliases from a pinned, trusted peer that
// fronts no engine — the announcer shape the routing-overlay feed used to
// hide, and the one a peer between engine runs actually has.
func peerAnnounceNode(t *testing.T, h *routerHarness, nodeID string, aliases ...noderec.ModelAlias) {
	peerAnnounceSnapshots(t, h, noderec.DirectoryNode{
		HostUUID:             nodeID,
		Name:                 nodeID,
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		Aliases:              aliases,
	})
}

// TestClusterRouterRoutesAPeerAnnouncedAliasByLookingUpItsModelHere is the
// requirement the feature exists for: a synonym typed on one computer has to
// route on another. Nothing here is told which machine serves anything — host-b's
// announcement carries a model name, and this node's own catalog is what turns
// it into the hosts it can actually see holding that model.
func TestClusterRouterRoutesAPeerAnnouncedAliasByLookingUpItsModelHere(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	peerAnnounceNode(t, h, "host-b", noderec.ModelAlias{
		Name: "quick-chat", Model: "llama3.2:1b",
	})

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "llama3.2:1b", h.provider(t, "host-a").lastModel(),
		"the provider was asked to run the alias name instead of its own identifier")

	// Listed ⇒ routable holds for announced aliases too.
	listing := h.do(http.MethodGet, "/v1/models", "").Body.String()
	assert.Contains(t, listing, "quick-chat",
		"a routable announced alias stayed out of the listing")
}

// TestClusterRouterRoutesAPeerAnnouncedAliasToEveryHostHoldingTheModel pins the
// other half of the shape: an announcement describes a model, not the machines
// around it. host-c names a model this node sees on two hosts, so both join the
// failover set. A host list arriving instead would have named one machine, and
// the same document would then mean different things on the two computers that
// read it.
func TestClusterRouterRoutesAPeerAnnouncedAliasToEveryHostHoldingTheModel(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("ollama", "host-a", "llama3.2:1b"),
		routerEngineServes("llamacpp", "host-b", "llama3.2:1b"),
	)
	peerAnnounceNode(t, h, "host-c", noderec.ModelAlias{Name: "fast-chat", Model: "llama3.2:1b"})

	hops := h.hops(t, "fast-chat")
	require.Len(t, hops, 2, "the synonym did not reach every host holding the model: %+v", hops)
	assert.Equal(t, "host-a", hops[0].cand.id, "hops (hosts the failover walk would try, in order)")
	assert.Equal(t, "host-b", hops[1].cand.id, "hops (hosts the failover walk would try, in order)")
}

// TestClusterRouterTreatsAPeerAliasForAModelNothingHereServesAsUnknown covers the
// inert case: the announced synonym names a model no endpoint this node can
// route to. It is neither listed nor routed, and a request for it is answered
// locally with no upstream contact — an announcement cannot create a route, it
// can only hand this node a model name to look up.
func TestClusterRouterTreatsAPeerAliasForAModelNothingHereServesAsUnknown(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	peerAnnounceNode(t, h, "host-b", noderec.ModelAlias{
		Name: "ghostly", Model: "deepseek-r1",
	})

	assert.Empty(t, h.hops(t, "ghostly"), "a synonym for a model nobody here serves still resolved")

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"ghostly"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	assert.Equal(t, "no-provider", decodedErrorCode(t, rec.Body.Bytes()))

	listing := h.do(http.MethodGet, "/v1/models", "").Body.String()
	assert.NotContains(t, listing, "ghostly", "an unroutable alias was listed (%s)", listing)
}

// TestClusterRouterFeedsTheCatalogFromAnEngineLessPeerAnnouncement is the live-run
// regression: an alias written on a peer whose engine is down (or that has
// none) announced nothing at all while the feed rode the per-engine routing
// overlays, because the overlay's admission gate requires an engine service.
// The feed's gate is identity, so this announcement routes.
func TestClusterRouterFeedsTheCatalogFromAnEngineLessPeerAnnouncement(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	peerAnnounceNode(t, h, "host-b", noderec.ModelAlias{Name: "quick-chat", Model: "llama3.2:1b"})
	// The announcer holds nothing this node can dial — no overlay entry for
	// host-b exists at all, engine-less in every sense.
	hops := h.hops(t, "quick-chat")
	require.Len(t, hops, 1, "the engine-less announcer became a provider itself: %+v", hops)
	require.Equal(t, "host-a", hops[0].cand.id)

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"the alias from a peer that fronts no engine never reached the catalog: %s", rec.Body.String())
	assert.Equal(t, "llama3.2:1b", h.provider(t, "host-a").lastModel())
}

// TestClusterRouterIgnoresAliasesFromAnUnverifiedOrUntrustedAnnouncement pins the
// feed's own gate: the announcement must come from a proved identity with
// current trust, the same bar a hop has to clear to be dialled. A name a
// stranger defined must not join this router's table at all.
func TestClusterRouterIgnoresAliasesFromAnUnverifiedOrUntrustedAnnouncement(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	peerAnnounceSnapshots(t, h,
		noderec.DirectoryNode{
			HostUUID:             "host-b",
			IdentityVerification: noderec.IdentityPinned,
			Trusted:              false, // pinned once, untrusted now
			Aliases:              []noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}},
		},
		noderec.DirectoryNode{
			HostUUID:             "host-c",
			IdentityVerification: noderec.IdentityUnverified, // claimed, never proved
			Trusted:              true,
			Aliases:              []noderec.ModelAlias{{Name: "fast-chat", Model: "llama3.2:1b"}},
		},
	)

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code,
		"an alias from an untrusted peer routed: %s", rec.Body.String())
	rec = h.do(http.MethodPost, "/api/generate", `{"model":"fast-chat"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code,
		"an alias from an unverified peer routed: %s", rec.Body.String())
	assert.Zero(t, h.provider(t, "host-a").requestCount(),
		"a provider answered a request built from an unproven announcement")
}

// TestClusterRouterIgnoresThisNodesOwnRecordInThePeerFeed pins the split
// between configuration and announcement: this computer's aliases arrive via
// setAliases, and seeing its own directory record in the feed adds nothing —
// reading it as an announcement would route on a discovery-paced copy of
// configuration that is, at best, fresher-by-luck.
func TestClusterRouterIgnoresThisNodesOwnRecordInThePeerFeed(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	// The only definition of the name lives in this node's own record: set
	// configuration says nothing, so if the feed read local records the name
	// would route.
	peerAnnounceSnapshots(t, h, noderec.DirectoryNode{
		HostUUID:             "host-me",
		IdentityVerification: noderec.IdentityLocal,
		Trusted:              true,
		Aliases:              []noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}},
	})

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code,
		"this node routed by a name it only saw in its own directory record: %s", rec.Body.String())
	assert.Zero(t, h.provider(t, "host-a").requestCount())
}

// TestClusterRouterDropsAPeerAliasWhenTheAnnouncerLeavesTheFeed pins removal:
// every snapshot is the full set, so unpinning the announcer (or it deleting
// the name and re-announcing shorter) takes the alias out of service with no
// extra protocol — the next snapshot is the truth.
func TestClusterRouterDropsAPeerAliasWhenTheAnnouncerLeavesTheFeed(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	peerAnnounceNode(t, h, "host-b", noderec.ModelAlias{Name: "quick-chat", Model: "llama3.2:1b"})
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`).Code,
		"the alias did not route before the removal was applied")

	peerAnnounceSnapshots(t, h) // the announcer left the cluster

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	assert.Equal(t, http.StatusBadGateway, rec.Code,
		"a name its announcer dropped still routed: %s", rec.Body.String())
	assert.Equal(t, 1, h.provider(t, "host-a").requestCount(),
		"the dropped name still dialed a provider (one call is the pre-removal request)")
}

// TestProxyDispatchDeliversTheAliasFeedToTheClusterRouter pins the last hop of
// the feed: the cluster:peer-aliases frame is process-scoped — its method
// carries no engine address — so it must be routed by method to the router
// rather than dropped for the want of a facade.
func TestProxyDispatchDeliversTheAliasFeedToTheClusterRouter(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	h.proxy.clusterRouter = h.router // bringUpRouter's shape: the host knows its router
	params, err := json.Marshal(noderec.GetNodesResult{Nodes: []noderec.DirectoryNode{{
		HostUUID:             "host-b",
		Name:                 "host-b",
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		Aliases:              []noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}},
	}}})
	require.NoError(t, err)
	h.proxy.handleMessage(&Message{JSONRPC: "2.0", Method: noderec.NotifyPeerAliases, Params: params})

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"the delivered frame never reached the router's alias feed: %s", rec.Body.String())
	assert.Equal(t, "llama3.2:1b", h.provider(t, "host-a").lastModel())
}

// TestClusterRouterRoutesAContestedNameToTheHostTheSchedulerRanksFirst is
// the merge rule on the request path: this node and a peer each define
// "quick-chat" for their own model, both models are live, and the scheduler
// ranks the peer's host first because it has less pending work. The peer's
// definition answers, under the name this node's own operator typed, and the
// body that reaches it names the model that machine's engine expects.
//
// This is what the earlier rule forbade — a peer answering for a name this node
// defined — and forbade it to protect: it assumed a name meant one machine's
// model. The feature exists for the opposite case, one set of weights packed on
// several engines under several identifiers, where the operator wants any busy
// machine's copy to be reachable through one name.
func TestClusterRouterRoutesAContestedNameToTheHostTheSchedulerRanksFirst(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("ollama", "host-a", "llama3.2:1b"),
		routerEngineServes("ollama", "host-b", "qwen3:4b"),
	)
	peerAnnounceNode(t, h, "host-c", noderec.ModelAlias{Name: "quick-chat", Model: "qwen3:4b"})
	h.router.setAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}})
	h.proxy.SetPrioritySnapshot(schedulerwire.Priority{
		Generation: 1,
		Nodes:      []string{"host-b", "host-a"},
		Ranks:      []schedulerwire.NodeRank{{ID: "host-b"}, {ID: "host-a"}},
	})

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "qwen3:4b", h.provider(t, "host-b").lastModel(),
		"the ranked-first machine was not dialed with the wire name its own engine uses")
	assert.Zero(t, h.provider(t, "host-a").requestCount(),
		"the walk tried the busier machine before the one priority ranks first")
}

// TestClusterRouterHandsATransientDefeatFromOneDefinitionToTheOther: the point
// of a name spanning machines is that one of them being at capacity does not
// spend the request. The first definition's provider refuses with 503, and the
// walk moves to the other definition's machine and answers from it.
func TestClusterRouterHandsATransientDefeatFromOneDefinitionToTheOther(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("ollama", "host-a", "llama3.2:1b"),
		routerEngineServes("ollama", "host-b", "qwen3:4b"),
	)
	peerAnnounceNode(t, h, "host-c", noderec.ModelAlias{Name: "quick-chat", Model: "qwen3:4b"})
	h.router.setAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}})
	h.provider(t, "host-a").refuse(http.StatusServiceUnavailable)

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"a name one machine answered was refused because the first machine was busy: %s",
		rec.Body.String())
	assert.Equal(t, "qwen3:4b", h.provider(t, "host-b").lastModel())
}

// TestClusterRouterRoutesAContestedNameToThePeerDefinitionWhileTheLocalModelIsOffline
// exercises the fallback rule on the request path: this node has a configured "quick-chat"
// whose model no endpoint here serves — the engine holding it is stopped — and a
// peer announces the same name standing for a model that is live. The request
// goes to the peer. The earlier rule kept the name inert out of caution about
// being misrouted by announcement; the live run showed that caution cost more
// than it bought, because the operator's own engine going offline blacked out a
// name the cluster could still serve. The local definition keeps its claim
// while it resolves (RoutesAContestedAliasNameByTheLocalConfig) and takes the
// name back the moment its model is advertised again.
func TestClusterRouterRoutesAContestedNameToThePeerDefinitionWhileTheLocalModelIsOffline(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "qwen3:4b"))
	peerAnnounceNode(t, h, "host-b", noderec.ModelAlias{Name: "quick-chat", Model: "qwen3:4b"})
	h.router.setAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "mistral-nemo"}})

	rec := h.do(http.MethodPost, "/api/generate", `{"model":"quick-chat"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"a name whose local model is offline was refused although a peer serves it: %s",
		rec.Body.String())
	assert.Equal(t, "qwen3:4b", h.provider(t, "host-a").lastModel())

	listing := h.do(http.MethodGet, "/v1/models", "").Body.String()
	assert.Contains(t, listing, "quick-chat",
		"a name the fallback routes was left out of the listing (%s)", listing)
}

// --- the cluster alias view ---------------------------------------------------
//
// The view is what the model-aliases card shows: every name this node routes
// by, including the ones written on other machines, each marked with where it
// was written. It answers a different question than /v1/models does — "which
// names exist and whose are they", not "which names can I dial right now" — so
// it reads the selection's rows rather than the routing index: the rows keep
// the names the index itself drops, because an alias that resolves nowhere here
// is still somebody's working synonym, and hiding it would strand its owner's
// only reader.

// TestRouterAliasesView_ListsPeerNamesWithTheirOrigins is the feature the user
// asked for: a name written on another computer shows up here, marked with
// that computer, so changing it does not mean guessing which machine to open.
// A locally configured entry carries no origin — there is nowhere else it
// could have come from.
func TestRouterAliasesView_ListsPeerNamesWithTheirOrigins(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	h.router.setAliases([]noderec.ModelAlias{{Name: "mine", Model: "llama3.2:1b"}})
	peerAnnounceSnapshots(t, h, noderec.DirectoryNode{
		HostUUID:             "host-b",
		Name:                 "peer-one",
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		Aliases:              []noderec.ModelAlias{{Name: "theirs", Model: "llama3.2:1b"}},
	})

	view := h.router.aliasesView()
	require.Len(t, view.Aliases, 2)

	local, peer := view.Aliases[0], view.Aliases[1] // configured claims first, then peers by node id
	assert.Equal(t, noderec.ModelAlias{Name: "mine", Model: "llama3.2:1b"}, local.ModelAlias)
	assert.Empty(t, local.OriginNode, "a locally configured alias was marked as an announcement")
	assert.Equal(t, noderec.ModelAlias{Name: "theirs", Model: "llama3.2:1b"}, peer.ModelAlias)
	assert.Equal(t, "host-b", peer.OriginNode)
	assert.Equal(t, "peer-one", peer.OriginHost,
		"the origin names only a uuid, which an operator cannot open in the UI")
}

// TestRouterAliasesView_ShowsInertAliasesTheCatalogDrops pins the split between
// the view and the listing. "ghostly" names a model nothing here serves, so it
// is absent from /v1/models and unroutable — and must still be in the view,
// because it routes fine on the machine that wrote it and that machine is the
// only place it can be edited.
func TestRouterAliasesView_ShowsInertAliasesTheCatalogDrops(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	peerAnnounceNode(t, h, "host-b", noderec.ModelAlias{Name: "ghostly", Model: "deepseek-r1"})

	require.NotContains(t, h.do(http.MethodGet, "/v1/models", "").Body.String(), "ghostly",
		"the unroutable name leaked into the routing listing")

	view := h.router.aliasesView()
	require.Len(t, view.Aliases, 1, "the view hid the one name its owner cannot see from the listing")
	assert.Equal(t, "ghostly", view.Aliases[0].Name)
	assert.Equal(t, "host-b", view.Aliases[0].OriginNode)
}

// TestRouterAliasesView_ReportsTheWinningDefinitionOfAContestedName keeps the
// view and the routing honest to each other: the card shows the synonym
// requests actually follow, not the loser, and marks it as local — so an
// operator who disagrees with the winner edits the peer, not themselves.
// TestRouterAliasesView_ListsEveryDefinitionOfAContestedNameThatRoutes: both
// machines can serve their own definition, so both are answering for the name
// and both get a row, the local one first and unattributed, the peer's marked
// with the machine to edit it on. A card showing one of these would describe
// half the capacity behind the name.
func TestRouterAliasesView_ListsEveryDefinitionOfAContestedNameThatRoutes(t *testing.T) {
	h := routerTestCluster(t,
		routerEngineServes("ollama", "host-a", "llama3.2:1b"),
		routerEngineServes("ollama", "host-b", "qwen3:4b"),
	)
	peerAnnounceNode(t, h, "host-c", noderec.ModelAlias{Name: "quick-chat", Model: "qwen3:4b"})
	h.router.setAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "llama3.2:1b"}})

	view := h.router.aliasesView()
	require.Len(t, view.Aliases, 2, "a contested name both machines serve showed only one definition")
	assert.Equal(t, noderec.ModelAlias{Name: "quick-chat", Model: "llama3.2:1b"}, view.Aliases[0].ModelAlias,
		"the locally configured definition did not lead its name's rows")
	assert.Empty(t, view.Aliases[0].OriginNode,
		"the definition this node configured was attributed to another machine")
	assert.Equal(t, noderec.ModelAlias{Name: "quick-chat", Model: "qwen3:4b"}, view.Aliases[1].ModelAlias)
	assert.Equal(t, "host-c", view.Aliases[1].OriginNode)
}

// TestRouterAliasesView_ShowsThePeerDefinitionWhileTheLocalModelIsOffline moves
// the fallback onto the card: with this node's own target offline, the row for
// the contested name carries the peer's model and the peer's name, so the
// operator reads the same answer their requests get and knows which machine's
// definition is currently in use. Their own editor row is untouched — this is a
// routing decision, not a rewrite of anyone's configuration.
func TestRouterAliasesView_ShowsThePeerDefinitionWhileTheLocalModelIsOffline(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "qwen3:4b"))
	peerAnnounceSnapshots(t, h, noderec.DirectoryNode{
		HostUUID:             "host-b",
		Name:                 "peer-one",
		IdentityVerification: noderec.IdentityPinned,
		Trusted:              true,
		Aliases:              []noderec.ModelAlias{{Name: "quick-chat", Model: "qwen3:4b"}},
	})
	h.router.setAliases([]noderec.ModelAlias{{Name: "quick-chat", Model: "mistral-nemo"}})

	view := h.router.aliasesView()
	require.Len(t, view.Aliases, 1, "both definitions of the contested name were listed")
	assert.Equal(t, noderec.ModelAlias{Name: "quick-chat", Model: "qwen3:4b"}, view.Aliases[0].ModelAlias,
		"the view showed the offline local definition while the fallback routes the peer's")
	assert.Equal(t, "host-b", view.Aliases[0].OriginNode)
	assert.Equal(t, "peer-one", view.Aliases[0].OriginHost)
}

// TestRouterGetAliases_RepliesWithTheView is the desktop's read path: the
// process-scoped request answers with the same annotated list, as data.
func TestRouterGetAliases_RepliesWithTheView(t *testing.T) {
	h := routerTestCluster(t, routerEngineServes("ollama", "host-a", "llama3.2:1b"))
	h.proxy.clusterRouter = h.router
	peerAnnounceNode(t, h, "host-b", noderec.ModelAlias{Name: "quick-chat", Model: "llama3.2:1b"})

	id := json.RawMessage(`7`)
	h.proxy.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "router/get-aliases"})

	frame := replyFrame(t, h.events.String())
	require.Nil(t, frame.Error, "the alias view read failed")
	var view routerAliasesResult
	require.NoError(t, json.Unmarshal(frame.Result, &view))
	require.Len(t, view.Aliases, 1)
	assert.Equal(t, noderec.ModelAlias{Name: "quick-chat", Model: "llama3.2:1b"}, view.Aliases[0].ModelAlias)
	assert.Equal(t, "host-b", view.Aliases[0].OriginNode,
		"the reply dropped the origin, leaving the UI no way to mark foreign names")
}

// TestRouterGetAliases_RepliesWithAnEmptyListForARouterlessProcess: same
// contract as router/status — an absent endpoint is a configuration, and the
// card renders "no aliases" rather than an error on those machines.
func TestRouterGetAliases_RepliesWithAnEmptyListForARouterlessProcess(t *testing.T) {
	rec := &recRW{}
	p := NewProxy(NewCodec(rec))

	id := json.RawMessage(`8`)
	p.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: "router/get-aliases"})

	frame := replyFrame(t, rec.String())
	require.Nil(t, frame.Error, "a router-less proxy refused the alias read")
	var view routerAliasesResult
	require.NoError(t, json.Unmarshal(frame.Result, &view))
	assert.Empty(t, view.Aliases)
}
