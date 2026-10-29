// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

// codecNop is a throwaway io.ReadWriter for a Manager whose broker side is
// never exercised.
type codecNop struct{}

func (codecNop) Read([]byte) (int, error)    { return 0, io.EOF }
func (codecNop) Write(p []byte) (int, error) { return len(p), nil }

type jSequence struct {
	Seq int `json:"seq"`
}

type jParams struct {
	Params jSequence `json:"params"`
}

func assertFrame(t *testing.T, body []byte, want int) {
	t.Helper()
	var frame jParams
	require.NoError(t, json.Unmarshal(body, &frame), "decode frame %d", want)
	require.Equal(t, want, frame.Params.Seq, "frame sequence")
}

func newBroadcastManagerForPeer(t *testing.T, selfDir string, peer *httptest.Server) *Manager {
	t.Helper()
	host, portStr, err := net.SplitHostPort(peer.Listener.Addr().String())
	require.NoError(t, err, "split test peer address")
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err, "parse test peer port")

	m := NewManager(NewCodec(codecNop{}), 0, "uuid-self", selfDir)
	m.peers.Replace([]PeerNode{{
		ID: "peer-1", Addresses: []string{host}, Port: port,
		TXT: []string{"cluster-uuid=uuid-peer"},
	}})
	return m
}

// TestManager_BroadcastPreservesEnqueueOrder checks that frames queued in order
// reach a peer in the same order over the cluster-mTLS broadcast path.
func TestManager_BroadcastPreservesEnqueueOrder(t *testing.T) {
	selfDir, peerDir := newPinnedPeerDirs(t)
	peerMesh := clustertrust.Open(peerDir)

	const frameCount = 25
	received := make(chan []byte, frameCount)
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "failed to read frame", http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = peerMesh.ServerTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	m := newBroadcastManagerForPeer(t, selfDir, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go m.broadcastLoop(ctx)

	for i := 0; i < frameCount; i++ {
		m.broadcastFrame("workload:started", []byte(fmt.Sprintf(`{"seq":%d}`, i)))
	}

	for want := 0; want < frameCount; want++ {
		select {
		case body := <-received:
			assertFrame(t, body, want)
		case <-ctx.Done():
			require.FailNow(t, "peer did not receive every frame", "%d of %d frames: %v", want, frameCount, ctx.Err())
		}
	}
}

// snapshotPauseHandler stops broadcastSnapshot after it copies activeLocal but
// before it queues the copied frames. This makes the remove race reproducible.
type snapshotPauseHandler struct {
	paused  chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (*snapshotPauseHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *snapshotPauseHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "snapshot copied before removal" {
		h.once.Do(func() { close(h.paused) })
		<-h.release
	}
	return nil
}

func (h *snapshotPauseHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *snapshotPauseHandler) WithGroup(string) slog.Handler      { return h }

func TestManager_SnapshotCannotResurrectRemovedWorkload(t *testing.T) {
	m := &Manager{
		activeLocal: make(map[workloadKey]workloadEvent),
		broadcastCh: make(chan []byte, 2),
		peers:       newPeerSet(0),
	}
	workload := &Workload{
		ID: "7", Model: "m", Engine: "ollama", RunID: "r1",
		State: StateRunning, OriginatedFrom: "node-a", CreatedAt: 1,
	}
	lifecycle, err := json.Marshal(lifecycleParams{WorkloadInfo: workload})
	require.NoError(t, err, "marshal lifecycle")
	m.trackActive(workloadKey{origin: "node-a", engine: "ollama", runID: "r1", id: "7"}, MethodStarted, lifecycle, StateRunning)

	paused := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSnapshot := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSnapshot()
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(&snapshotPauseHandler{paused: paused, release: release}))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	snapshotDone := make(chan struct{})
	go func() {
		m.broadcastSnapshot("snapshot copied before removal")
		close(snapshotDone)
	}()
	select {
	case <-paused:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "snapshot did not pause after copying the workload")
	}

	removal, err := json.Marshal(removeParams{WorkloadID: "7", OriginatedFrom: "node-a"})
	require.NoError(t, err, "marshal removal")
	removeDone := make(chan struct{})
	go func() {
		m.handleLocalRemove(&Message{Method: MethodRemove, Params: removal})
		close(removeDone)
	}()
	// The current implementation queues the removal while the snapshot is
	// paused. An implementation that serializes both operations may instead
	// hold the removal until the snapshot resumes.
	select {
	case <-removeDone:
	case <-time.After(100 * time.Millisecond):
	}
	releaseSnapshot()
	for _, done := range []<-chan struct{}{snapshotDone, removeDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			require.FailNow(t, "snapshot or removal did not finish")
		}
	}

	// The peer already has the running workload. Apply the queued frames in
	// their actual delivery order through the receiver's handlers.
	present := true
	removes := 0
	receiver := NewServer(0, newDedupIndex(8), nil,
		func(*Workload) error { present = true; return nil },
		func(string, string) error { present = false; removes++; return nil },
	)
	for len(m.broadcastCh) > 0 {
		var frame Message
		require.NoError(t, json.Unmarshal(<-m.broadcastCh, &frame), "decode queued frame")
		response := httptest.NewRecorder()
		switch frame.Method {
		case MethodStarted:
			receiver.handleLifecycle(response, &frame)
		case MethodRemove:
			receiver.handleRemove(response, &frame)
		default:
			require.FailNow(t, "unexpected queued method", "%q", frame.Method)
		}
		require.Equal(t, http.StatusOK, response.Code, "peer handling %s", frame.Method)
	}
	assert.Equal(t, 1, removes, "peer must receive one removal")
	assert.False(t, present, "stale snapshot must not resurrect the workload")
}
