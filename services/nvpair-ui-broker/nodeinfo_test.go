// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestLocalNodeInfo_RelaysOwnedWorkerSamples(t *testing.T) {
	// Follow an owned worker snapshot through the broker's notification handler
	// and scanner relay. Hardware values, identity, validity, and sample age
	// must survive that handoff.
	b := &Broker{nodeID: "owned-host"}
	sp, messages := localInfoScanner(t)
	b.setScanner(sp)
	b.forwardNodeInfoNotification(noderec.NotifyInfoUpdated, json.RawMessage(`{"info":{"hostUuid":"owned-host","GPUs":[{"name":"GPU","utilization_percent":61}],"telemetryValid":true,"msSince":137}}`))
	info := readLocalInfo(t, messages)
	assert.Equal(t, "owned-host", info.HostUUID)
	assert.True(t, info.TelemetryValid)
	assert.GreaterOrEqual(t, info.MSSince, int64(137))
	require.Len(t, info.GPUs, 1)
	assert.Equal(t, uint32(61), info.GPUs[0].UtilizationPercent)
}

func TestLocalNodeInfo_ReplayPreservesSampleAge(t *testing.T) {
	// Simulate a snapshot waiting in the broker cache while scanner is absent.
	// Replaying it must include that wait in its age so an old measurement
	// cannot appear freshly sampled after scanner restarts.
	b := &Broker{nodeID: "owned-host"}
	b.forwardNodeInfoNotification(noderec.NotifyInfoUpdated, json.RawMessage(`{"info":{"hostUuid":"owned-host","telemetryValid":true,"msSince":137}}`))
	require.NotNil(t, b.localInfo)
	b.localInfo.receivedAt = time.Now().Add(-3 * time.Second)
	sp, messages := localInfoScanner(t)
	b.replayLocalInfo(sp)
	info := readLocalInfo(t, messages)
	assert.GreaterOrEqual(t, info.MSSince, int64(3137))
	assert.LessOrEqual(t, info.MSSince, int64(6000))
}

func TestLocalNodeInfo_RejectsForeignWorkerIdentity(t *testing.T) {
	// A snapshot naming another host must not enter the locally owned sample
	// cache, even when it reports valid telemetry.
	b := &Broker{nodeID: "owned-host"}
	b.forwardNodeInfoNotification(noderec.NotifyInfoUpdated, json.RawMessage(`{"info":{"hostUuid":"someone-else","telemetryValid":true}}`))
	assert.Nil(t, b.localInfo)
}

// localInfoScanner connects the broker's production relay to an in-memory
// scanner stand-in. It returns the relay handle and messages received at the
// scanner end, and registers cleanup to stop the relay and close both ends.
func localInfoScanner(t *testing.T) (*scannerProcess, <-chan *Message) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = client.Close()
		_ = server.Close()
	})
	sp := &scannerProcess{
		peer: NewPeer(NewCodec(client)), done: done,
		localInfo: make(chan localInfoObservation, 1),
	}
	go sp.drainNodeActivity()
	messages := make(chan *Message, 4)
	go func() {
		defer close(messages)
		codec := NewCodec(server)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			messages <- msg
		}
	}()
	return sp, messages
}

// readLocalInfo waits up to two seconds for a local inventory notification,
// checks its method, and decodes the hardware snapshot for the test to assert on.
func readLocalInfo(t *testing.T, messages <-chan *Message) noderec.NodeInfo {
	t.Helper()
	select {
	case msg := <-messages:
		require.NotNil(t, msg)
		require.Equal(t, noderec.MethodLocalNodeInfo, msg.Method)
		var params noderec.LocalNodeInfoParams
		require.NoError(t, json.Unmarshal(msg.Params, &params))
		return params.Info
	case <-time.After(2 * time.Second):
		require.FailNow(t, "owned snapshot did not reach the scanner")
		return noderec.NodeInfo{}
	}
}

type nodeInfoFrameWriter struct {
	frames chan []byte
}

func (w *nodeInfoFrameWriter) Write(data []byte) (int, error) {
	w.frames <- append([]byte(nil), data...)
	return len(data), nil
}

func (w *nodeInfoFrameWriter) Close() error { return nil }

func TestPeerMetadata_RelaysAndReplaysOnlyTheOwnedRegistry(t *testing.T) {
	// Only the owned local registry may supply node-info's peer metadata.
	// Verify that unverified discoveries are ignored, cached metadata is replayed
	// when node-info becomes available, and later snapshots withdraw membership
	// and removed services instead of retaining stale fields.
	b := &Broker{nodeID: "owned-host"}
	node := noderec.DirectoryNode{
		HostUUID: "owned-host", IdentityVerification: noderec.IdentityUnverified,
		Name: "copied-name", IP: "192.0.2.99",
	}
	b.updateLocalPeerMetadata(node)
	require.Nil(t, b.localPeerMetadata, "unverified discovery must not supply local peer metadata")
	node.IdentityVerification = noderec.IdentityLocal
	node.Name, node.IP, node.ClusterUUID = "workstation", "192.0.2.20", "principal"
	node.IPs = []string{"192.0.2.20", "192.0.2.21"}
	node.Services = map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14318}, noderec.ServiceOllama: {Port: 11434}}
	b.updateLocalPeerMetadata(node)
	writer := &nodeInfoFrameWriter{frames: make(chan []byte, 4)}
	b.setNodeInfo(&nodeInfoProcess{stdin: writer})
	b.pushPeerMetadata()
	// Check one frame sent to node-info against the complete expected metadata.
	// Reuse this for the initial replay and the later withdrawal of stale fields.
	check := func(want noderec.PeerMetadataParams) {
		t.Helper()
		select {
		case data := <-writer.frames:
			var msg Message
			require.NoError(t, json.Unmarshal(data, &msg))
			var got noderec.PeerMetadataParams
			require.NoError(t, json.Unmarshal(msg.Params, &got))
			assert.Equal(t, noderec.MethodSetPeerMetadata, msg.Method)
			assert.Equal(t, want, got)
		case <-time.After(2 * time.Second):
			require.FailNow(t, "local metadata did not reach node-info")
		}
	}
	want := noderec.PeerMetadataParams{HostUUID: "owned-host", ClusterUUID: "principal", Peer: noderec.PeerMetadata{Name: node.Name, Addresses: node.IPs, Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: 14318, noderec.ServiceOllama: 11434}}}
	check(want)
	delete(node.Services, noderec.ServiceOllama)
	node.ClusterUUID = ""
	b.updateLocalPeerMetadata(node)
	want.ClusterUUID = ""
	delete(want.Peer.Services, noderec.ServiceOllama)
	check(want)
}

type blockedMetadataWriter struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (w *blockedMetadataWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.Conn.Write(data)
}

func TestPeerMetadata_UpdatesDoNotWaitForBlockedWrites(t *testing.T) {
	b := &Broker{nodeID: "owned-host"}
	node := noderec.DirectoryNode{
		HostUUID: "owned-host", IdentityVerification: noderec.IdentityLocal,
		Name: "initial-name",
	}
	b.updateLocalPeerMetadata(node)
	// Withhold reads until the cache update returns, simulating a worker that
	// has stopped consuming its stdin.
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	writer := &blockedMetadataWriter{Conn: client, started: make(chan struct{})}
	b.setNodeInfo(&nodeInfoProcess{stdin: writer})
	go b.pushPeerMetadata()
	select {
	case <-writer.started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "metadata write did not start")
	}
	node.Name = "updated-name"
	updated := make(chan struct{})
	go func() {
		b.updateLocalPeerMetadata(node)
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "a blocked node-info write stalled the scanner notification handler")
	}
	// Reading releases the blocked write; both snapshots must arrive in order.
	require.NoError(t, server.SetReadDeadline(time.Now().Add(2*time.Second)))
	codec := NewCodec(server)
	checkName := func(want string) {
		t.Helper()
		msg, err := codec.Read()
		require.NoError(t, err)
		var params noderec.PeerMetadataParams
		require.NoError(t, json.Unmarshal(msg.Params, &params))
		assert.Equal(t, want, params.Peer.Name)
	}
	checkName("initial-name")
	checkName("updated-name")
}

type heldMetadataWriter struct {
	frames      chan []byte
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func (w *heldMetadataWriter) Write(data []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	w.frames <- append([]byte(nil), data...)
	return len(data), nil
}

func (w *heldMetadataWriter) Close() error {
	w.releaseOnce.Do(func() { close(w.release) })
	return nil
}

func waitForMetadataSender(t *testing.T, b *Broker) {
	t.Helper()
	// Join the asynchronous sender before inspecting all frames it produced.
	require.Eventually(t, func() bool {
		b.peerMetadataMu.Lock()
		defer b.peerMetadataMu.Unlock()
		return !b.peerMetadataSending
	}, 2*time.Second, time.Millisecond)
}

func TestPeerMetadata_CoalescesUpdatesWhileWorkerIsBlocked(t *testing.T) {
	// A stopped reader must retain only the latest complete pending registry
	// snapshot, rather than accumulating one sender and write per update.
	const updates = 20
	b := &Broker{nodeID: "owned-host"}
	writer := &heldMetadataWriter{
		frames: make(chan []byte, updates+2), started: make(chan struct{}), release: make(chan struct{}),
	}
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	b.setNodeInfo(&nodeInfoProcess{stdin: writer})
	node := noderec.DirectoryNode{
		HostUUID: "owned-host", IdentityVerification: noderec.IdentityLocal, Name: "initial",
		IP: "192.0.2.20", ClusterUUID: "principal",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: 14318}, noderec.ServiceOllama: {Port: 11434},
		},
	}
	b.updateLocalPeerMetadata(node)
	select {
	case <-writer.started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "metadata write did not start")
	}
	for i := 1; i <= updates; i++ {
		node.Name = fmt.Sprintf("updated-%d", i)
		b.updateLocalPeerMetadata(node)
	}
	node.IP = "192.0.2.21"
	node.ClusterUUID = ""
	delete(node.Services, noderec.ServiceOllama)
	b.updateLocalPeerMetadata(node)

	require.NoError(t, writer.Close())
	waitForMetadataSender(t, b)

	require.Len(t, writer.frames, 2, "only the in-flight and latest pending snapshots should be written")
	<-writer.frames // The in-flight snapshot was already encoded before updates.
	var frame Message
	require.NoError(t, json.Unmarshal(<-writer.frames, &frame))
	assert.Equal(t, noderec.MethodSetPeerMetadata, frame.Method)
	var got noderec.PeerMetadataParams
	require.NoError(t, json.Unmarshal(frame.Params, &got))
	assert.Equal(t, noderec.PeerMetadataParams{
		HostUUID: "owned-host",
		Peer: noderec.PeerMetadata{
			Name: "updated-20", Addresses: []string{"192.0.2.21"},
			Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: 14318},
		},
	}, got)
}

func TestPeerMetadata_ClosingWorkerPipeReleasesSender(t *testing.T) {
	// Shutdown closes stdin without taking its write lock, so a blocked metadata
	// write and its pending update must both finish rather than leave a sender.
	b := &Broker{nodeID: "owned-host"}
	client, server := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, server.Close())
	})
	writer := &blockedMetadataWriter{Conn: client, started: make(chan struct{})}
	b.setNodeInfo(&nodeInfoProcess{stdin: writer})
	node := noderec.DirectoryNode{HostUUID: "owned-host", IdentityVerification: noderec.IdentityLocal, Name: "initial"}
	b.updateLocalPeerMetadata(node)
	select {
	case <-writer.started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "metadata write did not start")
	}
	node.Name = "pending"
	b.updateLocalPeerMetadata(node)

	require.NoError(t, client.Close())
	waitForMetadataSender(t, b)
}
