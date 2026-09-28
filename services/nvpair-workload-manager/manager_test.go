// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

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
	if err := json.Unmarshal(body, &frame); err != nil {
		t.Fatalf("decode frame %d: %v", want, err)
	}
	if frame.Params.Seq != want {
		t.Fatalf("frame %d arrived with sequence %d", want, frame.Params.Seq)
	}
}

func newBroadcastManagerForPeer(t *testing.T, selfDir string, peer *httptest.Server) *Manager {
	t.Helper()
	host, portStr, err := net.SplitHostPort(peer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test peer address: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test peer port: %v", err)
	}

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
			t.Fatalf("peer received %d of %d frames: %v", want, frameCount, ctx.Err())
		}
	}
}
