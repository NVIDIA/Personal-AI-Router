// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func (s *modelsStub) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
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
			assert.NoError(t, json.NewEncoder(w).Encode(body))
		}
	}
}

// startModelsStub stands up a stub engine-manager em surface and returns it plus
// the loopback port the daemon should dial.
func startModelsStub(t *testing.T, status int, body map[string]any) (*modelsStub, int) {
	t.Helper()
	stub := &modelsStub{}
	stub.set(status, body)
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)
	return stub, portFromURL(t, srv.URL)
}

func portFromURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err, "parse stub url (%v, %v)", raw, err)
	p, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "stub port from (%v, %v)", raw, err)
	return p
}

// newRefreshTestDaemon initializes model reconciliation without announcements or
// IPC. Its loopback fixtures exercise inventory changes; authenticated remote
// behavior uses newDiscoveryPeerFixture.
func newRefreshTestDaemon() *daemon {
	return &daemon{
		reg:        newRegistry("self-node-uuid", "", selfAddrs("127.0.0.1")),
		dir:        newDirectory(),
		modelsHTTP: &http.Client{Timeout: modelsFetchTimeout},
		lastInfoAt: make(map[string]time.Time),
	}
}

// seedEMNode adds and returns an engine-manager refresh target with no inventory.
func seedEMNode(d *daemon, hostUUID, ip string, emPort int) noderec.DirectoryNode {
	target := noderec.DirectoryNode{
		HostUUID: hostUUID,
		Name:     hostUUID,
		IP:       ip,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineManager: {Port: emPort},
		},
	}
	d.dir.upsert(target)
	return target
}

// Model inventory converges without requiring a changed discovery announcement.
func TestRefresh_PopulatesInitialEmptyInventory(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"llama3:8b", "qwen:0.5b"},
		"modelsByEngine": map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {"qwen:0.5b"}},
	})
	target := seedEMNode(d, "peer-A", "127.0.0.1", port)

	require.True(t, d.refreshNodeModels(target), "initial empty inventory should report a change once populated")
	got, _ := d.dir.get("peer-A")
	assert.Equal(t, []string{"llama3:8b", "qwen:0.5b"}, got.Models, "Models")
	assert.Equal(t, map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {"qwen:0.5b"}}, got.ModelsByEngine, "ModelsByEngine")
}

func TestRefreshModels_FallsBackToASecondPublishedAddress(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models": []string{"reachable-model"},
	})
	const peerUUID = "peer-multihomed-models"
	target := noderec.DirectoryNode{
		HostUUID: peerUUID,
		Name:     peerUUID,
		IP:       "127.0.0.2",
		IPs:      []string{"127.0.0.2", "127.0.0.1"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineManager: {Port: port},
		},
	}
	d.dir.upsert(target)

	require.True(t, d.refreshNodeModels(target), "model inventory did not refresh through the second published address")
	got, _ := d.dir.get(peerUUID)
	require.Equal(t, []string{"reachable-model"}, got.Models, "Models")
}

// The owned engine-manager listens on loopback while the directory advertises
// an unreachable LAN address. A local refresh must preserve that advertised IP.
func TestRefreshSelf_DialsLoopback(t *testing.T) {
	d := newRefreshTestDaemon() // reg self uuid = "self-node-uuid"
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"llama3:8b"},
		"modelsByEngine": map[string][]string{"ollama": {"llama3:8b"}},
	})
	// TEST-NET-1 (RFC 5737): guaranteed non-routable — only a loopback dial can
	// reach the stub, which httptest binds on 127.0.0.1:port.
	const unreachableLAN = "192.0.2.1"
	target := seedEMNode(d, "self-node-uuid", unreachableLAN, port)

	require.True(t, d.refreshNodeModels(target), "self model refresh should converge via loopback despite an unreachable advertised LAN address")
	got, _ := d.dir.get("self-node-uuid")
	assert.Equal(t, []string{"llama3:8b"}, got.Models, "self Models")
	assert.Equal(t, unreachableLAN, got.IP, "self IP (%v)", unreachableLAN)
}

// Publishing local services must not wait for an engine-manager read before
// owned hardware notifications and subsequent IPC can proceed. The publication
// must retain flat, per-engine, and loaded model inventories together.
func TestPublishSelf_UsesCachedModelsWithoutWaitingForHTTP(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.0.2.1")
	blockReads := make(chan struct{})
	readStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(release) })
	defer releaseRead()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-blockReads:
			readStarted <- struct{}{}
			<-release
		default:
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"models":         []string{"retained-model"},
			"modelsByEngine": map[string][]string{"ollama": {"retained-model"}},
			"loadedByEngine": map[string][]string{"ollama": {"retained-model"}, "lmstudio": {}},
		}))
	}))
	t.Cleanup(server.Close)
	port := portFromURL(t, server.URL)
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceEngineManager, Port: port})
	d.publishSelf()
	target, present := d.dir.get("self-uuid")
	require.True(t, present)
	require.True(t, d.refreshNodeModels(target))
	select {
	case <-d.modelsWake:
	default:
	}
	close(blockReads)

	published := make(chan struct{})
	go func() {
		d.publishSelf()
		close(published)
	}()
	select {
	case <-published:
	case <-readStarted:
		t.Error("local publication started an HTTP read instead of using cached models")
		releaseRead()
		<-published
	case <-time.After(5 * time.Second):
		releaseRead()
		t.Fatal("local publication did not complete")
	}

	node, present := d.dir.get("self-uuid")
	require.True(t, present)
	assert.Equal(t, []string{"retained-model"}, node.Models)
	assert.Equal(t, map[string][]string{"ollama": {"retained-model"}}, node.ModelsByEngine)
	assert.Equal(t, map[string][]string{"ollama": {"retained-model"}, "lmstudio": {}}, node.LoadedByEngine)
	select {
	case <-d.modelsWake:
	default:
		t.Error("local publication must schedule model refresh")
	}
}

// Downloads and deletions each change inventory once; unchanged reads stay quiet.
func TestRefreshModels_ReportsDownloadsAndDeletionsButNotUnchangedInventory(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
	})
	target := seedEMNode(d, "peer-B", "127.0.0.1", port)

	// Initial populate is a change.
	require.True(t, d.refreshNodeModels(target), "initial populate should report a change")
	// Unchanged inventory: no further change events.
	assert.False(t, d.refreshNodeModels(target), "unchanged inventory should not report a change")

	// Pull: add a model -> exactly one change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a", "b"},
		"modelsByEngine": map[string][]string{"ollama": {"a", "b"}},
	})
	require.True(t, d.refreshNodeModels(target), "a pull should report a change")
	assert.False(t, d.refreshNodeModels(target), "re-reading the same post-pull inventory should not report a change")

	// Delete: remove a model -> exactly one change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
	})
	require.True(t, d.refreshNodeModels(target), "a delete should report a change")
	assert.False(t, d.refreshNodeModels(target), "re-reading the same post-delete inventory should not report a change")
	got, _ := d.dir.get("peer-B")
	assert.Equal(t, []string{"a"}, got.Models, "final Models")

	// Deleting the final model carries an explicit empty per-engine inventory.
	// It must clear the stale model once and then remain stable.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{},
		"modelsByEngine": map[string][]string{"ollama": {}},
	})
	require.True(t, d.refreshNodeModels(target), "deleting the last model should report a change")
	assert.False(t, d.refreshNodeModels(target), "re-reading the same explicit empty inventory should not report a change")
	got, _ = d.dir.get("peer-B")
	assert.Empty(t, got.Models, "Models after last delete")
	assert.Equal(t, map[string][]string{"ollama": {}}, got.ModelsByEngine, "ModelsByEngine after last delete")
}

// Reordering the same installed models must not emit another directory update.
func TestRefreshReorderOnly_IsNotAChange(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"a", "b"},
		"modelsByEngine": map[string][]string{"ollama": {"a", "b"}},
	})
	target := seedEMNode(d, "peer-R", "127.0.0.1", port)
	require.True(t, d.refreshNodeModels(target), "initial populate should report a change")
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"b", "a"},
		"modelsByEngine": map[string][]string{"ollama": {"b", "a"}},
	})
	assert.False(t, d.refreshNodeModels(target), "a mere reordering of the same models should not report a change")
}

// A failed refresh preserves both flat and per-engine inventories.
func TestRefreshFetchFailure_RetainsInventory(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"keep-me"},
		"modelsByEngine": map[string][]string{"ollama": {"keep-me"}},
	})
	target := seedEMNode(d, "peer-C", "127.0.0.1", port)
	require.True(t, d.refreshNodeModels(target), "initial populate should report a change")

	// Now the endpoint fails.
	stub.set(http.StatusInternalServerError, nil)
	assert.False(t, d.refreshNodeModels(target), "a failed fetch should not report a change")
	got, _ := d.dir.get("peer-C")
	assert.Equal(t, []string{"keep-me"}, got.Models, "Models after failure")
	assert.Equal(t, map[string][]string{"ollama": {"keep-me"}}, got.ModelsByEngine, "ModelsByEngine after failure")
}

// A peer may attribute fewer models to engines than it includes in the union.
func TestRefreshModels_PreservesInventoryForUnreportedEngines(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"x", "y"},
		"modelsByEngine": map[string][]string{"ollama": {"x"}},
	})
	target := seedEMNode(d, "peer-D", "127.0.0.1", port)
	require.True(t, d.refreshNodeModels(target), "initial populate should report a change")
	got, _ := d.dir.get("peer-D")
	assert.Equal(t, []string{"x", "y"}, got.Models, "Models")
	assert.Equal(t, map[string][]string{"ollama": {"x"}}, got.ModelsByEngine, "ModelsByEngine")
}

// The same model fetch supplies the peer's installed and loaded inventories.
func TestRefresh_PopulatesLoadedByEngine(t *testing.T) {
	d := newRefreshTestDaemon()
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"llama3:8b", "qwen:0.5b"},
		"modelsByEngine": map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {"qwen:0.5b"}},
		"loadedByEngine": map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {}},
	})
	target := seedEMNode(d, "peer-L", "127.0.0.1", port)

	require.True(t, d.refreshNodeModels(target), "initial populate should report a change")
	got, _ := d.dir.get("peer-L")
	assert.Equal(t, map[string][]string{"ollama": {"llama3:8b"}, "lmstudio": {}}, got.LoadedByEngine, "LoadedByEngine")
}

// Loading a model changes the directory even when installed models stay the same.
func TestRefreshLoadedOnlyChange_ReportsChange(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
		"loadedByEngine": map[string][]string{"ollama": {}},
	})
	target := seedEMNode(d, "peer-M", "127.0.0.1", port)
	require.True(t, d.refreshNodeModels(target), "initial populate should report a change")

	// Same installed list, but the model is now resident -> a change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
		"loadedByEngine": map[string][]string{"ollama": {"a"}},
	})
	require.True(t, d.refreshNodeModels(target), "a loaded-set change should report a change even when the model list is unchanged")
	got, _ := d.dir.get("peer-M")
	assert.Equal(t, map[string][]string{"ollama": {"a"}}, got.LoadedByEngine, "LoadedByEngine after load")

	// A mere reordering of the same loaded set is not a change.
	stub.set(http.StatusOK, map[string]any{
		"models":         []string{"a"},
		"modelsByEngine": map[string][]string{"ollama": {"a"}},
		"loadedByEngine": map[string][]string{"ollama": {"a"}},
	})
	assert.False(t, d.refreshNodeModels(target), "an identical loaded set should not report a change")
}

// A failed refresh must not clear the last accepted loaded-model inventory.
func TestRefreshModels_RetainsLoadedModelsAfterFetchFailure(t *testing.T) {
	d := newRefreshTestDaemon()
	stub, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"keep-me"},
		"modelsByEngine": map[string][]string{"ollama": {"keep-me"}},
		"loadedByEngine": map[string][]string{"ollama": {"keep-me"}},
	})
	target := seedEMNode(d, "peer-N", "127.0.0.1", port)
	require.True(t, d.refreshNodeModels(target), "initial populate should report a change")

	stub.set(http.StatusInternalServerError, nil)
	assert.False(t, d.refreshNodeModels(target), "a failed fetch should not report a change")
	got, _ := d.dir.get("peer-N")
	assert.Equal(t, map[string][]string{"ollama": {"keep-me"}}, got.LoadedByEngine, "LoadedByEngine after failure")
}

// A missing node must stay absent when a periodic sweep reaches its old target.
func TestRefreshNodeModels_DoesNotRestoreNodeRemovedDuringSweep(t *testing.T) {
	d := newRefreshTestDaemon()
	target := seedEMNode(d, "gone", "127.0.0.1", 14322)
	require.True(t, d.dir.remove(target.HostUUID))
	// The sweep's target was removed before its fetch began. The directory
	// lookup rejects it before any request is made.
	assert.False(t, d.refreshNodeModels(target), "a refresh for a removed node should report no change")
	_, present := d.dir.get("gone")
	assert.False(t, present, "a periodic refresh must not restore a removed node")
}

// Both peer handlers must be entered before either is released. This proves
// concurrency without requiring successful requests to meet a timing threshold.
func TestRefreshModels_ConcurrentAuthenticatedPeers(t *testing.T) {
	first := newDiscoveryPeerFixture(t, "peer-1")
	first.discover(t)
	second := newDiscoveryPeerFixture(t, "peer-2")
	second.attach(t, first.daemon)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	for _, f := range []*discoveryPeerFixture{first, second} {
		f.info.Peer.Services[noderec.ServiceEngineManager] = portFromURL(t, f.server.URL)
		f.daemon.refreshTelemetryOnce(context.Background())
		f.respond = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/v1/models" {
				return false
			}
			entered <- struct{}{}
			<-release
			inventory := modelInventoryResponse{Models: []string{"m"}}
			assert.NoError(t, json.NewEncoder(w).Encode(inventory))
			return true
		}
	}
	done := make(chan struct{})
	go func() { first.daemon.refreshModelsOnce(); close(done) }()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			require.FailNow(t, "models did not refresh peers concurrently")
		}
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "models refresh did not finish")
	}
	for _, uuid := range []string{"peer-1", "peer-2"} {
		node, _ := first.daemon.dir.get(uuid)
		assert.Equal(t, []string{"m"}, node.Models, "peer %s", uuid)
	}
}

type modelInventoryResponse struct {
	Models []string `json:"models"`
}

func TestApplyModels_DoesNotRestoreRemovedNode(t *testing.T) {
	d := newDirectory()

	_, changed, ok := d.applyModels("ghost", "127.0.0.1", 14322, []string{"a"}, nil, nil)
	assert.False(t, ok)
	assert.False(t, changed)
	_, present := d.get("ghost")
	assert.False(t, present)
}

func TestApplyModels_RejectsChangedEndpoint(t *testing.T) {
	test := func(name, ip string, port int) {
		t.Run(name, func(t *testing.T) {
			d := newDirectory()
			d.upsert(noderec.DirectoryNode{
				HostUUID: "peer-E",
				IP:       "10.0.0.9",
				Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineManager: {Port: 14322}},
				Models:   []string{"old"},
			})

			_, changed, ok := d.applyModels("peer-E", ip, port, []string{"new"}, nil, nil)

			assert.False(t, ok)
			assert.False(t, changed)
			node, present := d.get("peer-E")
			require.True(t, present)
			assert.Equal(t, []string{"old"}, node.Models)
		})
	}
	test("address changes", "127.0.0.1", 14322)
	test("port changes", "10.0.0.9", 14323)
}
