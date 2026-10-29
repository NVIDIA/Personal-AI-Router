// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

func TestNodeInfo_StreamsPrivateSnapshotsAndAcceptsMetadata(t *testing.T) {
	// Exercise the real node-info process, with this test acting as its broker.
	// Verify that unsolicited snapshots reflect membership updates, metadata
	// requests are acknowledged, and public HTTP stays available without
	// exposing the peer metadata.
	const hostUUID = "owned-host"
	const clusterPrincipal = "principal"
	const peerName = "workstation"
	const peerAddress = "192.0.2.20"

	process := startNodeInfoForTest(t, hostUUID)

	assert.Nil(t, process.nextSnapshot(t).ClusterUUID)
	// The next snapshot must arrive without any HTTP request or IPC trigger.
	process.nextSnapshot(t)

	metadata := noderec.PeerMetadataParams{
		HostUUID: hostUUID,
		Peer: noderec.PeerMetadata{
			Name:      peerName,
			Addresses: []string{peerAddress},
			Services:  map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: process.port},
		},
	}
	require.True(t, process.requestPeerMetadata(t, metadata).OK)

	require.NoError(t, process.codec.Notify(noderec.MethodSetClusterIdentity, noderec.ClusterIdentityParams{ClusterUUID: clusterPrincipal}))
	info := process.nextSnapshot(t)
	require.NotNil(t, info.ClusterUUID)
	require.Equal(t, clusterPrincipal, *info.ClusterUUID)
	require.NoError(t, process.codec.Notify(noderec.MethodSetClusterIdentity, noderec.ClusterIdentityParams{}))
	info = process.nextSnapshot(t)
	require.NotNil(t, info.ClusterUUID)
	assert.Empty(t, *info.ClusterUUID)

	client := &http.Client{Timeout: 2 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Get(process.inventoryURL)
	require.NoError(t, err)
	defer response.Body.Close()
	type publicInventory struct {
		HostUUID string                `json:"hostUuid"`
		GPUs     json.RawMessage       `json:"GPUs"`
		Peer     *noderec.PeerMetadata `json:"peer,omitempty"`
	}
	var public publicInventory
	require.NoError(t, json.NewDecoder(response.Body).Decode(&public))
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, hostUUID, public.HostUUID)
	assert.NotNil(t, public.GPUs)
	assert.Nil(t, public.Peer)
}

// Drive membership on disk while a real owned worker is running. Its private
// stream and public hardware display must work before join and after leave.
func TestNodeInfo_PrivateIPCContinuesAcrossJoinAndLeave(t *testing.T) {
	const hostUUID = "owned-host"
	dir := t.TempDir()
	process := startNodeInfoForTest(t, hostUUID, "--cluster-dir", dir)
	initial := process.nextSnapshot(t)
	assert.True(t, initial.ClusterUUID == nil || *initial.ClusterUUID == "")

	clustertrusttest.Join(t, dir, "test-cluster", hostUUID)
	var joined noderec.NodeInfo
	for joined.ClusterUUID == nil || *joined.ClusterUUID != hostUUID {
		joined = process.nextSnapshot(t)
	}
	// A polling reader can still hold the admission file open during departure.
	// Clear active membership in place so departure also works with that reader.
	admissionReader, err := os.Open(filepath.Join(dir, "admission.json"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, admissionReader.Close()) })
	clustertrusttest.WriteAdmission(t, dir, "", 0)
	left := process.nextSnapshot(t)
	for left.ClusterUUID != nil && *left.ClusterUUID != "" {
		left = process.nextSnapshot(t)
	}
	assert.Equal(t, initial.HostUUID, left.HostUUID)
	assert.NotNil(t, left.GPUs)

	client := &http.Client{Timeout: 2 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Get(process.inventoryURL)
	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()
	var public noderec.NodeInfo
	require.NoError(t, json.NewDecoder(response.Body).Decode(&public))
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, hostUUID, public.HostUUID)
	assert.Nil(t, public.Peer)
}

// nodeInfoTestProcess is the test's broker-side connection to a real child.
type nodeInfoTestProcess struct {
	stdin        io.Writer
	codec        *jsonrpc.Codec
	hostUUID     string
	port         int
	inventoryURL string
}

// startNodeInfoForTest builds and starts node-info on an available port.
// It registers cleanup and bounds the child's lifetime so stalled reads end.
func startNodeInfoForTest(t *testing.T, hostUUID string, extraArgs ...string) *nodeInfoTestProcess {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nvpair-node-info")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	build := exec.Command("go", "build", "-o", path, ".")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build node-info: %s", output)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, portString, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, listener.Close())
	port, err := strconv.Atoi(portString)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	args := append([]string{"--node-id", hostUUID, "--port", portString}, extraArgs...)
	cmd := exec.CommandContext(ctx, path, args...)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		assert.NoError(t, cmd.Wait())
	})
	return &nodeInfoTestProcess{
		stdin:        stdin,
		codec:        jsonrpc.NewCodec(readWritePair{Reader: stdout, Writer: stdin}),
		hostUUID:     hostUUID,
		port:         port,
		inventoryURL: "http://127.0.0.1:" + portString + "/v1/node-info",
	}
}

// nextSnapshot skips unrelated messages and decodes the next inventory update.
// Every returned snapshot must report the identity assigned to the child.
func (process *nodeInfoTestProcess) nextSnapshot(t *testing.T) noderec.NodeInfo {
	t.Helper()
	for {
		msg, err := process.codec.Read()
		require.NoError(t, err)
		if msg.Method != noderec.NotifyInfoUpdated {
			continue
		}
		var params noderec.LocalNodeInfoParams
		require.NoError(t, json.Unmarshal(msg.Params, &params))
		require.Equal(t, process.hostUUID, params.Info.HostUUID)
		return params.Info
	}
}

// requestPeerMetadata sends metadata and waits for its matching acknowledgement,
// skipping periodic notifications that arrive while the request is in flight.
func (process *nodeInfoTestProcess) requestPeerMetadata(t *testing.T, metadata noderec.PeerMetadataParams) metadataAcknowledgement {
	t.Helper()
	id := json.RawMessage("1")
	params, err := json.Marshal(metadata)
	require.NoError(t, err)
	request := jsonrpc.Message{JSONRPC: "2.0", ID: &id, Method: noderec.MethodSetPeerMetadata, Params: params}
	require.NoError(t, json.NewEncoder(process.stdin).Encode(request))
	for {
		msg, err := process.codec.Read()
		require.NoError(t, err)
		if msg.ID == nil || string(*msg.ID) != string(id) {
			continue
		}
		require.Nil(t, msg.Error)
		var acknowledgement metadataAcknowledgement
		require.NoError(t, json.Unmarshal(msg.Result, &acknowledgement))
		return acknowledgement
	}
}

type readWritePair struct {
	io.Reader
	io.Writer
}

type metadataAcknowledgement struct {
	OK bool `json:"ok"`
}

func TestInfoUpdates_PublishesStartupAndPeriodicSnapshots(t *testing.T) {
	// Drive startup and one controlled tick to verify unsolicited publication
	// without waiting for a real timer or making any HTTP requests.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	var output bytes.Buffer
	var calls int
	snapshot := func() NodeInfoResponse {
		calls++
		if calls == 2 {
			cancel()
		}
		return buildResponse(nil, nil, 0, statsSnapshot{}, "owned-host", nil)
	}
	publishInfoUpdates(ctx, applog.NewNotifier(&output), snapshot, ticks)
	decoder := json.NewDecoder(&output)
	for i := 0; i < 2; i++ {
		var frame jsonrpc.Message
		require.NoError(t, decoder.Decode(&frame), "snapshot %d", i)
		var params noderec.LocalNodeInfoParams
		require.NoError(t, json.Unmarshal(frame.Params, &params))
		assert.Equal(t, noderec.NotifyInfoUpdated, frame.Method, "snapshot %d", i)
		assert.Equal(t, "owned-host", params.Info.HostUUID, "snapshot %d", i)
		assert.False(t, params.Info.TelemetryValid, "snapshot %d", i)
	}
	assert.Equal(t, 2, calls)
	assert.False(t, decoder.More(), "only startup and one tick should publish")
}

func TestPeerMetadata_ReplacesTheCompleteSnapshot(t *testing.T) {
	// Apply successive registry snapshots through the stdin handler and verify
	// acknowledgement and stored contents, including removal of obsolete fields.
	var metadata peerMetadata
	// Send one request and compare its acknowledged snapshot with stored state.
	apply := func(params noderec.PeerMetadataParams) {
		t.Helper()
		body, err := json.Marshal(params)
		require.NoError(t, err)
		var output bytes.Buffer
		handlePeerMetadata(applog.StdinMessage{ID: json.RawMessage(`1`), Method: noderec.MethodSetPeerMetadata, Params: body}, "owned-host", &metadata, applog.NewNotifier(&output))
		var response jsonrpc.Message
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response))
		var acknowledgement metadataAcknowledgement
		require.NoError(t, json.Unmarshal(response.Result, &acknowledgement))
		require.Nil(t, response.Error)
		require.True(t, acknowledgement.OK)
		got, known := metadata.get()
		require.True(t, known)
		assert.Equal(t, params, got)
	}
	apply(noderec.PeerMetadataParams{
		HostUUID: "owned-host", ClusterUUID: "principal",
		Peer: noderec.PeerMetadata{Name: "old-name", Addresses: []string{"192.0.2.1"}, Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: 14318, noderec.ServiceOllama: 11434}},
	})
	apply(noderec.PeerMetadataParams{
		HostUUID: "owned-host",
		Peer:     noderec.PeerMetadata{Name: "new-name", Addresses: []string{"192.0.2.2"}, Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: 14318}},
	})
}

func TestPeerMetadata_RejectsMalformedOrForeignIdentity(t *testing.T) {
	// Invalid requests must return an invalid-params error and leave the worker
	// without registry metadata that it could later serve as its own.
	test := func(name, params string) {
		t.Run(name, func(t *testing.T) {
			var metadata peerMetadata
			var output bytes.Buffer
			handlePeerMetadata(applog.StdinMessage{ID: json.RawMessage(`2`), Method: noderec.MethodSetPeerMetadata, Params: json.RawMessage(params)}, "owned-host", &metadata, applog.NewNotifier(&output))
			var response jsonrpc.Message
			require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response))
			require.NotNil(t, response.Error)
			assert.Equal(t, -32602, response.Error.Code)
			_, known := metadata.get()
			assert.False(t, known, "rejected metadata must not be stored")
		})
	}
	test("malformed object", `"invalid"`)
	test("missing host", `{"peer":{"name":"copied-name"}}`)
	test("foreign host", `{"hostUuid":"someone-else","peer":{"name":"copied-name"}}`)
}

type pairedNodeInfoProcess struct {
	process    *nodeInfoTestProcess
	clusterDir string
	client     *http.Client
	peerURL    string
}

func startPairedNodeInfoForTest(t *testing.T) pairedNodeInfoProcess {
	t.Helper()
	serverCert, serverKey := genLeaf(t, "uuid-a")
	clientCert, clientKey := genLeaf(t, "uuid-b")
	serverDir := setupNode(t, serverCert, serverKey, map[string][]byte{"uuid-b": clientCert})
	// Keep membership active when the client's pin alone is removed or replaced.
	require.NoError(t, os.WriteFile(filepath.Join(serverDir, "admission.json"), []byte(`{"clusterId":"test-cluster","epoch":1}`), 0o600))
	clientDir := setupNode(t, clientCert, clientKey, map[string][]byte{"uuid-a": serverCert})
	cfg, ok := clustertrust.Open(clientDir).ClientTLSConfig("uuid-a")
	require.True(t, ok, "pinned client configuration must be available")
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	t.Cleanup(client.CloseIdleConnections)
	process := startNodeInfoForTest(t, "uuid-a", "--cluster-dir", serverDir)
	process.nextSnapshot(t) // startup has bound both listener personalities
	return pairedNodeInfoProcess{process: process, clusterDir: serverDir, client: client, peerURL: strings.Replace(process.inventoryURL, "http://", "https://", 1)}
}

func nodeInfoMetadataForTest(port int) noderec.PeerMetadataParams {
	return noderec.PeerMetadataParams{
		HostUUID: "uuid-a", ClusterUUID: "uuid-a",
		Peer: noderec.PeerMetadata{
			Name: "workstation", Addresses: []string{"192.0.2.20"},
			Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: port, noderec.ServiceOllama: 11434},
		},
	}
}

type nodeInfoTestInventory struct {
	NodeInfoResponse
	Peer *noderec.PeerMetadata `json:"peer,omitempty"`
}

type nodeInfoHTTPResult struct {
	status    int
	inventory nodeInfoTestInventory
	reused    bool
}

func getNodeInfoForTest(t *testing.T, client *http.Client, endpoint string) nodeInfoHTTPResult {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	var reused bool
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}
	response, err := client.Do(request.WithContext(httptrace.WithClientTrace(request.Context(), trace)))
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	result := nodeInfoHTTPResult{status: response.StatusCode, reused: reused}
	if response.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &result.inventory))
	}
	return result
}

func TestNodeInfo_PublicInventoryRemainsAvailableWhileClustered(t *testing.T) {
	// Verify that a clustered node still serves public hardware over HTTP while
	// omitting the peer registry metadata served by the mTLS handler.
	paired := startPairedNodeInfoForTest(t)
	public := &http.Client{Timeout: 3 * time.Second}
	t.Cleanup(public.CloseIdleConnections)
	result := getNodeInfoForTest(t, public, paired.process.inventoryURL)
	assert.Equal(t, http.StatusOK, result.status)
	assert.Equal(t, "uuid-a", result.inventory.HostUUID)
	assert.Nil(t, result.inventory.Peer)
}

func TestNodeInfo_PeerNodeInventoryServesCompleteMatchingNodeDetails(t *testing.T) {
	// Verify that a pinned client receives inventory with complete registry
	// details matching the serving node's certificate identity.
	paired := startPairedNodeInfoForTest(t)
	metadata := nodeInfoMetadataForTest(paired.process.port)
	paired.process.requestPeerMetadata(t, metadata)
	result := getNodeInfoForTest(t, paired.client, paired.peerURL)
	require.Equal(t, http.StatusOK, result.status)
	assert.Equal(t, "uuid-a", result.inventory.HostUUID)
	require.NotNil(t, result.inventory.ClusterUUID)
	assert.Equal(t, "uuid-a", *result.inventory.ClusterUUID)
	assert.Equal(t, &metadata.Peer, result.inventory.Peer)
}

func TestNodeInfo_PeerNodeInventoryRejectsIncompleteOrMismatchedNodeDetails(t *testing.T) {
	// Verify 503 before any node details arrive and when replacement details
	// are incomplete or identify a different node.
	paired := startPairedNodeInfoForTest(t)
	assert.Equal(t, http.StatusServiceUnavailable, getNodeInfoForTest(t, paired.client, paired.peerURL).status)
	reject := func(name string, incomplete noderec.PeerMetadataParams) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			// Replace a complete snapshot so each case also rejects stale details
			// left over from a previously valid state.
			paired.process.requestPeerMetadata(t, nodeInfoMetadataForTest(paired.process.port))
			paired.process.requestPeerMetadata(t, incomplete)
			assert.Equal(t, http.StatusServiceUnavailable, getNodeInfoForTest(t, paired.client, paired.peerURL).status)
		})
	}
	principal := nodeInfoMetadataForTest(paired.process.port)
	principal.ClusterUUID = "different-principal"
	reject("mismatched node identity", principal)

	name := nodeInfoMetadataForTest(paired.process.port)
	name.Peer.Name = ""
	reject("missing name", name)

	addresses := nodeInfoMetadataForTest(paired.process.port)
	addresses.Peer.Addresses = nil
	reject("missing addresses", addresses)

	service := nodeInfoMetadataForTest(paired.process.port)
	delete(service.Peer.Services, noderec.ServiceNodeInfo)
	reject("missing node-info registration", service)
}

func TestNodeInfo_RechecksTrustOnReusedConnections(t *testing.T) {
	// Reuse a TLS connection after pin removal, replacement, or local departure.
	// The next inventory request must be forbidden while public HTTP still
	// serves hardware without a peer object.
	test := func(name string, changeTrust func(*testing.T, string)) {
		t.Run(name, func(t *testing.T) {
			paired := startPairedNodeInfoForTest(t)
			paired.process.requestPeerMetadata(t, nodeInfoMetadataForTest(paired.process.port))
			require.Equal(t, http.StatusOK, getNodeInfoForTest(t, paired.client, paired.peerURL).status)
			changeTrust(t, paired.clusterDir)
			result := getNodeInfoForTest(t, paired.client, paired.peerURL)
			assert.True(t, result.reused)
			assert.Equal(t, http.StatusForbidden, result.status)
			public := &http.Client{Timeout: 3 * time.Second}
			t.Cleanup(public.CloseIdleConnections)
			result = getNodeInfoForTest(t, public, paired.process.inventoryURL)
			assert.Equal(t, http.StatusOK, result.status)
			assert.Nil(t, result.inventory.Peer)
		})
	}
	removePin := func(t *testing.T, dir string) {
		t.Helper()
		require.NoError(t, os.Remove(filepath.Join(dir, "trusted", "uuid-b.json")))
	}
	test("pin removal", removePin)
	test("pin replacement with the same UUID", func(t *testing.T, dir string) {
		t.Helper()
		cert, _ := genLeaf(t, "uuid-b")
		clustertrusttest.WritePeerPinFromCertificate(t, dir, "uuid-b", cert)
	})
	test("local departure retaining the keypair", func(t *testing.T, dir string) {
		t.Helper()
		removePin(t, dir)
		require.NoError(t, os.Remove(filepath.Join(dir, "admission.json")))
	})
}

func TestNodeInfoHandler_RejectsInventoryIfTrustChangesDuringCollection(t *testing.T) {
	// Revoke the client's pin during response collection and verify that the
	// in-flight request is rejected instead of serving the completed inventory.
	serverCert, serverKey := genLeaf(t, "uuid-a")
	clientCert, clientKey := genLeaf(t, "uuid-b")
	serverDir := setupNode(t, serverCert, serverKey, map[string][]byte{"uuid-b": clientCert})
	clientDir := setupNode(t, clientCert, clientKey, map[string][]byte{"uuid-a": serverCert})
	mesh := clustertrust.Open(serverDir)
	clientConfig, ok := clustertrust.Open(clientDir).ClientTLSConfig("uuid-a")
	require.True(t, ok, "client certificate fixture must be available")
	require.NotEmpty(t, clientConfig.Certificates)
	require.NotNil(t, clientConfig.Certificates[0].Leaf)
	metadata := &peerMetadata{value: nodeInfoMetadataForTest(14318)}
	// Call the handler synchronously with the authenticated client's TLS state
	// so trust can be changed deterministically while it builds the response.
	request := httptest.NewRequest(http.MethodGet, "https://localhost/v1/node-info", nil)
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{clientConfig.Certificates[0].Leaf}}
	principal := "uuid-a"
	snapshot := func() NodeInfoResponse {
		require.NoError(t, os.Remove(filepath.Join(serverDir, "trusted", "uuid-b.json")))
		return buildResponse(nil, nil, 0, statsSnapshot{}, principal, &principal)
	}
	response := httptest.NewRecorder()
	nodeInfoHandler(mesh, metadata, snapshot)(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code)
}
