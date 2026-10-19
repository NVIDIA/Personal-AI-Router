// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
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
func TestReloadIdentity_AdoptsClusterPrincipal(t *testing.T) {
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
	body, err := json.Marshal(map[string]any{"node_uuid": "cluster-principal-Y", "created_at": 1})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(idPath, body, 0o600), "write identity.json")

	d.reloadIdentity()

	require.Equal(t, "cluster-principal-Y", d.reg.record().HostUUID, "reloadIdentity hostUuid")
	// Inbound reconciliation: the directory entry moved to the new uuid and the
	// old one is gone (no ghost).
	_, ok := d.dir.get("cluster-principal-Y")
	assert.True(t, ok, "directory missing the re-stamped hostUuid after reloadIdentity")
	_, ok = d.dir.get("scanner-minted-X")
	assert.False(t, ok, "stale scanner-minted hostUuid left in the directory (ghost)")
}

// TestSelfNotEvictedByBrowse is the regression: the local node must
// never be removed from its own directory by a browse event. On Windows the host
// frequently stops looping its own multicast back, so the browser ages self like
// any peer and (its LAN-IP liveness probe failing behind a first-run firewall)
// evicts it. With self registry-driven and ignored in onBrowse, a removed/
// discovered/updated browse event for our own uuid is a no-op on the directory.
func TestBrowse_DoesNotEvictLocalNode(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318})
	d.publishSelf()
	_, ok := d.dir.get("self-uuid")
	require.True(t, ok, "publishSelf should put the local node in the directory")

	// Its own record aged out of the browse (Windows self-multicast loss): the
	// browser emits removed for our uuid. It must NOT drop the self entry.
	selfTXT := []string{"v=1", "uuid=self-uuid", "ip=192.168.1.17", "ni=14318"}
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: RawNode{ID: "myhost", TXT: selfTXT}})
	_, ok = d.dir.get("self-uuid")
	assert.True(t, ok, "a browse 'removed' for our own uuid must not evict the local node")

	// A later self re-appearance in the browse is also a no-op (self stays
	// registry-driven; the browse never clobbers it).
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "myhost", TXT: selfTXT}})
	if n, ok := d.dir.get("self-uuid"); !ok {
		assert.Fail(t, "self entry disappeared after a self 'discovered' browse event")
	} else {
		assert.Equal(t, "myhost", n.Name, "self entry Name")
	}
}

func TestLocalNodeInfo_AttachesOwnedMeasurements(t *testing.T) {
	// Verify that private worker samples populate the local registry record and
	// scheduling telemetry before membership and after departure. Re-publishing
	// local service registrations must retain that owned inventory.
	want := NodeInfoResponse{
		HostUUID:       "self-uuid",
		GPUs:           []GPUInfo{{Name: "NVIDIA GeForce RTX 5090"}},
		CPU:            &CPUInfo{Name: "AMD Ryzen 7 9800X3D", Cores: 8},
		Memory:         &MemoryInfo{TotalBytes: 68719476736},
		TelemetryValid: true,
		MSSince:        137,
	}
	want.GPUs[0].UtilizationPercent = 61
	type localInfoParams struct {
		Info NodeInfoResponse `json:"info"`
	}
	test := func(name string, leftCluster bool) {
		t.Run(name, func(t *testing.T) {
			d := newSelfTestDaemon("self-uuid", "192.0.2.17")
			if leftCluster {
				d.reg.setIdentity("self-uuid", "former-principal", selfAddrs("192.0.2.17"))
				d.reg.setIdentity("self-uuid", "", selfAddrs("192.0.2.17"))
			}
			d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318})
			params, err := json.Marshal(localInfoParams{Info: want})
			require.NoError(t, err)
			var output bytes.Buffer
			d.codec = NewCodec(&output)
			require.True(t, d.handle(&Message{Method: "discovery:local-node-info", Params: params}))
			node, ok := d.dir.get("self-uuid")
			require.True(t, ok)
			assert.Equal(t, noderec.IdentityLocal, node.IdentityVerification)
			assert.Equal(t, "192.0.2.17", node.IP)
			assert.Equal(t, want.GPUs, node.GPUs)
			assert.Equal(t, want.CPU, node.CPU)
			assert.Equal(t, want.Memory, node.Memory)
			// A later registration must retain the owned hardware sample.
			d.reg.register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434})
			d.publishSelf()
			node, _ = d.dir.get("self-uuid")
			assert.Equal(t, want.GPUs, node.GPUs, "registration must retain local inventory")
			var telemetry noderec.NodeTelemetry
			decoder := json.NewDecoder(&output)
			for decoder.More() {
				var msg Message
				require.NoError(t, decoder.Decode(&msg))
				if msg.Method == noderec.NotifyNodeTelemetry {
					require.NoError(t, json.Unmarshal(msg.Params, &telemetry))
				}
			}
			assert.Equal(t, "self-uuid", telemetry.HostUUID)
			assert.True(t, telemetry.TelemetryValid)
			assert.Equal(t, uint32(61), telemetry.GPUUtilizationPct)
			assert.GreaterOrEqual(t, telemetry.MSSince, int64(137))
		})
	}
	test("before joining", false)
	test("after leaving", true)
}

func TestLocalNodeInfo_NeverPollsPublicInventory(t *testing.T) {
	// Provide a public endpoint that would answer local reads, then verify that
	// local publication and telemetry sampling issue no requests to it.
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"hostUuid":"self-uuid","GPUs":[{"name":"unverified hardware"}]}`))
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err)
	d := newSelfTestDaemon("self-uuid", "192.0.2.17")
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: port})
	d.publishSelf()
	d.refreshTelemetryOnce(context.Background())
	assert.Equal(t, int32(0), requests.Load())
}

// TestPublishSelfLoopbackIPFallback covers the "IP unknown at startup" case: when
// the LAN ranker hasn't picked an address, self still gets a dialable host
// (loopback) so the local card is never address-less.
func TestPublishSelf_UsesLoopbackWhenNoAddressIsPublished(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "") // BestLocalIP() empty
	d.publishSelf()
	n, ok := d.dir.get("self-uuid")
	require.True(t, ok, "self missing from directory")
	assert.Equal(t, loopbackHost, n.IP, "self IP with no LAN address (%v)", loopbackHost)
}

// TestPeerStillAgesOut confirms the fix is scoped to self: a genuine remote peer
// is still added on discovery and removed on a browse 'removed' event. The peer
// advertises no services so onBrowse does no enrichment dial.
func TestReachable_RemovesPeerAfterLivenessExpires(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	peerTXT := []string{"v=1", "uuid=peer-uuid", "ip=192.168.1.99"}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "peer", TXT: peerTXT}})
	_, ok := d.dir.get("peer-uuid")
	require.True(t, ok, "a discovered peer should be added to the directory")
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: RawNode{ID: "peer", TXT: peerTXT}})
	_, ok = d.dir.get("peer-uuid")
	assert.False(t, ok, "a genuine remote peer must still age out when it leaves")
}

// TestReachableAntiFlap covers the daemon's liveness probe: prefer fresh
// lastInfo, else TCP to node-info / engine-manager, then identity when
// conclusive. Never treat ol/lm alone as proof of life.
func TestReachable_RetainsNodeAcrossTransientFailures(t *testing.T) {
	niSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			http.NotFound(w, r)
			return
		}
		_, err := w.Write([]byte(`{}`))
		assert.NoError(t, err)
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
	require.NoError(t, dl.Close())

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

func TestEnrich_FallsBackToASecondPublishedAddress(t *testing.T) {
	const hostUUID = "host-enrich-failover"
	niSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(NodeInfoResponse{
			HostUUID: hostUUID,
			GPUs:     []GPUInfo{{Name: "GPU from reachable address"}},
		}))
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

func TestReachable_RejectsAdvertisementsClaimingTheLocalHost(t *testing.T) {
	// Reject a discovery claim to our UUID even when a fresh cached sample
	// would otherwise make that UUID reachable, without any network probe.
	const selfUUID = "self-uuid"
	d := newSelfTestDaemon(selfUUID, "192.0.2.1")
	d.lastInfoAt[selfUUID] = time.Now()
	self := RawNode{
		TXT: []string{"uuid=" + selfUUID, "ip=192.0.2.1"},
	}
	assert.False(t, d.reachable(self), "an advertisement claiming our UUID must be rejected despite a fresh cached sample")
}

func TestPublishSelf_LocalRecordHasLocalVerificationAndDiscoveryRecordID(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.0.2.10")
	d.publishSelf()
	node, ok := d.dir.get("self-uuid")
	require.True(t, ok, "local registry record must exist")
	assert.Equal(t, noderec.IdentityLocal, node.IdentityVerification)
	assert.Equal(t, "local:self-uuid", node.DiscoveryRecordID)
}
