// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func TestDiscovery_CandidateAliasesYieldBusyOriginToVerifiedTelemetry(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	var requests atomic.Int32
	f.respond = func(http.ResponseWriter, *http.Request) bool {
		requests.Add(1)
		return false
	}

	// Hold an existing read's permit so copied announcements cannot line up
	// ahead of the verified peer's next scheduling sample.
	release, acquired := f.daemon.nodeInfoOrigins.acquire(context.Background(), f.server.URL)
	require.True(t, acquired)
	var releaseOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	authDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(release)
		<-authDone
	})
	for i := range 4 {
		alias := f.raw
		alias.ID = fmt.Sprintf("copied peer %d", i)
		f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: alias})
	}
	go func() {
		f.daemon.refreshCandidatesOnce(ctx)
		close(authDone)
	}()

	select {
	case <-authDone:
	case <-time.After(time.Second):
		require.FailNow(t, "candidate aliases queued behind the busy verified endpoint")
	}
	assert.Zero(t, requests.Load())
	nodes := f.daemon.dir.snapshot("")
	require.Len(t, nodes, 5, "the verified peer and four separate aliases must remain")
	var unverified int
	for _, node := range nodes {
		if node.IdentityVerification == noderec.IdentityUnverified {
			assert.Equal(t, "peer-uuid", node.HostUUID)
			assert.False(t, node.Trusted)
			unverified++
		}
	}
	assert.Equal(t, 4, unverified)

	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)
	releaseOnce.Do(release)
	f.daemon.refreshTelemetryOnce(ctx)

	message, err := NewCodec(&output).Read()
	require.NoError(t, err)
	require.Equal(t, noderec.NotifyNodeTelemetry, message.Method)
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.Equal(t, "peer-uuid", sample.HostUUID)
	assert.Equal(t, uint32(fakePeerGPUUtilization), sample.GPUUtilizationPct)
	assert.True(t, sample.TelemetryValid)
	assert.Equal(t, int32(1), requests.Load())
}

// selfAddrs returns the test address as a list, or nil when no address is available.
func selfAddrs(ip string) []string {
	if ip == "" {
		return nil
	}
	return []string{ip}
}

// newSelfTestDaemon creates a scanner with a local UUID, address, and service
// registry. Cluster membership is disabled, caches and HTTP clients are ready,
// and no network announcements or IPC notifications are sent.
func newSelfTestDaemon(hostUUID, ip string) *daemon {
	return &daemon{
		mesh:           clustertrust.Open(""),
		reg:            newRegistry(hostUUID, "", selfAddrs(ip)),
		instance:       "myhost",
		dir:            newDirectory(),
		modelsHTTP:     &http.Client{Timeout: modelsFetchTimeout},
		lastInfoAt:     make(map[string]time.Time),
		lastActivityAt: make(map[string]time.Time),
		authWake:       make(chan struct{}, 1),
		modelsWake:     make(chan struct{}, 1),
	}
}

// The scanner may create a UUID before cluster-manager writes identity.json.
// Reloading must use the UUID from that file and remove the old local record.
func TestReloadIdentity_UsesClusterIdentityUUIDAndRemovesOldRecord(t *testing.T) {
	base := t.TempDir()
	d := newSelfTestDaemon("scanner-minted-X", "127.0.0.1")
	d.baseDir = base
	_, port := startModelsStub(t, http.StatusOK, map[string]any{
		"models":         []string{"retained-model"},
		"modelsByEngine": map[string][]string{"ollama": {"retained-model"}},
		"loadedByEngine": map[string][]string{"ollama": {}},
	})
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceEngineManager, Port: port})
	d.publishSelf()
	target, present := d.dir.get("scanner-minted-X")
	require.True(t, present)
	require.True(t, d.refreshNodeModels(target))

	// cluster-manager writes the UUID to <base>/cluster/identity.json.
	idPath := filepath.Join(base, "cluster", "identity.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(idPath), 0o700), "mkdir")
	body, err := json.Marshal(map[string]any{"node_uuid": "cluster-principal-Y", "created_at": 1})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(idPath, body, 0o600), "write identity.json")

	d.reloadIdentity()

	assert.Equal(t, "cluster-principal-Y", d.reg.record().HostUUID)
	// The directory must contain the new UUID and no longer contain the old one.
	node, present := d.dir.get("cluster-principal-Y")
	require.True(t, present)
	assert.Equal(t, []string{"retained-model"}, node.Models)
	assert.Equal(t, map[string][]string{"ollama": {"retained-model"}}, node.ModelsByEngine)
	assert.Equal(t, map[string][]string{"ollama": {}}, node.LoadedByEngine)
	_, present = d.dir.get("scanner-minted-X")
	assert.False(t, present)
}

// Windows may stop receiving its own multicast announcements and report the local
// node as removed. Discovery events must preserve its local service registry record.
func TestDiscovery_BrowseEventsKeepLocalNode(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318})
	d.publishSelf()
	_, ok := d.dir.get("self-uuid")
	require.True(t, ok, "publishSelf should put the local node in the directory")

	// Losing the local announcement must not remove the local directory record.
	selfTXT := []string{"v=1", "uuid=self-uuid", "ip=192.168.1.17", "ni=14318"}
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: RawNode{ID: "myhost", TXT: selfTXT}})
	_, ok = d.dir.get("self-uuid")
	assert.True(t, ok, "a removed event for our own UUID must preserve the local node")

	// Receiving the announcement again must not replace the registry's local name.
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "myhost", TXT: selfTXT}})
	n, ok := d.dir.get("self-uuid")
	require.True(t, ok)
	assert.Equal(t, "myhost", n.Name)
}

func TestLocalNodeInfo_UsesMeasurementsFromLocalWorker(t *testing.T) {
	// Use measurements from the local worker before joining and after leaving a
	// cluster. Registering another service must retain those hardware measurements.
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
			// A later service registration must retain the local hardware measurements.
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

// Local scheduling must reflect the busiest GPU, regardless of its list position.
func TestLocalNodeInfo_ReportsHighestGPUUtilization(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.0.2.17")
	params, err := json.Marshal(noderec.LocalNodeInfoParams{Info: noderec.NodeInfo{
		HostUUID: "self-uuid", TelemetryValid: true,
		GPUs: []GPUInfo{
			{Name: "first GPU", UtilizationPercent: 12},
			{Name: "busiest GPU", UtilizationPercent: 84},
			{Name: "last GPU", UtilizationPercent: 20},
		},
	}})
	require.NoError(t, err)
	var output bytes.Buffer
	d.codec = NewCodec(&output)

	require.True(t, d.handle(&Message{Method: noderec.MethodLocalNodeInfo, Params: params}))

	message := readTelemetryMessage(t, json.NewDecoder(&output))
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.Equal(t, uint32(84), sample.GPUUtilizationPct)
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

// Use the loopback address when no LAN address has been selected at startup.
func TestPublishSelf_UsesLoopbackWhenNoLANAddressIsAvailable(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "") // BestLocalIP() empty
	d.publishSelf()
	n, ok := d.dir.get("self-uuid")
	require.True(t, ok, "self missing from directory")
	assert.Equal(t, loopbackHost, n.IP, "self IP with no LAN address (%v)", loopbackHost)
}

// An unverified peer record is removed when its announcement disappears.
// With no advertised services, there is no node-info endpoint to authenticate.
func TestDiscovery_RemovesUnverifiedRecordWhenAnnouncementDisappears(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	peerTXT := []string{"v=1", "uuid=peer-uuid", "ip=192.168.1.99"}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "peer", TXT: peerTXT}})
	_, present := d.dir.get(nodeDiscoveryRecordID(RawNode{ID: "peer", TXT: peerTXT}))
	require.True(t, present)
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: RawNode{ID: "peer", TXT: peerTXT}})
	_, present = d.dir.get(nodeDiscoveryRecordID(RawNode{ID: "peer", TXT: peerTXT}))
	assert.False(t, present)
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

// Remove the old local identity and inspect its removal event. Its
// local provenance and discovery record ID must survive so consumers invalidate
// the correct source instead of treating it as an unverified disappearance.
func TestDropSelf_PreservesRecordIdentityInRemoval(t *testing.T) {
	const oldUUID = "old-self"
	d := newSelfTestDaemon(oldUUID, "192.0.2.20")
	d.publishSelf()
	var output bytes.Buffer
	d.codec = NewCodec(&output)

	d.selfMu.Lock()
	d.dropSelf(oldUUID)
	d.selfMu.Unlock()

	message, err := NewCodec(&output).Read()
	require.NoError(t, err)
	var event noderec.NodeEvent
	require.NoError(t, json.Unmarshal(message.Params, &event))
	assert.Equal(t, noderec.NotifyNodeRemoved, message.Method)
	assert.Equal(t, "local:old-self", event.Node.DiscoveryRecordID)
	assert.Equal(t, noderec.IdentityLocal, event.Node.IdentityVerification)
}

type discoveryPeerFixture struct {
	daemon      *daemon
	selfDir     string
	peerDir     string
	server      *httptest.Server
	raw         RawNode
	info        NodeInfoResponse
	respond     func(http.ResponseWriter, *http.Request) bool
	connections atomic.Int32
}

const (
	fakePeerPort            = 11434
	fakePeerGPUUtilization  = 84
	fakePeerSampleAgeMillis = 137
)

// newDiscoveryPeerFixture creates an isolated scanner and TLS peer. Each saves
// the other's certificate as its expected identity (certificate pinning).
// The returned fixture exposes the server, node-info response, and announcement.
func newDiscoveryPeerFixture(t *testing.T, uuid string) *discoveryPeerFixture {
	t.Helper()
	selfDir, peerDir := t.TempDir(), t.TempDir()
	clustertrusttest.Join(t, selfDir, "test-cluster", "self-uuid")
	clustertrusttest.Join(t, peerDir, "test-cluster", uuid)
	writePinFromCert(t, selfDir, uuid, filepath.Join(peerDir, "node.crt"))
	writePinFromCert(t, peerDir, "self-uuid", filepath.Join(selfDir, "node.crt"))
	mesh, peerMesh := clustertrust.Open(selfDir), clustertrust.Open(peerDir)
	f := &discoveryPeerFixture{selfDir: selfDir, peerDir: peerDir}
	f.info = NodeInfoResponse{
		HostUUID: uuid, ClusterUUID: &uuid, TelemetryValid: true, MSSince: fakePeerSampleAgeMillis,
		GPUs: []GPUInfo{{Name: "verified GPU", UtilizationPercent: fakePeerGPUUtilization}},
		Peer: &noderec.PeerMetadata{Name: "owned peer", Addresses: []string{"127.0.0.1"}},
	}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerMesh.Refresh()
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if f.respond != nil && f.respond(w, r) {
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(f.info))
	}))
	f.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			f.connections.Add(1)
		}
	}
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		peerMesh.Refresh()
		return peerMesh.ServerTLSConfig().GetConfigForClient(hello)
	}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	port := portFromURL(t, f.server.URL)
	f.info.Peer.Services = map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: port, noderec.ServiceOllama: fakePeerPort}
	f.raw = RawNode{ID: "unverified name", Host: "peer.local.", Addresses: []string{"127.0.0.1"},
		TXT: []string{"uuid=" + uuid, "cluster-uuid=" + uuid, fmt.Sprintf("ni=%d", port), "ol=1"}}
	f.daemon = newSelfTestDaemon("self-uuid", "192.0.2.1")
	f.daemon.mesh = mesh
	f.daemon.clusterDir = selfDir
	// Trust reload may resolve local identity. Keep that lookup inside this
	// fixture rather than the developer's default application directory.
	f.daemon.baseDir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(f.daemon.baseDir, "node-id.json"), []byte(`{"node_uuid":"self-uuid"}`), 0o600))
	f.daemon.modelsClients = clustertrust.NewPeerClientPool(mesh, modelsFetchTimeout)
	t.Cleanup(f.daemon.modelsClients.CloseIdle)
	f.daemon.nodeInfoClients = newNodeInfoClients(mesh)
	t.Cleanup(f.daemon.nodeInfoClients.CloseIdle)
	return f
}

func TestDiscovery_ReplacesUnverifiedDiscoveryWithVerifiedPeer(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	node, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok)
	assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
	assert.True(t, node.Trusted)
	assert.NotEqual(t, nodeDiscoveryRecordID(f.raw), node.DiscoveryRecordID)
	assert.Equal(t, "owned peer", node.Name)
	assert.Equal(t, fakePeerPort, node.Services[noderec.ServiceOllama].Port)
	assert.Equal(t, f.info.GPUs, node.GPUs)
	assert.Len(t, f.daemon.dir.snapshot(""), 1)
}

// Authenticate a real pinned test peer and inspect the scanner's outbound
// notifications. Exactly one full hardware snapshot must carry that verified
// record's identity and the hardware supplied by its authenticated response.
func TestDiscovery_PublishesAuthenticatedHardwareSnapshots(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)

	f.discover(t)

	codec := NewCodec(&output)
	var snapshots int
	for {
		message, err := codec.Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if message.Method != "discovery:node-info" {
			continue
		}
		type hardwareSnapshot struct {
			DiscoveryRecordID    string                       `json:"discoveryRecordId"`
			IdentityVerification noderec.IdentityVerification `json:"identityVerification"`
			Info                 noderec.NodeInfo             `json:"info"`
		}
		var snapshot hardwareSnapshot
		require.NoError(t, json.Unmarshal(message.Params, &snapshot))
		assert.Equal(t, f.daemon.mesh.PeerRecordID("peer-uuid"), snapshot.DiscoveryRecordID)
		assert.Equal(t, noderec.IdentityPinned, snapshot.IdentityVerification)
		assert.Equal(t, f.info.GPUs, snapshot.Info.GPUs)
		snapshots++
	}
	assert.Equal(t, 1, snapshots)
}

type delayedNodeEventsWriter struct {
	bytes.Buffer
}

func (w *delayedNodeEventsWriter) Write(frame []byte) (int, error) {
	var message Message
	if err := json.Unmarshal(frame, &message); err != nil {
		return 0, err
	}
	if message.Method == noderec.NotifyNodeDiscovered {
		time.Sleep(100 * time.Millisecond)
	}
	return w.Buffer.Write(frame)
}

// Delay the directory notification ahead of the hardware notification by
// 100 ms. The later snapshot must include that delay in its GPU sample age,
// preventing a blocked notification write from making old data look fresh.
func TestDiscovery_SnapshotAgeIncludesNotificationWait(t *testing.T) {
	const peerUUID = "peer-uuid"
	f := newDiscoveryPeerFixture(t, peerUUID)
	output := &delayedNodeEventsWriter{}
	f.daemon.codec = NewCodec(output)

	f.discover(t)

	decoder := json.NewDecoder(&output.Buffer)
	var snapshot noderec.NodeInfoEvent
	for decoder.More() {
		var message Message
		require.NoError(t, decoder.Decode(&message))
		if message.Method == noderec.NotifyNodeInfo {
			require.NoError(t, json.Unmarshal(message.Params, &snapshot))
		}
	}
	assert.Equal(t, peerUUID, snapshot.HostUUID)
	assert.GreaterOrEqual(t, snapshot.Info.MSSince, int64(fakePeerSampleAgeMillis+100))
}

func TestDiscovery_FetchesModelsAfterAuthentication(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.info.Peer.Services[noderec.ServiceEngineManager] = portFromURL(t, f.server.URL)
	type modelResponse struct {
		Models         []string            `json:"models"`
		ModelsByEngine map[string][]string `json:"modelsByEngine"`
		LoadedByEngine map[string][]string `json:"loadedByEngine"`
	}
	response := modelResponse{
		Models:         []string{"reported-model"},
		ModelsByEngine: map[string][]string{"ollama": {"reported-model"}},
		LoadedByEngine: map[string][]string{"ollama": {"reported-model"}},
	}
	requested := make(chan struct{}, 1)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/models" {
			return false
		}
		node, ok := f.daemon.dir.get("peer-uuid")
		assert.True(t, ok, "models require an authenticated peer")
		assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
		requested <- struct{}{}
		assert.NoError(t, json.NewEncoder(w).Encode(response))
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		f.daemon.refreshPeersLoop(ctx)
	}()
	f.discover(t)
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "authentication did not trigger model inventory before the periodic refresh")
	}
	// The refresh finishes its model update before the loop observes cancellation.
	cancel()
	<-done
	node, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok)
	assert.Equal(t, response.Models, node.Models)
	assert.Equal(t, response.ModelsByEngine, node.ModelsByEngine)
	assert.Equal(t, response.LoadedByEngine, node.LoadedByEngine)
}

func TestDiscovery_SpoofedClaimsCannotChangeVerifiedPeers(t *testing.T) {
	// clusterUuid is the peer's node UUID from its cluster identity.
	test := func(name, hostClaim, clusterUUIDClaim string) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			f.discover(t)
			before, ok := f.daemon.dir.get("peer-uuid")
			require.True(t, ok, "established peer must exist")
			var publicReads atomic.Int32
			rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				publicReads.Add(1)
				assert.NoError(t, json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: hostClaim, ClusterUUID: &clusterUUIDClaim, TelemetryValid: true, GPUs: []GPUInfo{{Name: "forged idle GPU"}}}))
			}))
			t.Cleanup(rogue.Close)
			raw := RawNode{ID: "owned peer", Addresses: []string{"127.0.0.1"}, TXT: []string{"uuid=" + hostClaim, fmt.Sprintf("ni=%d", portFromURL(t, rogue.URL)), "ol=2"}}
			if clusterUUIDClaim != "" {
				raw.TXT = append(raw.TXT, "cluster-uuid="+clusterUUIDClaim)
			}
			f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
			f.daemon.refreshCandidatesOnce(context.Background())
			after, present := f.daemon.dir.get("peer-uuid")
			require.True(t, present)
			assert.Equal(t, before, after, "rogue discovery must preserve the established peer")
			var unverified int
			for _, node := range f.daemon.dir.snapshot("") {
				if node.DiscoveryRecordID == nodeDiscoveryRecordID(raw) {
					unverified++
					assert.False(t, node.Trusted)
					assert.Equal(t, noderec.IdentityUnverified, node.IdentityVerification)
					assert.Empty(t, node.GPUs)
				}
			}
			assert.Equal(t, 1, unverified)
			assert.Equal(t, int32(0), publicReads.Load())
			f.daemon.onBrowse(DiscoveryEvent{Type: "removed", Node: raw})
			after, present = f.daemon.dir.get("peer-uuid")
			require.True(t, present)
			assert.Equal(t, before, after, "rogue removal must preserve the established peer")
		})
	}
	test("copied host and cluster UUIDs", "peer-uuid", "peer-uuid")
	test("copied host UUID without cluster UUID", "peer-uuid", "")
	test("copied host UUID with another cluster UUID", "peer-uuid", "other-peer")
	test("new host UUID with copied cluster UUID", "fresh-uuid", "peer-uuid")
}

func TestDiscovery_RemovingSavedCertificateRemovesVerifiedPeer(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	clustertrusttest.RemovePeerPin(t, f.selfDir, "peer-uuid")
	f.daemon.reloadTrust()
	_, present := f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
}

// discover reports the peer's announcement and waits for authentication attempts.
func (f *discoveryPeerFixture) discover(t *testing.T) {
	t.Helper()
	f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: f.raw})
	f.daemon.refreshCandidatesOnce(context.Background())
}

// refreshCandidatesOnce starts pending authentication attempts with the normal
// concurrency limit, then waits for them to finish so tests can check the result.
func (d *daemon) refreshCandidatesOnce(ctx context.Context) {
	sem := make(chan struct{}, telemetryRefreshConcurrency)
	var workers sync.WaitGroup
	d.dispatchCandidates(ctx, sem, &workers)
	workers.Wait()
}

// rescheduleAuthenticationFailure checks that dispatch recorded its failure,
// then moves the retry deadline using the gate's explicit timestamps. Tests can
// examine pending and expired retries without relying on wall-clock delays.
func (f *discoveryPeerFixture) rescheduleAuthenticationFailure(t *testing.T, checkedAt, finishedAt time.Time) {
	t.Helper()
	id := nodeDiscoveryRecordID(f.raw)
	candidate := f.daemon.candidates[id]
	require.NotNil(t, candidate)
	node := candidate.node
	target := telemetryTargetKey(node.CandidateIPs(), node.Services[noderec.ServiceNodeInfo].Port) + node.ClusterUUID
	_, due := f.daemon.authRetries.claim(id, target, checkedAt)
	require.False(t, due, "failed authentication must establish backoff")
	token, due := f.daemon.authRetries.claim(id, target, time.Now().Add(24*time.Hour))
	require.True(t, due, "completed authentication must release its active attempt")
	f.daemon.authRetries.finish(id, token, false, finishedAt)
}

// Repeated observations of the same failing endpoint must retain its retry deadline.
func TestAuthenticationDispatch_AnnouncementUpdatesPreserveBackoff(t *testing.T) {
	test := func(name string, extraTXT []string) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			var reads atomic.Int32
			f.respond = func(w http.ResponseWriter, _ *http.Request) bool {
				reads.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			startedAt := time.Now()
			f.discover(t)
			require.Equal(t, int32(1), reads.Load())
			f.rescheduleAuthenticationFailure(t, startedAt, time.Now().Add(time.Hour))
			f.daemon.refreshCandidatesOnce(context.Background())
			require.Equal(t, int32(1), reads.Load(), "repeated dispatch must wait for the retry deadline")

			updated := f.raw
			updated.TXT = append(append([]string(nil), f.raw.TXT...), extraTXT...)
			f.daemon.onBrowse(DiscoveryEvent{Type: "updated", Node: updated})
			f.daemon.refreshCandidatesOnce(context.Background())

			assert.Equal(t, int32(1), reads.Load(), "an unchanged node-info endpoint must keep its backoff")
		})
	}
	test("same announcement", nil)
	test("unrelated service changes", []string{"er=14319"})
}

// A failed candidate must be reconsidered after backoff without another announcement.
func TestAuthenticationDispatch_RetriesCandidateAfterBackoff(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	var reads atomic.Int32
	f.respond = func(w http.ResponseWriter, _ *http.Request) bool {
		reads.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	startedAt := time.Now()
	f.discover(t)
	require.Equal(t, int32(1), reads.Load())
	f.rescheduleAuthenticationFailure(t, startedAt, time.Now().Add(time.Hour))
	f.daemon.refreshCandidatesOnce(context.Background())
	require.Equal(t, int32(1), reads.Load(), "failed candidate retried before backoff")

	f.rescheduleAuthenticationFailure(t, startedAt, time.Now().Add(-time.Hour))
	f.daemon.refreshCandidatesOnce(context.Background())
	assert.Equal(t, int32(2), reads.Load(), "candidate must retry after its deadline without another announcement")
}

// A failed old endpoint must not delay authentication of a newly announced endpoint.
func TestAuthenticationDispatch_ChangedEndpointCanAuthenticateDuringBackoff(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	oldURL, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	var oldReads, newReads atomic.Int32
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Host == oldURL.Host {
			oldReads.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		newReads.Add(1)
		return false
	}
	startedAt := time.Now()
	f.discover(t)
	require.Equal(t, int32(1), oldReads.Load())
	f.rescheduleAuthenticationFailure(t, startedAt, time.Now().Add(time.Hour))

	replacement := httptest.NewUnstartedServer(f.server.Config.Handler)
	replacement.TLS = f.server.TLS
	replacement.StartTLS()
	t.Cleanup(replacement.Close)
	port := portFromURL(t, replacement.URL)
	f.info.Peer.Services[noderec.ServiceNodeInfo] = port
	updated := f.raw
	updated.TXT = append([]string(nil), f.raw.TXT...)
	for i, field := range updated.TXT {
		if strings.HasPrefix(field, "ni=") {
			updated.TXT[i] = fmt.Sprintf("ni=%d", port)
		}
	}
	f.daemon.onBrowse(DiscoveryEvent{Type: "updated", Node: updated})
	f.daemon.refreshCandidatesOnce(context.Background())

	assert.Equal(t, int32(1), oldReads.Load(), "old endpoint must remain backed off")
	assert.Equal(t, int32(1), newReads.Load())
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
	assert.Equal(t, port, node.Services[noderec.ServiceNodeInfo].Port)
}

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port := splitHostPort(t, ln.Addr().String())
	require.NoError(t, ln.Close())
	return port
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err, "address %q", addr)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err, "port %q", portStr)
	return host, port
}

func TestDiscovery_RejectsMissingOrInvalidPeerDetails(t *testing.T) {
	test := func(name string, change func(*NodeInfoResponse)) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			change(&f.info)
			f.discover(t)
			_, present := f.daemon.dir.get("peer-uuid")
			assert.False(t, present)
			nodes := f.daemon.dir.snapshot("")
			require.Len(t, nodes, 1)
			assert.False(t, nodes[0].Trusted)
			assert.Empty(t, nodes[0].GPUs)
		})
	}
	test("another host UUID", func(info *NodeInfoResponse) { info.HostUUID = "other-peer" })
	test("another cluster UUID", func(info *NodeInfoResponse) { uuid := "other-peer"; info.ClusterUUID = &uuid })
	test("missing host identity", func(info *NodeInfoResponse) { info.HostUUID = "" })
	test("missing cluster UUID", func(info *NodeInfoResponse) { info.ClusterUUID = nil })
	test("missing peer metadata", func(info *NodeInfoResponse) { info.Peer = nil })
	test("invalid address", func(info *NodeInfoResponse) { info.Peer.Addresses = []string{"not-an-IP"} })
	test("missing name", func(info *NodeInfoResponse) { info.Peer.Name = "" })
	test("missing node-info port", func(info *NodeInfoResponse) { delete(info.Peer.Services, noderec.ServiceNodeInfo) })
	test("invalid service port", func(info *NodeInfoResponse) { info.Peer.Services[noderec.ServiceOllama] = 65536 })
}

func TestDiscovery_RejectsCertificatesThatDoNotMatchExpectedPeer(t *testing.T) {
	const expectedPeerUUID = "peer-uuid"

	test := func(name, presentedUUID string) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, expectedPeerUUID)
			wrongDir := t.TempDir()
			clustertrusttest.Join(t, wrongDir, "test-cluster", presentedUUID)
			// Keep the expected peer's original pin, even when the presented UUID
			// is also trusted. Another trusted peer cannot authenticate this one.
			writePinFromCert(t, f.selfDir, presentedUUID, filepath.Join(wrongDir, "node.crt"))
			writePinFromCert(t, f.selfDir, expectedPeerUUID, filepath.Join(f.peerDir, "node.crt"))
			wrongMesh := clustertrust.Open(wrongDir)
			f.server.TLS.GetConfigForClient = wrongMesh.ServerTLSConfig().GetConfigForClient
			f.discover(t)
			_, present := f.daemon.dir.get(expectedPeerUUID)
			assert.False(t, present)
		})
	}
	test("new certificate for same UUID does not match saved certificate", expectedPeerUUID)
	test("saved certificate belongs to another peer", "other-peer")
}

func TestDiscovery_DiscardsResultsInvalidatedDuringRead(t *testing.T) {
	test := func(name string, invalidate func(*testing.T, *discoveryPeerFixture)) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			f.invalidateDuringRead(t, invalidate, func() { f.discover(t) })
			_, present := f.daemon.dir.get("peer-uuid")
			assert.False(t, present)
		})
	}
	test("saved peer certificate removed", func(t *testing.T, f *discoveryPeerFixture) { clustertrusttest.RemovePeerPin(t, f.selfDir, "peer-uuid") })
	test("saved peer certificate replaced", func(t *testing.T, f *discoveryPeerFixture) { clustertrusttest.WritePeerPin(t, f.selfDir, "peer-uuid") })
	test("local node leaves cluster", func(t *testing.T, f *discoveryPeerFixture) {
		clustertrusttest.RemovePeerPin(t, f.selfDir, "peer-uuid")
		require.NoError(t, os.Remove(filepath.Join(f.selfDir, "admission.json")))
	})
	test("local certificate replaced", func(t *testing.T, f *discoveryPeerFixture) { clustertrusttest.WriteKeypair(t, f.selfDir, "self-uuid") })
	test("local node readmitted with the same certificate", func(t *testing.T, f *discoveryPeerFixture) {
		clustertrusttest.WriteAdmission(t, f.selfDir, "test-cluster", 2)
	})
	test("discovery removed", func(t *testing.T, f *discoveryPeerFixture) {
		f.daemon.onBrowse(DiscoveryEvent{Type: "removed", Node: f.raw})
	})
}

// invalidateDuringRead holds the HTTP response while the test changes trust.
// File setup and its fatal assertions run on the test goroutine.
func (f *discoveryPeerFixture) invalidateDuringRead(t *testing.T, invalidate func(*testing.T, *discoveryPeerFixture), read func()) {
	t.Helper()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	f.respond = func(http.ResponseWriter, *http.Request) bool {
		close(entered)
		<-release
		return false
	}
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-done })
	go func() {
		defer close(done)
		read()
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "inventory request did not reach the server")
	}
	invalidate(t, f)
	releaseOnce.Do(func() { close(release) })
	<-done
}

func TestDiscovery_ReauthenticatesAfterSavedCertificateChanges(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	previous, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	clustertrusttest.WriteKeypair(t, f.peerDir, "peer-uuid")
	writePinFromCert(t, f.selfDir, "peer-uuid", filepath.Join(f.peerDir, "node.crt"))
	f.daemon.reloadTrust()
	_, present = f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
	f.daemon.refreshCandidatesOnce(context.Background())
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.True(t, node.Trusted)
	assert.NotEqual(t, previous.DiscoveryRecordID, node.DiscoveryRecordID)
}

func TestDiscovery_LocalCertificateReplacementDiscardsVerifiedInventory(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)

	clustertrusttest.WriteKeypair(t, f.selfDir, "self-uuid")
	f.daemon.reloadTrust()

	_, present := f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
}

type heldModelResponseTransport struct {
	http.RoundTripper
	closed  chan<- struct{}
	release <-chan struct{}
}

func (transport heldModelResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.RoundTripper.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &heldModelResponseBody{ReadCloser: response.Body, closed: transport.closed, release: transport.release}
	return response, nil
}

type heldModelResponseBody struct {
	io.ReadCloser
	closed  chan<- struct{}
	release <-chan struct{}
}

func (body *heldModelResponseBody) Close() error {
	err := body.ReadCloser.Close()
	close(body.closed)
	<-body.release
	return err
}

func TestDiscovery_DiscardsModelResponseAfterSavedCertificateReplacement(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	port := portFromURL(t, f.server.URL)
	f.info.Peer.Services[noderec.ServiceEngineManager] = port
	type modelResponse struct {
		Models         []string            `json:"models"`
		ModelsByEngine map[string][]string `json:"modelsByEngine"`
		LoadedByEngine map[string][]string `json:"loadedByEngine"`
	}
	response := modelResponse{
		Models:         []string{"reported-model"},
		ModelsByEngine: map[string][]string{"ollama": {"reported-model"}},
		LoadedByEngine: map[string][]string{"ollama": {"reported-model"}},
	}
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/models" {
			return false
		}
		assert.NoError(t, json.NewEncoder(w).Encode(response))
		return true
	}
	f.discover(t)
	target, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	client, ok := f.daemon.modelsClients.Client("peer-uuid")
	require.True(t, ok, "authenticated models client must be available")
	closed, release := make(chan struct{}), make(chan struct{})
	// fetchModels closes the body after its certificate check. Pause there so
	// replacement happens after that check but before the directory update.
	client.Transport = heldModelResponseTransport{RoundTripper: client.Transport, closed: closed, release: release}
	changed := make(chan bool, 1)
	done := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-done })
	go func() {
		defer close(done)
		changed <- f.daemon.refreshNodeModels(target)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "model response did not reach the point before directory update")
	}
	clustertrusttest.WriteKeypair(t, f.peerDir, "peer-uuid")
	writePinFromCert(t, f.selfDir, "peer-uuid", filepath.Join(f.peerDir, "node.crt"))
	f.daemon.reloadTrust()
	f.discover(t)
	before, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok)
	require.True(t, before.Trusted)
	require.Empty(t, before.Models)
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)
	releaseOnce.Do(func() { close(release) })
	<-done
	assert.False(t, <-changed, "model response from the replaced certificate must not update the new peer")
	after, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok)
	assert.Equal(t, before, after)
	assert.Empty(t, output.Bytes())
	require.True(t, f.daemon.refreshNodeModels(before))
	after, _ = f.daemon.dir.get("peer-uuid")
	assert.Equal(t, response.Models, after.Models)
	assert.Equal(t, response.ModelsByEngine, after.ModelsByEngine)
	assert.Equal(t, response.LoadedByEngine, after.LoadedByEngine)
}

func TestRefreshNodeModels_AcceptsPinnedPeersWithMultipleAddresses(t *testing.T) {
	const peerUUID = "peer-uuid"
	f := newDiscoveryPeerFixture(t, peerUUID)
	port := portFromURL(t, f.server.URL)
	f.info.Peer.Services[noderec.ServiceEngineManager] = port
	f.info.Peer.Addresses = []string{"127.0.0.1", "127.0.0.2"}
	type modelResponse struct {
		Models []string `json:"models"`
	}
	response := modelResponse{Models: []string{"reported-model"}}
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/models" {
			return false
		}
		assert.NoError(t, json.NewEncoder(w).Encode(response))
		return true
	}
	f.discover(t)

	target, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)
	assert.True(t, f.daemon.refreshNodeModels(target))

	node, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)
	assert.Equal(t, response.Models, node.Models)
}

func TestDiscovery_DiscardsModelsAfterSecondaryAddressChanges(t *testing.T) {
	const peerUUID = "peer-uuid"
	const primaryAddress = "127.0.0.2"

	test := func(name, secondaryAddress string, wantAccepted bool) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, peerUUID)
			port := portFromURL(t, f.server.URL)
			f.info.Peer.Services[noderec.ServiceEngineManager] = port
			// Only the secondary address reaches this peer's server.
			f.info.Peer.Addresses = []string{primaryAddress, "127.0.0.1"}
			type modelResponse struct {
				Models []string `json:"models"`
			}
			response := modelResponse{Models: []string{"reported-model"}}
			f.respond = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/v1/models" {
					return false
				}
				assert.NoError(t, json.NewEncoder(w).Encode(response))
				return true
			}
			f.discover(t)
			original, present := f.daemon.dir.get(peerUUID)
			require.True(t, present)
			client, ok := f.daemon.modelsClients.Client(peerUUID)
			require.True(t, ok)
			closed, release := make(chan struct{}), make(chan struct{})
			client.Transport = heldModelResponseTransport{RoundTripper: client.Transport, closed: closed, release: release}
			changed := make(chan bool, 1)
			done := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-done })
			go func() {
				defer close(done)
				changed <- f.daemon.refreshNodeModels(original)
			}()
			select {
			case <-closed:
			case <-time.After(8 * time.Second):
				require.FailNow(t, "model response did not reach the point before directory update")
			}

			// Refresh authoritative inventory while the old model result waits to
			// be applied. The primary address, port, and pin remain unchanged.
			f.info.Peer.Addresses = []string{primaryAddress, secondaryAddress}
			require.True(t, f.daemon.refreshNodeTelemetryCandidates(context.Background(), peerUUID, original.CandidateIPs(), original.Services[noderec.ServiceNodeInfo].Port))
			before, present := f.daemon.dir.get(peerUUID)
			require.True(t, present)
			require.Equal(t, []string{primaryAddress, secondaryAddress}, before.CandidateIPs())
			var output bytes.Buffer
			f.daemon.codec = NewCodec(&output)

			releaseOnce.Do(func() { close(release) })
			<-done

			assert.Equal(t, wantAccepted, <-changed)
			after, present := f.daemon.dir.get(peerUUID)
			require.True(t, present)
			if wantAccepted {
				assert.Equal(t, response.Models, after.Models)
				return
			}
			assert.Equal(t, before, after)
			assert.Empty(t, output.Bytes())
		})
	}

	test("unchanged addresses accept the model response", "127.0.0.1", true)
	test("removed secondary address discards the model response", "127.0.0.3", false)
}

func TestDiscovery_DiscardsQueuedModelsForChangedAddresses(t *testing.T) {
	const peerUUID = "peer-uuid"
	const primaryAddress = "127.0.0.2"
	f := newDiscoveryPeerFixture(t, peerUUID)
	port := portFromURL(t, f.server.URL)
	f.info.Peer.Services[noderec.ServiceEngineManager] = port
	f.info.Peer.Addresses = []string{primaryAddress, "127.0.0.1"}
	type modelResponse struct {
		Models []string `json:"models"`
	}
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/models" {
			return false
		}
		assert.NoError(t, json.NewEncoder(w).Encode(modelResponse{Models: []string{"obsolete-model"}}))
		return true
	}
	f.discover(t)
	original, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)

	// The model sweep queued the old addresses before receiving new inventory.
	f.info.Peer.Addresses = []string{primaryAddress, "127.0.0.3"}
	require.True(t, f.daemon.refreshNodeTelemetryCandidates(context.Background(), peerUUID, original.CandidateIPs(), original.Services[noderec.ServiceNodeInfo].Port))
	before, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)

	assert.False(t, f.daemon.refreshNodeModels(original))

	after, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)
	assert.Equal(t, before, after)
	assert.Empty(t, output.Bytes())
}

func TestDiscovery_DiscardsQueuedModelsAfterReadmission(t *testing.T) {
	const peerUUID = "peer-uuid"
	f := newDiscoveryPeerFixture(t, peerUUID)
	f.info.Peer.Services[noderec.ServiceEngineManager] = portFromURL(t, f.server.URL)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/models" {
			return false
		}
		assert.NoError(t, json.NewEncoder(w).Encode(modelInventoryResponse{Models: []string{"reported-model"}}))
		return true
	}
	f.discover(t)
	target, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)

	// Queued work keeps its original proof even when readmission leaves the
	// peer's certificate and endpoints unchanged.
	clustertrusttest.WriteAdmission(t, f.selfDir, "test-cluster", 2)
	f.daemon.reloadTrust()
	f.discover(t)
	before, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)
	require.NotEqual(t, target.DiscoveryRecordID, before.DiscoveryRecordID)
	require.Equal(t, target.CandidateIPs(), before.CandidateIPs())
	require.Equal(t, target.Services, before.Services)
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)

	assert.False(t, f.daemon.refreshNodeModels(target))
	after, present := f.daemon.dir.get(peerUUID)
	require.True(t, present)
	assert.Equal(t, before, after)
	assert.Empty(t, output.Bytes())
	assert.True(t, f.daemon.refreshNodeModels(before))
}

func TestDiscovery_PeerActivityDoesNotKeepSpoofedDiscoveryReachable(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.daemon.noteActivity("peer-uuid", 0)
	require.True(t, f.daemon.reachable(f.raw), "recent authenticated node-info must keep its peer reachable")
	spoof := f.raw
	spoof.ID = "spoof"
	f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: spoof})
	assert.False(t, f.daemon.reachable(spoof), "verified activity must not keep a spoofed discovery reachable")
	f.server.Close()
	f.daemon.infoMu.Lock()
	f.daemon.lastInfoAt["peer-uuid"] = time.Now().Add(-reachableInfoFreshness - time.Second)
	f.daemon.infoMu.Unlock()
	assert.True(t, f.daemon.reachable(f.raw), "recent inference activity must keep the verified peer reachable")
	clustertrusttest.RemovePeerPin(t, f.selfDir, "peer-uuid")
	assert.False(t, f.daemon.reachable(f.raw), "activity must not retain a peer after its certificate is removed")
}

func TestPeerReachable_RejectsCachedLivenessAfterReadmission(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.daemon.noteActivity("peer-uuid", 0)
	require.True(t, f.daemon.peerReachable("peer-uuid"))
	require.True(t, f.daemon.reachable(f.raw))

	// Readmission changes the proof binding even when both certificates remain.
	// Availability checks must reject old evidence before a reload notification.
	clustertrusttest.WriteAdmission(t, f.selfDir, "test-cluster", 2)

	assert.False(t, f.daemon.peerReachable("peer-uuid"))
	assert.False(t, f.daemon.reachable(f.raw))

	f.daemon.reloadTrust()
	f.daemon.refreshCandidatesOnce(context.Background())
	assert.True(t, f.daemon.reachable(f.raw))
}

func TestDiscovery_RequiresReauthenticationBeforeReadmittedPeerTelemetry(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	before, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	acceptedAt := f.daemon.lastInfoAt["peer-uuid"]
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)

	// A fresh read under a new admission must not renew a directory record
	// authenticated under the previous admission, even with the same certificate.
	clustertrusttest.WriteAdmission(t, f.selfDir, "test-cluster", 2)
	assert.False(t, f.daemon.refreshNodeTelemetryCandidates(context.Background(), before.HostUUID, before.CandidateIPs(), before.Services[noderec.ServiceNodeInfo].Port))
	after, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.Equal(t, before, after)
	assert.Equal(t, acceptedAt, f.daemon.lastInfoAt["peer-uuid"])
	assert.Empty(t, output.Bytes())

	f.daemon.reloadTrust()
	f.daemon.refreshCandidatesOnce(context.Background())
	after, present = f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.NotEqual(t, before.DiscoveryRecordID, after.DiscoveryRecordID)
	assert.True(t, f.daemon.refreshNodeTelemetryCandidates(context.Background(), after.HostUUID, after.CandidateIPs(), after.Services[noderec.ServiceNodeInfo].Port))
}

func TestDiscovery_RefreshesOnlyAuthenticatedInventory(t *testing.T) {
	const updatedPort = 11435
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.info.Peer.Name = "updated owned name"
	f.info.Peer.Services[noderec.ServiceOllama] = updatedPort
	f.info.CPU = &CPUInfo{Name: "updated CPU"}
	f.daemon.refreshTelemetryOnce(context.Background())
	node, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok)
	assert.Equal(t, f.info.Peer.Name, node.Name)
	assert.Equal(t, updatedPort, node.Services[noderec.ServiceOllama].Port)
	assert.Equal(t, f.info.CPU, node.CPU)
	f.server.Close()
	f.daemon.refreshTelemetryOnce(context.Background())
	after, _ := f.daemon.dir.get("peer-uuid")
	assert.Equal(t, node, after, "temporary failure must preserve inventory")
}

func TestDiscovery_SlowAuthenticationDoesNotBlockVerifiedPeerTelemetry(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	slow := newDiscoveryPeerFixture(t, "slow-peer")
	writePinFromCert(t, f.selfDir, "slow-peer", filepath.Join(slow.peerDir, "node.crt"))
	writePinFromCert(t, slow.peerDir, "self-uuid", filepath.Join(f.selfDir, "node.crt"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow.respond = func(http.ResponseWriter, *http.Request) bool {
		once.Do(func() { close(entered) })
		<-release
		return false
	}
	f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: slow.raw})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.daemon.authenticateCandidatesLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); close(release); <-done })
	select {
	case <-entered:
	case <-time.After(time.Second):
		require.FailNow(t, "authentication request to the slow peer did not start")
	}
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)
	f.daemon.refreshTelemetryOnce(context.Background())
	message := readTelemetryMessage(t, json.NewDecoder(&output))
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.Equal(t, "peer-uuid", sample.HostUUID)
	assert.Equal(t, uint32(fakePeerGPUUtilization), sample.GPUUtilizationPct)
}

func TestDiscovery_TelemetryIncludesSampleAgeAndReusesConnection(t *testing.T) {
	const maxSampleAgeMillis = 3500
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)
	for range 3 {
		f.daemon.refreshTelemetryOnce(context.Background())
	}
	decoder := json.NewDecoder(&output)
	for range 3 {
		message := readTelemetryMessage(t, decoder)
		var sample noderec.NodeTelemetry
		require.NoError(t, json.Unmarshal(message.Params, &sample))
		assert.Equal(t, "peer-uuid", sample.HostUUID)
		assert.Equal(t, uint32(fakePeerGPUUtilization), sample.GPUUtilizationPct)
		assert.True(t, sample.TelemetryValid)
		assert.GreaterOrEqual(t, sample.MSSince, int64(fakePeerSampleAgeMillis))
		assert.LessOrEqual(t, sample.MSSince, int64(maxSampleAgeMillis))
	}
	assert.Equal(t, int32(1), f.connections.Load())
}

// Authenticated remote telemetry must report the maximum across every GPU.
func TestDiscovery_TelemetryReportsHighestGPUUtilization(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.info.GPUs = []GPUInfo{
		{Name: "first GPU", UtilizationPercent: 12},
		{Name: "busiest GPU", UtilizationPercent: 84},
		{Name: "last GPU", UtilizationPercent: 20},
	}
	f.discover(t)
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)

	f.daemon.refreshTelemetryOnce(context.Background())

	message := readTelemetryMessage(t, json.NewDecoder(&output))
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.Equal(t, uint32(84), sample.GPUUtilizationPct)
}

func TestDiscovery_InvalidTelemetryStaysInvalidAndReportsNoAge(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.info.TelemetryValid, f.info.MSSince = false, 10000
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)
	f.daemon.refreshTelemetryOnce(context.Background())
	message := readTelemetryMessage(t, json.NewDecoder(&output))
	var sample noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &sample))
	assert.False(t, sample.TelemetryValid)
	assert.Zero(t, sample.MSSince)
}

func readTelemetryMessage(t *testing.T, decoder *json.Decoder) Message {
	t.Helper()
	for {
		var message Message
		require.NoError(t, decoder.Decode(&message))
		if message.Method == noderec.NotifyNodeTelemetry {
			return message
		}
	}
}

func TestDiscovery_RechecksSavedCertificatesOnReusedConnections(t *testing.T) {
	test := func(name string, invalidate func(*testing.T, *discoveryPeerFixture)) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			f.discover(t)
			var output bytes.Buffer
			f.daemon.codec = NewCodec(&output)
			f.invalidateDuringRead(t, invalidate, func() { f.daemon.refreshTelemetryOnce(context.Background()) })
			assert.Empty(t, output.Bytes(), "invalidated requests must not emit telemetry")
			assert.Equal(t, int32(1), f.connections.Load())
		})
	}
	test("saved peer certificate removed during response", func(t *testing.T, f *discoveryPeerFixture) { clustertrusttest.RemovePeerPin(t, f.selfDir, "peer-uuid") })
	test("saved peer certificate replaced during response", func(t *testing.T, f *discoveryPeerFixture) { clustertrusttest.WritePeerPin(t, f.selfDir, "peer-uuid") })
}

func TestDiscovery_QueuedNodeInfoRequestsGetTheirOwnTimeout(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.daemon.nodeInfoClients.CloseIdle()
	f.daemon.nodeInfoClients = clustertrust.NewPeerClientPoolOpts(f.daemon.mesh, clustertrust.PeerClientOptions{
		Timeout: 300 * time.Millisecond, DialTimeout: nodeInfoDialTimeout, MaxConnsPerHost: 1,
	})
	t.Cleanup(f.daemon.nodeInfoClients.CloseIdle)
	// The requests share one connection, so the second waits for the first.
	f.connections.Store(0)
	f.respond = func(http.ResponseWriter, *http.Request) bool { time.Sleep(200 * time.Millisecond); return false }
	node, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok, "verified node must exist")
	var workers sync.WaitGroup
	results := make(chan bool, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, ok := f.daemon.readPeerInfo(context.Background(), node)
			results <- ok
		}()
	}
	workers.Wait()
	close(results)
	for ok := range results {
		assert.True(t, ok, "queued healthy request must succeed")
	}
	assert.Equal(t, int32(1), f.connections.Load())
}

func TestDiscovery_TriesNextAddressWhenFirstAddressFails(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.raw.Addresses = []string{"127.0.0.2", "127.0.0.1"}
	f.discover(t)
	node, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok)
	assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
}

func TestDiscovery_NewSavedCertificateRequiresAuthenticationBeforeTrust(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	clustertrusttest.RemovePeerPin(t, f.selfDir, "peer-uuid")
	f.discover(t)
	_, present := f.daemon.dir.get("peer-uuid")
	assert.False(t, present, "verification requires a saved certificate")
	writePinFromCert(t, f.selfDir, "peer-uuid", filepath.Join(f.peerDir, "node.crt"))
	f.daemon.reloadTrust()
	_, present = f.daemon.dir.get("peer-uuid")
	assert.False(t, present, "saving a certificate must not grant trust before authentication")
	f.daemon.refreshCandidatesOnce(context.Background())
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.True(t, node.Trusted)
}

func (f *discoveryPeerFixture) attach(t *testing.T, d *daemon) {
	t.Helper()
	writePinFromCert(t, d.clusterDir, f.info.HostUUID, filepath.Join(f.peerDir, "node.crt"))
	writePinFromCert(t, f.peerDir, "self-uuid", filepath.Join(d.clusterDir, "node.crt"))
	f.daemon = d
	f.discover(t)
}

func TestDiscovery_TelemetryRequestsStayWithinConcurrencyLimitAcrossRefreshes(t *testing.T) {
	first := newDiscoveryPeerFixture(t, "peer-0")
	d := first.daemon
	first.discover(t)
	fixtures := []*discoveryPeerFixture{first}
	for i := 1; i <= telemetryRefreshConcurrency; i++ {
		f := newDiscoveryPeerFixture(t, fmt.Sprintf("peer-%d", i))
		f.attach(t, d)
		fixtures = append(fixtures, f)
	}
	entered := make(chan struct{}, len(fixtures))
	release := make(chan struct{})
	var releaseOnce sync.Once
	for _, f := range fixtures {
		f.respond = func(w http.ResponseWriter, r *http.Request) bool {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.runTelemetryLoop(ctx, 5*time.Millisecond); close(done) }()
	t.Cleanup(func() { cancel(); releaseOnce.Do(func() { close(release) }); <-done })
	for range telemetryRefreshConcurrency {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			require.FailNow(t, "telemetry did not start the allowed number of simultaneous requests")
		}
	}
	select {
	case <-entered:
		require.FailNow(t, "later refreshes exceeded the limit on simultaneous telemetry requests")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-entered:
	case <-time.After(time.Second):
		require.FailNow(t, "queued peer did not start when capacity became available")
	}
}

func TestDiscovery_RemovesUnavailablePeerAfterItsDiscoveryExpires(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.server.Close()
	delete(f.daemon.lastInfoAt, "peer-uuid")
	f.daemon.onBrowse(DiscoveryEvent{Type: "removed", Node: f.raw})
	_, present := f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
}

func TestDiscovery_RemovesExpiredDiscoveryAfterUUIDClaimChanges(t *testing.T) {
	test := func(name, uuidClaim string, discover func(*testing.T, *discoveryPeerFixture)) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "peer-uuid")
			discover(t, f)
			recordID := nodeDiscoveryRecordID(f.raw)
			require.NotNil(t, f.daemon.candidates[recordID])
			f.daemon.publishSelf()
			selfUUID := f.daemon.reg.record().HostUUID
			self, ok := f.daemon.dir.get(selfUUID)
			require.True(t, ok, "local record must exist")
			f.server.Close()
			delete(f.daemon.lastInfoAt, "peer-uuid")
			updatedTXT := make([]string, 0, len(f.raw.TXT))
			for _, field := range f.raw.TXT {
				if !strings.HasPrefix(field, "uuid=") {
					updatedTXT = append(updatedTXT, field)
				}
			}
			if uuidClaim != "" {
				updatedTXT = append(updatedTXT, "uuid="+uuidClaim)
			}
			f.raw.TXT = updatedTXT
			require.Equal(t, recordID, nodeDiscoveryRecordID(f.raw))
			f.daemon.onBrowse(DiscoveryEvent{Type: "updated", Node: f.raw})
			f.daemon.onBrowse(DiscoveryEvent{Type: "removed", Node: f.raw})

			assert.Nil(t, f.daemon.candidates[recordID])
			_, present := f.daemon.dir.get(recordID)
			assert.False(t, present, "expired unverified discovery must be removed")
			_, present = f.daemon.dir.get("peer-uuid")
			assert.False(t, present, "expired unavailable peer must be removed")
			after, present := f.daemon.dir.get(selfUUID)
			require.True(t, present)
			assert.Equal(t, self, after, "discovery removal must preserve the local record")
		})
	}
	test("verified peer claims local UUID", "self-uuid", func(t *testing.T, f *discoveryPeerFixture) {
		f.discover(t)
	})
	test("unverified discovery claims local UUID", "self-uuid", func(t *testing.T, f *discoveryPeerFixture) {
		f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: f.raw})
	})
	test("verified peer drops UUID", "", func(t *testing.T, f *discoveryPeerFixture) {
		f.discover(t)
	})
	test("unverified discovery drops UUID", "", func(t *testing.T, f *discoveryPeerFixture) {
		f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: f.raw})
	})
}

func TestDiscovery_RetainedPeerExpiresAfterAvailabilityEvidenceEnds(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	refresh := func() {
		var workers sync.WaitGroup
		// Advance dispatch time past any failed attempt's retry deadline.
		f.daemon.dispatchTelemetry(context.Background(), time.Now().Add(24*time.Hour), make(chan struct{}, telemetryRefreshConcurrency), &workers)
		workers.Wait()
	}
	f.daemon.onBrowse(DiscoveryEvent{Type: "removed", Node: f.raw})
	assert.Empty(t, f.daemon.candidates)
	refresh()
	_, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present, "reachable peer must survive announcement expiry")
	f.server.Close()
	f.daemon.lastInfoAt["peer-uuid"] = time.Now().Add(-15 * time.Second)
	refresh()
	_, present = f.daemon.dir.get("peer-uuid")
	require.True(t, present, "temporary node-info failure must retain the peer")
	f.daemon.lastInfoAt["peer-uuid"] = time.Now().Add(-2 * time.Minute)
	f.daemon.noteActivity("peer-uuid", 0)
	refresh()
	_, present = f.daemon.dir.get("peer-uuid")
	require.True(t, present, "recent inference activity must retain the peer")
	f.daemon.lastActivityAt["peer-uuid"] = time.Now().Add(-activityFreshness - time.Second)
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)
	refresh()
	_, present = f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
	assert.Empty(t, f.daemon.lastInfoAt)
	var message Message
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &message))
	var removed noderec.NodeEvent
	require.NoError(t, json.Unmarshal(message.Params, &removed))
	assert.Equal(t, noderec.NotifyNodeRemoved, message.Method)
	assert.Equal(t, f.daemon.mesh.PeerRecordID("peer-uuid"), removed.Node.DiscoveryRecordID)
	f.daemon.mesh.Refresh()
	assert.True(t, f.daemon.mesh.Clustered())
	assert.True(t, f.daemon.mesh.HasPin("peer-uuid"))
}

func TestDiscovery_FailedRefreshKeepsPeersWithActiveDiscoveries(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.server.Close()
	f.daemon.lastInfoAt["peer-uuid"] = time.Now().Add(-2 * time.Minute)
	f.daemon.refreshTelemetryOnce(context.Background())
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.True(t, node.Trusted)
}

func TestDiscovery_RejectsNodeInfoResponsesExceedingSizeLimit(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.respond = func(w http.ResponseWriter, _ *http.Request) bool {
		assert.NoError(t, json.NewEncoder(w).Encode(f.info))
		_, err := io.WriteString(w, strings.Repeat(" ", nodeInfoResponseDrainLimit))
		assert.NoError(t, err)
		return true
	}
	f.discover(t)
	_, present := f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
}

func TestDiscovery_RejectsRedirectsToPublicHTTP(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	var reads atomic.Int32
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reads.Add(1) }))
	t.Cleanup(public.Close)
	f.respond = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, public.URL, http.StatusTemporaryRedirect)
		return true
	}
	f.discover(t)
	assert.Equal(t, int32(0), reads.Load())
	_, present := f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
}

type peerProcessPipes struct {
	io.Reader
	io.Writer
}

func TestDiscovery_AuthenticatesRealNodeInfoProcess(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.server.Close()
	path := filepath.Join(t.TempDir(), "nvpair-node-info")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	build := exec.Command("go", "build", "-o", path, ".")
	build.Dir = "../nvpair-node-info"
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build node-info: %s", output)
	port := closedPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, path, "--cluster-dir", f.peerDir, "--node-id", "peer-uuid", "--port", strconv.Itoa(port))
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		assert.NoError(t, stdin.Close())
		assert.NoError(t, cmd.Wait())
	})
	codec := NewCodec(peerProcessPipes{Reader: stdout, Writer: stdin})
	for {
		message, err := codec.Read()
		require.NoError(t, err)
		if message.Method == noderec.NotifyInfoUpdated {
			break
		}
	}
	metadata := noderec.PeerMetadataParams{HostUUID: "peer-uuid", ClusterUUID: "peer-uuid", Peer: noderec.PeerMetadata{
		Name: "owned process metadata", Addresses: []string{"127.0.0.1"}, Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: port, noderec.ServiceOllama: fakePeerPort},
	}}
	params, err := json.Marshal(metadata)
	require.NoError(t, err)
	id := json.RawMessage("1")
	require.NoError(t, json.NewEncoder(stdin).Encode(Message{JSONRPC: "2.0", ID: &id, Method: noderec.MethodSetPeerMetadata, Params: params}))
	for {
		message, err := codec.Read()
		require.NoError(t, err)
		if message.ID == nil {
			continue
		}
		require.Nil(t, message.Error)
		break
	}
	f.raw.TXT = []string{"uuid=peer-uuid", "cluster-uuid=peer-uuid", fmt.Sprintf("ni=%d", port), "ol=1"}
	f.discover(t)
	node, ok := f.daemon.dir.get("peer-uuid")
	require.True(t, ok)
	assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
	assert.Equal(t, metadata.Peer.Name, node.Name)
	assert.Equal(t, fakePeerPort, node.Services[noderec.ServiceOllama].Port)
	public := &http.Client{Timeout: time.Second}
	t.Cleanup(public.CloseIdleConnections)
	resp, err := public.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/v1/node-info")
	require.NoError(t, err)
	defer resp.Body.Close()
	var info NodeInfoResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "peer-uuid", info.HostUUID)
	assert.Nil(t, info.Peer)
}

func TestDiscovery_UnverifiedAddressesCannotInfluenceLocalAddressSelection(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{ID: "rogue", Addresses: []string{"192.0.2.30"}, TXT: []string{"uuid=peer-uuid", "ni=14318"}}})
	assert.Equal(t, []string{"127.0.0.1", "127.0.0.1"}, f.daemon.peerAddresses())
}

func TestDiscovery_FailedAnnouncementUpdateStillAllowsRemovalOfUnavailablePeer(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.server.Close()
	delete(f.daemon.lastInfoAt, "peer-uuid")
	updated := f.raw
	updated.TXT = append(append([]string(nil), f.raw.TXT...), "er=14319")
	f.daemon.onBrowse(DiscoveryEvent{Type: "updated", Node: updated})
	f.daemon.refreshCandidatesOnce(context.Background())
	f.daemon.onBrowse(DiscoveryEvent{Type: "removed", Node: updated})
	_, present := f.daemon.dir.get("peer-uuid")
	assert.False(t, present)
}
