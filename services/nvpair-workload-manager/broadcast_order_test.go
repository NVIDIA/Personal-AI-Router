// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
)

// codecNop is a throwaway io.ReadWriter for a Manager whose broker side is
// never exercised.
type codecNop struct{}

func (codecNop) Read([]byte) (int, error)    { return 0, io.EOF }
func (codecNop) Write(p []byte) (int, error) { return len(p), nil }

// TestBroadcastPreservesOriginOrder: frames must reach the peer in the order
// the read loop produced them. The old code fanned each frame out in its own
// goroutine, so a remove could overtake the lifecycle upsert it followed and
// the late upsert resurrected a ghost workload on the peer (phantom pending
// load in the scheduler's view). The ordered broadcast queue fixes this; the
// test drives 25 frames through the real cluster-mTLS broadcast path and
// asserts arrival order.
func TestBroadcastPreservesOriginOrder(t *testing.T) {
	selfCert, selfKey := genLeaf(t, "uuid-self")
	peerCert, peerKey := genLeaf(t, "uuid-peer")
	selfDir := setupNode(t, selfCert, selfKey, map[string][]byte{"uuid-peer": peerCert})
	peerMesh := clustertrust.Open(setupNode(t, peerCert, peerKey, map[string][]byte{"uuid-self": selfCert}))

	var mu sync.Mutex
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc(eventsPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = r.Body.Close()
		mu.Lock()
		got = append(got, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = peerMesh.ServerTLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	m := NewManager(NewCodec(codecNop{}), 0, "uuid-self", selfDir)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split test server hostport: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	m.peers.Replace([]PeerNode{{
		ID: "peer-1", Addresses: []string{host}, Port: port,
		TXT: []string{"cluster-uuid=uuid-peer"},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.broadcastLoop(ctx)

	const n = 25
	for i := 0; i < n; i++ {
		m.broadcastFrame("workload:started", []byte(fmt.Sprintf(`{"seq":%d}`, i)))
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		l := len(got)
		mu.Unlock()
		if l >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		t.Fatalf("peer received %d of %d frames", len(got), n)
	}
	for i, f := range got {
		if want := fmt.Sprintf(`"seq":%d`, i); !strings.Contains(f, want) {
			t.Fatalf("frame %d arrived out of order: %s", i, f)
		}
	}
}
