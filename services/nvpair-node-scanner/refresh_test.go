// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// The model-inventory half of the periodic refresh loop (daemon.refreshPeersLoop) converges
// a peer's DirectoryNode.Models / ModelsByEngine to its live engine-manager
// /v1/models even when no mDNS change fires. These unit tests drive the loop's
// per-node/sweep methods directly (nil codec, since emit is nil-safe) against a
// stub /v1/models server, asserting directory + cache state and the emit
// decision (refreshNodeModels' bool return).

// modelsStub is a swappable /v1/models handler: a test can change the status and
// body between refresh calls to simulate a pull, a delete, or a transient
// failure without touching any mDNS record.
type modelsStub struct {
	mu     sync.Mutex
	status int
	body   map[string]any
}

func (s *modelsStub) set(status int, body map[string]any) {
	s.mu.Lock()
	s.status = status
	s.body = body
	s.mu.Unlock()
}

func (s *modelsStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		status, body := s.status, s.body
		s.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

// startModelsStub stands up a stub engine-manager em surface and returns it plus
// the loopback port the daemon should dial.
func startModelsStub(t *testing.T, status int, body map[string]any) (*modelsStub, int) {
	t.Helper()
	stub := &modelsStub{}
	stub.set(status, body)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	return stub, portFromURL(t, srv.URL)
}

func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse stub url %q: %v", raw, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port from %q: %v", raw, err)
	}
	return p
}

// newRefreshTestDaemon builds a daemon wired for the refresh path only: a real
// directory + modelsHTTP client and initialized caches, no codec/responder (emit
// is nil-safe and the refresh path never advertises). The registry is stamped
// with a self uuid that no peer test seeds, so refreshNodeModels dials each peer
// at its advertised address (the self-loopback branch is exercised separately in
// TestRefreshSelfDialsLoopback).
func newRefreshTestDaemon() *daemon {
	return &daemon{
		reg:                newRegistry("self-node-uuid", "", selfAddrs("127.0.0.1")),
		dir:                newDirectory(),
		modelsHTTP:         &http.Client{Timeout: modelsFetchTimeout},
		lastInfo:           make(map[string]NodeInfoResponse),
		lastInfoAt:         make(map[string]time.Time),
		lastModels:         make(map[string][]string),
		lastModelsByEngine: make(map[string]map[string][]string),
		lastLoadedByEngine: make(map[string]map[string][]string),
	}
}

// seedEMNode adds a directory entry advertising engine-manager (em) at ip:port,
// with no model inventory yet (the state before any enrichment).
func seedEMNode(d *daemon, hostUUID, ip string, emPort int) {
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: hostUUID,
		Name:     hostUUID,
		IP:       ip,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineManager: {Port: emPort},
		},
	})
}

// TestRefreshPopulatesInitialEmptyInventory covers the primary bug: a known em
// peer whose first enrichment returned nothing converges once its engine serves
// models, with no mDNS record change — a single refresh fetch populates the
// directory and reports a change.
func TestRefreshPopulatesInitialEmptyInventory(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"llama3:8b", "qwen:0.5b"},
		"modelsByEngine": map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {"qwen:0.5b"}},
	})
	seedEMNode(d, "peer-A", "127.0.0.1", port)

	if changed := d.refreshNodeModels("peer-A", "127.0.0.1", port, ""); !changed {
		t.Fatal("initial empty inventory should report a change once populated")
	}
	got, _ := d.dir.get("peer-A")
	if want := []string{"llama3:8b", "qwen:0.5b"}; !reflect.DeepEqual(got.Models, want) {
		t.Errorf("Models = %v, want %v", got.Models, want)
	}
	if want := map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {"qwen:0.5b"}}; !reflect.DeepEqual(got.ModelsByEngine, want) {
		t.Errorf("ModelsByEngine = %v, want %v", got.ModelsByEngine, want)
	}
}

func TestRefreshModelsFallsBackToASecondPublishedAddress(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models": []string{"reachable-model"},
	})
	const peerUUID = "peer-multihomed-models"
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: peerUUID,
		Name:     peerUUID,
		IP:       "127.0.0.2",
		IPs:      []string{"127.0.0.2", "127.0.0.1"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineManager: {Port: port},
		},
	})

	if !d.refreshNodeModelsCandidates(peerUUID, "127.0.0.2", []string{"127.0.0.2", "127.0.0.1"}, port, "") {
		t.Fatal("model inventory did not refresh through the second published address")
	}
	got, _ := d.dir.get(peerUUID)
	if want := []string{"reachable-model"}; !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("Models = %v, want %v", got.Models, want)
	}
}

// TestRefreshSelfDialsLoopback is the regression for the periodic model
// refresh: our OWN node must be dialed over loopback, not its advertised LAN
// ip=, so a pull/delete/late-engine-start keeps converging after startup even
// when inbound to the LAN address is firewalled (the Windows failure this
// addresses). The stub listens ONLY on loopback while the self entry advertises
// an unreachable LAN address, so the fetch can only succeed via the loopback
// dial. The stored (advertised) address is still used as applyModels' stale-
// address guard, so it must be preserved.
func TestRefreshSelfDialsLoopback(t *testing.T) {
	d := newRefreshTestDaemon() // reg self uuid = "self-node-uuid"
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"llama3:8b"},
		"modelsByEngine": map[string][]string{"ollama": {"llama3:8b"}},
	})
	// TEST-NET-1 (RFC 5737): guaranteed non-routable — only a loopback dial can
	// reach the stub, which httptest binds on 127.0.0.1:port.
	const unreachableLAN = "192.0.2.1"
	seedEMNode(d, "self-node-uuid", unreachableLAN, port)

	if changed := d.refreshNodeModels("self-node-uuid", unreachableLAN, port, ""); !changed {
		t.Fatal("self model refresh should converge via loopback despite an unreachable advertised LAN address")
	}
	got, _ := d.dir.get("self-node-uuid")
	if want := []string{"llama3:8b"}; !reflect.DeepEqual(got.Models, want) {
		t.Errorf("self Models = %v, want %v (must be dialed over loopback)", got.Models, want)
	}
	if got.IP != unreachableLAN {
		t.Errorf("self IP = %q, want the advertised address %q preserved for the stale-address guard", got.IP, unreachableLAN)
	}
}

// TestRefreshPullDeleteAndUnchanged covers three required behaviors together: a
// pull and a delete each change the inventory and emit exactly one update, while
// an unchanged inventory emits nothing (no repeated updates).
func TestRefreshPullDeleteAndUnchanged(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
	})
	seedEMNode(d, "peer-B", "127.0.0.1", port)

	// Initial populate is a change.
	if !d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Fatal("initial populate should report a change")
	}
	// Unchanged inventory: no further change events.
	if d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Error("unchanged inventory should not report a change")
	}

	// Pull: add a model -> exactly one change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a", "b"},
		"modelsByEngine": map[string][]string{"ollama": {"a", "b"}},
	})
	if !d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Fatal("a pull should report a change")
	}
	if d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Error("re-reading the same post-pull inventory should not report a change")
	}

	// Delete: remove a model -> exactly one change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
	})
	if !d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Fatal("a delete should report a change")
	}
	if d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Error("re-reading the same post-delete inventory should not report a change")
	}
	got, _ := d.dir.get("peer-B")
	if want := []string{"a"}; !reflect.DeepEqual(got.Models, want) {
		t.Errorf("final Models = %v, want %v", got.Models, want)
	}

	// Deleting the final model carries an explicit empty per-engine inventory.
	// It must clear the stale model once and then remain stable.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{},
		"modelsByEngine": map[string][]string{"ollama": {}},
	})
	if !d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Fatal("deleting the last model should report a change")
	}
	if d.refreshNodeModels("peer-B", "127.0.0.1", port, "") {
		t.Error("re-reading the same explicit empty inventory should not report a change")
	}
	got, _ = d.dir.get("peer-B")
	if len(got.Models) != 0 {
		t.Errorf("Models after last delete = %v, want empty", got.Models)
	}
	if want := map[string][]string{"ollama": {}}; !reflect.DeepEqual(got.ModelsByEngine, want) {
		t.Errorf("ModelsByEngine after last delete = %v, want %v", got.ModelsByEngine, want)
	}
}

// TestRefreshReorderOnlyIsNotAChange asserts the compare is order-insensitive: a
// response that returns the same models in a different order is not treated as a
// change (no spurious update).
func TestRefreshReorderOnlyIsNotAChange(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"a", "b"},
		"modelsByEngine": map[string][]string{"ollama": {"a", "b"}},
	})
	seedEMNode(d, "peer-R", "127.0.0.1", port)
	if !d.refreshNodeModels("peer-R", "127.0.0.1", port, "") {
		t.Fatal("initial populate should report a change")
	}
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"b", "a"},
		"modelsByEngine": map[string][]string{"ollama": {"b", "a"}},
	})
	if d.refreshNodeModels("peer-R", "127.0.0.1", port, "") {
		t.Error("a mere reordering of the same models should not report a change")
	}
}

// TestRefreshFetchFailureRetainsInventory covers requirement 5: a transient
// fetch failure preserves the last successful flat and per-engine inventories
// (no blanking) and reports no change.
func TestRefreshFetchFailureRetainsInventory(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"keep-me"},
		"modelsByEngine": map[string][]string{"ollama": {"keep-me"}},
	})
	seedEMNode(d, "peer-C", "127.0.0.1", port)
	if !d.refreshNodeModels("peer-C", "127.0.0.1", port, "") {
		t.Fatal("initial populate should report a change")
	}

	// Now the endpoint fails.
	stub.set(http.StatusInternalServerError, nil)
	if d.refreshNodeModels("peer-C", "127.0.0.1", port, "") {
		t.Error("a failed fetch should not report a change")
	}
	got, _ := d.dir.get("peer-C")
	if want := []string{"keep-me"}; !reflect.DeepEqual(got.Models, want) {
		t.Errorf("Models after failure = %v, want retained %v", got.Models, want)
	}
	if want := map[string][]string{"ollama": {"keep-me"}}; !reflect.DeepEqual(got.ModelsByEngine, want) {
		t.Errorf("ModelsByEngine after failure = %v, want retained %v", got.ModelsByEngine, want)
	}
	d.infoMu.Lock()
	cachedFlat := d.lastModels["peer-C"]
	cachedByEngine := d.lastModelsByEngine["peer-C"]
	d.infoMu.Unlock()
	if want := []string{"keep-me"}; !reflect.DeepEqual(cachedFlat, want) {
		t.Errorf("cached flat inventory = %v, want retained %v", cachedFlat, want)
	}
	if want := map[string][]string{"ollama": {"keep-me"}}; !reflect.DeepEqual(cachedByEngine, want) {
		t.Errorf("cached per-engine inventory = %v, want retained %v", cachedByEngine, want)
	}
}

// TestRefreshPartialModelsByEngine covers requirement 4: a partial
// modelsByEngine response (attribution covering fewer models than the flat
// union, as a mixed-version peer might send) is propagated exactly as returned.
func TestRefreshPartialModelsByEngine(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"x", "y"},
		"modelsByEngine": map[string][]string{"ollama": {"x"}},
	})
	seedEMNode(d, "peer-D", "127.0.0.1", port)
	if !d.refreshNodeModels("peer-D", "127.0.0.1", port, "") {
		t.Fatal("initial populate should report a change")
	}
	got, _ := d.dir.get("peer-D")
	if want := []string{"x", "y"}; !reflect.DeepEqual(got.Models, want) {
		t.Errorf("Models = %v, want %v", got.Models, want)
	}
	if want := map[string][]string{"ollama": {"x"}}; !reflect.DeepEqual(got.ModelsByEngine, want) {
		t.Errorf("ModelsByEngine = %v, want the partial map propagated verbatim %v", got.ModelsByEngine, want)
	}
}

// TestRefreshPopulatesLoadedByEngine covers Phase 2: a peer's loadedByEngine is
// carried onto the directory node from the same /v1/models fetch.
func TestRefreshPopulatesLoadedByEngine(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"llama3:8b", "qwen:0.5b"},
		"modelsByEngine": map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {"qwen:0.5b"}},
		"loadedByEngine": map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {}},
	})
	seedEMNode(d, "peer-L", "127.0.0.1", port)

	if !d.refreshNodeModels("peer-L", "127.0.0.1", port, "") {
		t.Fatal("initial populate should report a change")
	}
	got, _ := d.dir.get("peer-L")
	if want := map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {}}; !reflect.DeepEqual(got.LoadedByEngine, want) {
		t.Errorf("LoadedByEngine = %v, want %v", got.LoadedByEngine, want)
	}
}

// TestRefreshLoadedOnlyChangeReportsChange covers the loaded-set reconcile: when
// only loadedByEngine changes (a JIT load / TTL eviction with the same installed
// list), the refresh reports a change and updates the field so remote cards stay
// live.
func TestRefreshLoadedOnlyChangeReportsChange(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
		"loadedByEngine": map[string][]string{"ollama": {}},
	})
	seedEMNode(d, "peer-M", "127.0.0.1", port)
	if !d.refreshNodeModels("peer-M", "127.0.0.1", port, "") {
		t.Fatal("initial populate should report a change")
	}

	// Same installed list, but the model is now resident -> a change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
		"loadedByEngine": map[string][]string{"ollama": {"a"}},
	})
	if !d.refreshNodeModels("peer-M", "127.0.0.1", port, "") {
		t.Fatal("a loaded-set change should report a change even when the model list is unchanged")
	}
	got, _ := d.dir.get("peer-M")
	if want := map[string][]string{"ollama": {"a"}}; !reflect.DeepEqual(got.LoadedByEngine, want) {
		t.Errorf("LoadedByEngine after load = %v, want %v", got.LoadedByEngine, want)
	}

	// A mere reordering of the same loaded set is not a change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
		"loadedByEngine": map[string][]string{"ollama": {"a"}},
	})
	if d.refreshNodeModels("peer-M", "127.0.0.1", port, "") {
		t.Error("an identical loaded set should not report a change")
	}
}

// TestRefreshLoadedRetainedOnFailure confirms a transient fetch failure preserves
// the last successful loadedByEngine rather than blanking it.
func TestRefreshLoadedRetainedOnFailure(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"keep-me"},
		"modelsByEngine": map[string][]string{"ollama": {"keep-me"}},
		"loadedByEngine": map[string][]string{"ollama": {"keep-me"}},
	})
	seedEMNode(d, "peer-N", "127.0.0.1", port)
	if !d.refreshNodeModels("peer-N", "127.0.0.1", port, "") {
		t.Fatal("initial populate should report a change")
	}

	stub.set(http.StatusInternalServerError, nil)
	if d.refreshNodeModels("peer-N", "127.0.0.1", port, "") {
		t.Error("a failed fetch should not report a change")
	}
	got, _ := d.dir.get("peer-N")
	if want := map[string][]string{"ollama": {"keep-me"}}; !reflect.DeepEqual(got.LoadedByEngine, want) {
		t.Errorf("LoadedByEngine after failure = %v, want retained %v", got.LoadedByEngine, want)
	}
	d.infoMu.Lock()
	cachedLoaded := d.lastLoadedByEngine["peer-N"]
	d.infoMu.Unlock()
	if want := map[string][]string{"ollama": {"keep-me"}}; !reflect.DeepEqual(cachedLoaded, want) {
		t.Errorf("cached loaded inventory = %v, want retained %v", cachedLoaded, want)
	}
}

// TestApplyModelsGuard covers requirement 6 at the directory layer: a completed
// fetch is discarded (ok == false) when the node was removed or re-addressed
// while it was in flight, so it can neither resurrect a removed node nor
// overwrite a re-addressed one.
func TestApplyModelsGuard(t *testing.T) {
	d := newDirectory()

	// Removed mid-sweep: node absent -> not resurrected.
	if _, changed, ok := d.applyModels("ghost", "127.0.0.1", 14322, []string{"a"}, nil, nil, nil); ok || changed {
		t.Errorf("applyModels on an absent node = (changed %v, ok %v), want (false, false)", changed, ok)
	}
	if _, present := d.get("ghost"); present {
		t.Error("applyModels must not resurrect a removed node")
	}

	// Re-addressed mid-sweep: node present but IP changed -> not overwritten.
	d.upsert(noderec.DirectoryNode{
		HostUUID: "peer-E",
		IP:       "10.0.0.9",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineManager: {Port: 14322}},
		Models:   []string{"old"},
	})
	if _, changed, ok := d.applyModels("peer-E", "127.0.0.1", 14322, []string{"new"}, nil, nil, nil); ok || changed {
		t.Errorf("applyModels with a stale IP = (changed %v, ok %v), want (false, false)", changed, ok)
	}
	// em port changed -> also discarded.
	if _, changed, ok := d.applyModels("peer-E", "10.0.0.9", 99999, []string{"new"}, nil, nil, nil); ok || changed {
		t.Errorf("applyModels with a stale em port = (changed %v, ok %v), want (false, false)", changed, ok)
	}
	got, _ := d.get("peer-E")
	if want := []string{"old"}; !reflect.DeepEqual(got.Models, want) {
		t.Errorf("Models after guarded rejects = %v, want unchanged %v", got.Models, want)
	}
}

// TestRefreshNodeModelsRemovedMidSweep exercises the guard through the daemon's
// per-node path: a node evicted between the sweep snapshot and the fetch's
// completion is not resurrected in the directory (the requirement-6 guarantee).
// The cache may hold the fetched value (it is written before the directory
// guard, as last-known-good), so this asserts only the directory invariant.
func TestRefreshNodeModelsRemovedMidSweep(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models": []string{"a"},
	})
	// The node is not in the directory (simulating removal before the in-flight
	// fetch completes); the fetch succeeds but must not be applied to the
	// directory.
	if d.refreshNodeModels("gone", "127.0.0.1", port, "") {
		t.Error("a fetch for a removed node should report no change")
	}
	if _, present := d.dir.get("gone"); present {
		t.Error("an in-flight result must not resurrect a removed node")
	}
}

// TestRefreshOnceConcurrentSlowPeers covers requirement 7: multiple slow peers
// are refreshed concurrently, so one slow peer does not consume another's
// timeout budget. It proves concurrency deterministically with an entry barrier
// rather than a wall-clock threshold: both stub handlers must be ENTERED before
// either is released. A serial sweep would block inside the first handler and
// never enter the second, so the barrier wait fails fast. The time.After bounds
// are failure ceilings, not success-timing assertions, so the test isn't flaky.
func TestRefreshOnceConcurrentSlowPeers(t *testing.T) {
	const peers = 2 // must stay <= modelsRefreshConcurrency
	entered := make(chan struct{}, peers)
	release := make(chan struct{})
	handler := func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release // hold until both peers are concurrently in-flight
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []string{"m"}})
	}
	s1 := httptest.NewServer(http.HandlerFunc(handler))
	s2 := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(s1.Close)
	t.Cleanup(s2.Close)

	d := newRefreshTestDaemon()
	seedEMNode(d, "peer-1", "127.0.0.1", portFromURL(t, s1.URL))
	seedEMNode(d, "peer-2", "127.0.0.1", portFromURL(t, s2.URL))

	done := make(chan struct{})
	go func() { d.refreshModelsOnce(); close(done) }()

	// Both handlers must be entered before either is released; only a concurrent
	// sweep can reach this point.
	for i := 0; i < peers; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d peers entered concurrently; sweep is not concurrent", i, peers)
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refreshModelsOnce did not complete after releasing peers")
	}
	if got, _ := d.dir.get("peer-1"); !reflect.DeepEqual(got.Models, []string{"m"}) {
		t.Errorf("peer-1 Models = %v, want [m]", got.Models)
	}
	if got, _ := d.dir.get("peer-2"); !reflect.DeepEqual(got.Models, []string{"m"}) {
		t.Errorf("peer-2 Models = %v, want [m]", got.Models)
	}
}
