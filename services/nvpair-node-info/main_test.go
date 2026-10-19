// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/applog"
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
func startNodeInfoForTest(t *testing.T, hostUUID string) *nodeInfoTestProcess {
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
	cmd := exec.CommandContext(ctx, path, "--node-id", hostUUID, "--port", portString)
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
	body := func() []byte {
		calls++
		if calls == 2 {
			cancel()
		}
		return buildResponse(nil, nil, 0, statsSnapshot{}, "owned-host", nil)
	}
	publishInfoUpdates(ctx, applog.NewNotifier(&output), body, ticks)
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
	test("foreign host", `{"hostUuid":"someone-else","peer":{"name":"copied-name"}}`)
}
