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

// The worker calculates GPU age before writing stdout. If the broker stops
// draining that pipe, receipt must include the wait rather than revive an old
// reading as fresh enough for scheduling.
func TestLocalNodeInfo_IncludesWorkerPipeWaitInGPUAge(t *testing.T) {
	broker := &Broker{nodeID: "owned-host"}
	receivedAt := time.UnixMilli(1794500000000)
	params, err := json.Marshal(noderec.LocalNodeInfoParams{
		Info:       noderec.NodeInfo{HostUUID: "owned-host", TelemetryValid: true, MSSince: 137},
		CapturedAt: receivedAt.Add(-12 * time.Second).UnixMilli(),
	})
	require.NoError(t, err)

	broker.acceptLocalInfo(params, receivedAt)

	require.NotNil(t, broker.localInfo)
	assert.Equal(t, int64(12137), broker.localInfo.info.MSSince)
	assert.Equal(t, receivedAt.UnixMilli(), broker.localInfo.info.ObservedAt)
}

func TestLocalNodeInfo_RelaysOwnedWorkerSamples(t *testing.T) {
	// Follow an owned worker snapshot through the broker's notification handler
	// and scanner relay. Hardware values, identity, validity, and sample age
	// must survive that handoff.
	b := &Broker{nodeID: "owned-host"}
	sp, messages := localInfoScanner(t)
	b.setScanner(sp)
	beforeReceipt := time.Now()
	b.nodeInfoNotificationHandler(0)(noderec.NotifyInfoUpdated, localInfoPayload(t, noderec.NodeInfo{
		HostUUID: "owned-host", GPUs: []GPUInfo{{Name: "GPU", UtilizationPercent: 61}},
		TelemetryValid: true, MSSince: 137, ObservedAt: 1,
	}, beforeReceipt))
	info := readLocalInfo(t, messages)
	afterReceipt := time.Now()
	assert.Equal(t, "owned-host", info.HostUUID)
	assert.True(t, info.TelemetryValid)
	assert.GreaterOrEqual(t, info.MSSince, int64(137))
	assert.GreaterOrEqual(t, info.ObservedAt, beforeReceipt.UnixMilli(), "the broker replaces the worker's supplied display time")
	assert.LessOrEqual(t, info.ObservedAt, afterReceipt.UnixMilli())
	require.Len(t, info.GPUs, 1)
	assert.Equal(t, uint32(61), info.GPUs[0].UtilizationPercent)
}

func TestLocalNodeInfo_ReplayPreservesObservationTimeAndGPUAge(t *testing.T) {
	// Simulate a snapshot waiting in the broker cache while scanner is absent.
	// Replaying it must age the GPU sample without giving the display observation
	// a new timestamp when scanner restarts.
	b := &Broker{nodeID: "owned-host"}
	// Match the private wire clock's millisecond precision for exact age bounds.
	receivedAt := time.Now().Add(-3 * time.Second).Truncate(time.Millisecond)
	b.acceptLocalInfo(localInfoPayload(t, noderec.NodeInfo{HostUUID: "owned-host", TelemetryValid: true, MSSince: 137}, receivedAt), receivedAt)
	sp, messages := localInfoScanner(t)
	beforeReplay := time.Now()
	b.replayLocalInfo(sp)
	info := readLocalInfo(t, messages)
	afterReplay := time.Now()
	assert.GreaterOrEqual(t, info.MSSince, int64(137)+beforeReplay.Sub(receivedAt).Milliseconds())
	assert.LessOrEqual(t, info.MSSince, int64(137)+afterReplay.Sub(receivedAt).Milliseconds())

	// Scanner returns the cached observation later. Its second broker receipt
	// must preserve the display time assigned before the restart.
	store := newDiscoveryStore()
	store.Upsert(EnrichedNode{
		HostUUID: "owned-host", DiscoveryRecordID: "local:owned-host", IdentityVerification: noderec.IdentityLocal,
	}, sourceScanner)
	accepted, ok := store.AcceptInfo(noderec.NodeInfoEvent{
		HostUUID: "owned-host", DiscoveryRecordID: "local:owned-host", IdentityVerification: noderec.IdentityLocal, Info: info,
	}, time.Now())
	require.True(t, ok)
	assert.Equal(t, receivedAt.UnixMilli(), accepted.Info.ObservedAt)
}

func TestLocalNodeInfo_RejectsForeignWorkerIdentity(t *testing.T) {
	// A snapshot naming another host must not enter the locally owned sample
	// cache, even when it reports valid telemetry.
	b := &Broker{nodeID: "owned-host"}
	b.nodeInfoNotificationHandler(0)(noderec.NotifyInfoUpdated, localInfoPayload(t, noderec.NodeInfo{HostUUID: "someone-else", TelemetryValid: true}, time.Now()))
	assert.Nil(t, b.localInfo)
}

func TestNodeInfoRestart_RejectsSnapshotsFromPreviousWorker(t *testing.T) {
	b := &Broker{nodeID: "owned-host"}
	previous := b.nodeInfoNotificationHandler(b.nodeInfoGeneration)
	previous(noderec.NotifyInfoUpdated, localInfoPayload(t, noderec.NodeInfo{HostUUID: "owned-host", TelemetryValid: true, MSSince: 137}, time.Now()))
	require.NotNil(t, b.localInfo)
	retained := *b.localInfo

	b.clearNodeInfoState()
	previous(noderec.NotifyInfoUpdated, localInfoPayload(t, noderec.NodeInfo{HostUUID: "owned-host", TelemetryValid: true}, time.Now()))

	assert.Equal(t, retained, *b.localInfo)
	replacement := noderec.NodeInfo{HostUUID: "owned-host", CPU: &CPUInfo{UtilizationPercent: 42}}
	b.nodeInfoNotificationHandler(b.nodeInfoGeneration)(noderec.NotifyInfoUpdated, localInfoPayload(t, replacement, time.Now()))
	assert.Equal(t, replacement.CPU, b.localInfo.info.CPU)
	assert.False(t, b.localInfo.info.TelemetryValid)
}

func localInfoPayload(t *testing.T, info noderec.NodeInfo, capturedAt time.Time) json.RawMessage {
	t.Helper()
	params, err := json.Marshal(noderec.LocalNodeInfoParams{Info: info, CapturedAt: capturedAt.UnixMilli()})
	require.NoError(t, err)
	return params
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
		assert.Positive(t, params.CapturedAt)
		return params.InfoAt(time.Now())
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
	onWrite func()
}

func (w *blockedMetadataWriter) Write(data []byte) (int, error) {
	w.onWrite()
	return w.Conn.Write(data)
}

type heldMetadataWriter struct {
	frames     chan []byte
	startWrite func()
	release    func()
}

func (w *heldMetadataWriter) Write(data []byte) (int, error) {
	w.startWrite()
	w.frames <- append([]byte(nil), data...)
	return len(data), nil
}

func (w *heldMetadataWriter) Close() error {
	w.release()
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
	started, released := make(chan struct{}), make(chan struct{})
	writer := &heldMetadataWriter{
		frames: make(chan []byte, updates+2),
		startWrite: sync.OnceFunc(func() {
			close(started)
			<-released
		}),
		release: sync.OnceFunc(func() { close(released) }),
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
	// The first frame is already encoded when this barrier closes. Keep its
	// write blocked while the registry advances to the latest pending snapshot.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "metadata write did not start")
	}
	updated := make(chan struct{})
	go func() {
		for i := 1; i <= updates; i++ {
			node.Name = fmt.Sprintf("updated-%d", i)
			b.updateLocalPeerMetadata(node)
		}
		node.IP = "192.0.2.21"
		node.ClusterUUID = ""
		delete(node.Services, noderec.ServiceOllama)
		b.updateLocalPeerMetadata(node)
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "a blocked node-info write stalled registry updates")
	}

	require.NoError(t, writer.Close())
	waitForMetadataSender(t, b)

	require.Len(t, writer.frames, 2, "only the in-flight and latest pending snapshots should be written")
	// The in-flight frame stays first; all intervening updates collapse into
	// the complete final frame, including removed membership and services.
	var initialFrame Message
	require.NoError(t, json.Unmarshal(<-writer.frames, &initialFrame))
	assert.Equal(t, noderec.MethodSetPeerMetadata, initialFrame.Method)
	var initial noderec.PeerMetadataParams
	require.NoError(t, json.Unmarshal(initialFrame.Params, &initial))
	assert.Equal(t, "initial", initial.Peer.Name)
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

func TestPeerMetadata_ReplaysLatestSnapshotAfterBlockedWorkerIsReplaced(t *testing.T) {
	// Closing the old stdin must release the sole sender so restart replay can
	// deliver updates that arrived while the retired worker was blocked.
	b := &Broker{nodeID: "owned-host"}
	client, server := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, client.Close())
		assert.NoError(t, server.Close())
	})
	started := make(chan struct{})
	writer := &blockedMetadataWriter{Conn: client, onWrite: sync.OnceFunc(func() { close(started) })}
	b.setNodeInfo(&nodeInfoProcess{stdin: writer})
	node := noderec.DirectoryNode{HostUUID: "owned-host", IdentityVerification: noderec.IdentityLocal, Name: "initial"}
	b.updateLocalPeerMetadata(node)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "metadata write did not start")
	}
	node.Name = "latest"
	b.updateLocalPeerMetadata(node)
	b.clearNodeInfoState()
	replacement := &nodeInfoFrameWriter{frames: make(chan []byte, 4)}
	b.setNodeInfo(&nodeInfoProcess{stdin: replacement})
	b.pushPeerMetadata()

	require.NoError(t, client.Close())
	waitForMetadataSender(t, b)

	require.Len(t, replacement.frames, 1)
	var frame Message
	require.NoError(t, json.Unmarshal(<-replacement.frames, &frame))
	var got noderec.PeerMetadataParams
	require.NoError(t, json.Unmarshal(frame.Params, &got))
	assert.Equal(t, "latest", got.Peer.Name)
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
	started := make(chan struct{})
	writer := &blockedMetadataWriter{Conn: client, onWrite: sync.OnceFunc(func() { close(started) })}
	b.setNodeInfo(&nodeInfoProcess{stdin: writer})
	node := noderec.DirectoryNode{HostUUID: "owned-host", IdentityVerification: noderec.IdentityLocal, Name: "initial"}
	b.updateLocalPeerMetadata(node)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "metadata write did not start")
	}
	node.Name = "pending"
	b.updateLocalPeerMetadata(node)

	require.NoError(t, client.Close())
	waitForMetadataSender(t, b)
}
