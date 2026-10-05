// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// selfAddrs turns one test address into the ranked candidate list the registry
// holds, mapping "" to "this host has no publishable address".
func selfAddrs(ip string) []string {
	if ip == "" {
		return nil
	}
	return []string{ip}
}

// newSelfTestDaemon builds a daemon wired for the registry-driven self path: a
// registry stamped with hostUUID/ip, a real directory, a disabled mesh, and
// initialized enrichment caches + HTTP clients. No codec/responder (emit is
// nil-safe; publishSelf skips re-advertise when the responder is nil).
func newSelfTestDaemon(hostUUID, ip string) *daemon {
	return &daemon{
		mesh:               clustertrust.Open(""),
		reg:                newRegistry(hostUUID, "", selfAddrs(ip)),
		instance:           "myhost",
		dir:                newDirectory(),
		http:               &http.Client{Timeout: nodeInfoFetchTimeout},
		modelsHTTP:         &http.Client{Timeout: modelsFetchTimeout},
		lastInfo:           make(map[string]NodeInfoResponse),
		lastInfoAt:         make(map[string]time.Time),
		lastModels:         make(map[string][]string),
		lastModelsByEngine: make(map[string]map[string][]string),
		lastLoadedByEngine: make(map[string]map[string][]string),
	}
}

// TestReloadIdentityAdoptsClusterPrincipal covers the fresh-host uuid= re-stamp:
// the scanner spawns first and mints its own node-id, then cluster-manager
// writes identity.json; reloadIdentity must re-resolve and adopt that principal
// so the advertised uuid= converges (no lingering scanner-minted id) — AND move
// this node's own directory entry to the new uuid so the stale one isn't served
// to peers as a ghost.
func TestReloadIdentityAdoptsClusterPrincipal(t *testing.T) {
	base := t.TempDir()
	d := &daemon{
		mesh:               clustertrust.Open(""), // unclustered: no cluster dir -> no clusterUuid
		reg:                newRegistry("scanner-minted-X", "", selfAddrs("127.0.0.1")),
		dir:                newDirectory(),
		baseDir:            base,
		lastInfo:           make(map[string]NodeInfoResponse),
		lastInfoAt:         make(map[string]time.Time),
		lastModels:         make(map[string][]string),
		lastModelsByEngine: make(map[string]map[string][]string),
		lastLoadedByEngine: make(map[string]map[string][]string),
		// codec/responder nil: emit is nil-safe; reloadIdentity skips re-advertise.
	}
	// Simulate this node already having been browsed under its scanner-minted uuid.
	d.dir.upsert(noderec.DirectoryNode{HostUUID: "scanner-minted-X", Name: "host", IP: "127.0.0.1"})

	// cluster-manager writes its principal under <base>/cluster/identity.json.
	idPath := filepath.Join(base, "cluster", "identity.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(idPath), 0o700), "mkdir")
	body, _ := json.Marshal(map[string]any{"node_uuid": "cluster-principal-Y", "created_at": 1})
	require.NoError(t, os.WriteFile(idPath, body, 0o600), "write identity.json")

	d.reloadIdentity()

	{
		got := d.reg.record().HostUUID
		require.Equal(t, "cluster-principal-Y", got, "reloadIdentity hostUuid")
	}
	// Inbound reconciliation: the directory entry moved to the new uuid and the
	// old one is gone (no ghost).
	{
		_, ok := d.dir.get("cluster-principal-Y")
		assert.True(t, ok, "directory missing the re-stamped hostUuid after reloadIdentity")
	}
	{
		_, ok := d.dir.get("scanner-minted-X")
		assert.False(t, ok, "stale scanner-minted hostUuid left in the directory (ghost)")
	}
}

// TestSelfNotEvictedByBrowse is the regression: the local node must
// never be removed from its own directory by a browse event. On Windows the host
// frequently stops looping its own multicast back, so the browser ages self like
// any peer and (its LAN-IP liveness probe failing behind a first-run firewall)
// evicts it. With self registry-driven and ignored in onBrowse, a removed/
// discovered/updated browse event for our own uuid is a no-op on the directory.
func TestSelfNotEvictedByBrowse(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318})
	d.publishSelf()
	{
		_, ok := d.dir.get("self-uuid")
		require.True(t, ok, "publishSelf should put the local node in the directory")
	}

	// Its own record aged out of the browse (Windows self-multicast loss): the
	// browser emits removed for our uuid. It must NOT drop the self entry.
	selfTXT := []string{"v=1", "uuid=self-uuid", "ip=192.168.1.17", "ni=14318"}
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: RawNode{ID: "myhost", TXT: selfTXT}})
	{
		_, ok := d.dir.get("self-uuid")
		assert.True(t, ok, "a browse 'removed' for our own uuid must not evict the local node")
	}

	// A later self re-appearance in the browse is also a no-op (self stays
	// registry-driven; the browse never clobbers it).
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "myhost", TXT: selfTXT}})
	if n, ok := d.dir.get("self-uuid"); !ok {
		assert.Fail(t, "test expectation failed", "self entry disappeared after a self 'discovered' browse event")
	} else {
		assert.Equal(t, "myhost", n.Name, "self entry Name")
	}
}

// TestPublishSelfEnrichesOverLoopback covers the "no metrics" failure mode:
// self enrichment must dial loopback, not the advertised LAN ip=, so a firewall
// block on inbound to our own LAN address can't blank the local card. The stub
// listens on loopback while the node advertises an (unreachable-in-test) LAN IP.
func TestPublishSelfEnrichesOverLoopback(t *testing.T) {
	want := NodeInfoResponse{
		GPUs:   []GPUInfo{{Name: "NVIDIA GeForce RTX 5090"}},
		CPU:    &CPUInfo{Name: "AMD Ryzen 7 9800X3D", Cores: 8},
		Memory: &MemoryInfo{TotalBytes: 68719476736},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err, "split stub addr")
	port, _ := strconv.Atoi(portStr)

	// The node advertises the LAN address (which no server answers on in the
	// test); enrichment must still succeed via loopback, where the stub listens.
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: port})
	d.publishSelf()

	n, ok := d.dir.get("self-uuid")
	require.True(t, ok, "self missing from directory")
	assert.Equal(t, "192.168.1.17", n.IP, "self display IP")
	require.Len(t, n.GPUs, 1, "self GPUs")
	assert.Equal(t, "NVIDIA GeForce RTX 5090", n.GPUs[0].Name, "self GPUs")
	require.NotNil(t, n.CPU, "self CPU")
	assert.Equal(t, "AMD Ryzen 7 9800X3D", n.CPU.Name, "self CPU")
	require.NotNil(t, n.Memory, "self Memory")
	assert.Equal(t, uint64(68719476736), n.Memory.TotalBytes, "self Memory")
}

// TestPublishSelfLoopbackIPFallback covers the "IP unknown at startup" case: when
// the LAN ranker hasn't picked an address, self still gets a dialable host
// (loopback) so the local card is never address-less.
func TestPublishSelfLoopbackIPFallback(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "") // BestLocalIP() empty
	d.publishSelf()
	n, ok := d.dir.get("self-uuid")
	require.True(t, ok, "self missing from directory")
	assert.True(t, n.IP == loopbackHost, "self IP with no LAN address (%v)", loopbackHost)
}

// TestPeerStillAgesOut confirms the fix is scoped to self: a genuine remote peer
// is still added on discovery and removed on a browse 'removed' event. The peer
// advertises no services so onBrowse does no enrichment dial.
func TestPeerStillAgesOut(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	peerTXT := []string{"v=1", "uuid=peer-uuid", "ip=192.168.1.99"}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "peer", TXT: peerTXT}})
	{
		_, ok := d.dir.get("peer-uuid")
		require.True(t, ok, "a discovered peer should be added to the directory")
	}
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: RawNode{ID: "peer", TXT: peerTXT}})
	{
		_, ok := d.dir.get("peer-uuid")
		assert.False(t, ok, "a genuine remote peer must still age out when it leaves")
	}
}

// TestReachableAntiFlap covers the daemon's liveness probe: prefer fresh
// lastInfo, else TCP to node-info / engine-manager, then identity when
// conclusive. Never treat ol/lm alone as proof of life.
func TestReachableAntiFlap(t *testing.T) {
	niSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer niSrv.Close()
	niURL, err := url.Parse(niSrv.URL)
	require.NoError(t, err, "parse ni url")
	niPort, err := strconv.Atoi(niURL.Port())
	require.NoError(t, err, "ni port")

	emLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "em listen")
	defer emLn.Close()
	emPort := emLn.Addr().(*net.TCPAddr).Port

	olLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "ol listen")
	defer olLn.Close()
	olPort := olLn.Addr().(*net.TCPAddr).Port

	dl, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "dead listen")
	deadPort := dl.Addr().(*net.TCPAddr).Port
	dl.Close()

	d := &daemon{
		lastInfo:   make(map[string]NodeInfoResponse),
		lastInfoAt: make(map[string]time.Time),
	}

	liveNI := RawNode{TXT: []string{"v=1", "uuid=host-a", "ip=127.0.0.1", fmt.Sprintf("ni=%d", niPort)}}
	assert.True(t, d.reachable(liveNI), "node with live node-info should be reachable")

	// The node's first-ranked address can be unreachable from this observer even
	// when a later published address reaches the same live service. A transient
	// mDNS miss must not evict that live multi-homed node.
	liveNIOnSecondAddress := RawNode{TXT: []string{
		"v=1", "uuid=host-multihomed", "ip=127.0.0.2",
		"ips=127.0.0.2,127.0.0.1", fmt.Sprintf("ni=%d", niPort),
	}}
	assert.True(t, d.reachable(liveNIOnSecondAddress), "node reachable at its second published address should stay alive")

	deadNI := RawNode{TXT: []string{"v=1", "ip=127.0.0.1", fmt.Sprintf("ni=%d", deadPort)}}
	assert.False(t, d.reachable(deadNI), "node whose only ni port is closed should be unreachable")

	// Live ol alone must not keep the node (inference proxy is not a liveness signal).
	olOnly := RawNode{TXT: []string{"v=1", "ip=127.0.0.1", fmt.Sprintf("ol=%d", olPort)}}
	assert.False(t, d.reachable(olOnly), "node with only a live ol port must not be reachable")

	// Dead ni + live ol: still unreachable — ol is ignored.
	mixed := RawNode{TXT: []string{"v=1", "ip=127.0.0.1", fmt.Sprintf("ni=%d", deadPort), fmt.Sprintf("ol=%d", olPort)}}
	assert.False(t, d.reachable(mixed), "dead ni with live ol must not be reachable")

	// em fallback when ni is absent.
	emOnly := RawNode{TXT: []string{"v=1", "ip=127.0.0.1", fmt.Sprintf("em=%d", emPort)}}
	assert.True(t, d.reachable(emOnly), "node with live em and no ni should be reachable")

	// Fresh lastInfo skips dialing.
	d.lastInfo["host-cached"] = NodeInfoResponse{}
	d.lastInfoAt["host-cached"] = time.Now()
	cached := RawNode{TXT: []string{"v=1", "uuid=host-cached", "ip=127.0.0.1", fmt.Sprintf("ni=%d", deadPort)}}
	assert.True(t, d.reachable(cached), "fresh lastInfo should keep the node without a successful probe")

	idOnly := RawNode{TXT: []string{"v=1", "ip=127.0.0.1"}}
	assert.False(t, d.reachable(idOnly), "identity-only node (no ni/em) should be unreachable")

	noIP := RawNode{TXT: []string{"v=1", fmt.Sprintf("ni=%d", niPort)}}
	assert.False(t, d.reachable(noIP), "node with no resolvable address should be unreachable")
}

func TestEnrichFallsBackToASecondPublishedAddress(t *testing.T) {
	const hostUUID = "host-enrich-failover"
	niSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{
			HostUUID: hostUUID,
			GPUs:     []GPUInfo{{Name: "GPU from reachable address"}},
		})
	}))
	defer niSrv.Close()
	niURL, err := url.Parse(niSrv.URL)
	require.NoError(t, err, "parse node-info URL")
	niPort, err := strconv.Atoi(niURL.Port())
	require.NoError(t, err, "parse node-info port")

	d := &daemon{
		http:         niSrv.Client(),
		lastInfo:     make(map[string]NodeInfoResponse),
		lastInfoAt:   make(map[string]time.Time),
		nodeInfoDown: make(map[string]bool),
	}
	node := noderec.DirectoryNode{
		HostUUID: hostUUID,
		IP:       "127.0.0.2",
		IPs:      []string{"127.0.0.2", "127.0.0.1"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: niPort},
		},
	}
	d.enrich(&node)
	require.Len(t, node.GPUs, 1)
	require.Equal(t, "GPU from reachable address", node.GPUs[0].Name)
}

// TestReachableSelfDialsLoopback pins the uuid-keyed self probe: when the
// threshold-missed record is this host, reachable dials loopbackHost rather than
// the advertised LAN ip=. A hostname-keyed browse filter is not required — and
// would wrongly drop a peer that merely shares our instance name.
func TestReachableSelfDialsLoopback(t *testing.T) {
	const selfUUID = "self-uuid"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: selfUUID})
	}))
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err, "split stub addr")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err, "stub port")

	d := newSelfTestDaemon(selfUUID, "192.0.2.1")
	// Advertise an unroutable DOCUMENTATION address so a LAN dial cannot succeed;
	// the stub only listens on loopback.
	self := RawNode{
		ID:  "myhost",
		TXT: []string{"v=1", "uuid=" + selfUUID, "ip=192.0.2.1", fmt.Sprintf("ni=%d", port)},
	}
	require.True(t, d.reachable(self), "self record must stay reachable via loopback when LAN ip= is unroutable")

	peer := RawNode{
		ID:  "myhost", // same instance name as self — must not be treated as us
		TXT: []string{"v=1", "uuid=peer-uuid", "ip=192.0.2.1", fmt.Sprintf("ni=%d", port)},
	}
	assert.False(t, d.reachable(peer), "a same-hostname peer advertising an unroutable LAN ip must not inherit the self loopback dial")
}
