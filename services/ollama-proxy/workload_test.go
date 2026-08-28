// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recRW is a thread-safe io.ReadWriter that records everything the codec writes
// (newline-delimited JSON-RPC frames) so a test can assert which notifications
// were emitted. Reads hit EOF immediately.
type recRW struct {
	mu sync.Mutex
	b  []byte
}

func (r *recRW) Read([]byte) (int, error) { return 0, io.EOF }

func (r *recRW) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b = append(r.b, p...)
	return len(p), nil
}

func (r *recRW) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Contains(string(r.b), s)
}

// TestHandleHTTP_WorkloadStartedAtForward is a regression test: a job's
// workload:started must be emitted when the request is FORWARDED, not when the
// upstream first responds. Ollama serializes inference on a single GPU, so a
// burst of parallel submissions is forwarded concurrently but streams back one
// at a time; emitting at first byte made the queued submissions invisible
// (they appeared one card at a time). Here the upstream accepts the connection
// but never sends a byte until released, so a first-byte emission could never
// fire — yet workload:started must already be on the wire.
//
// proxy/request-started intentionally stays at the commit point (so it names
// the node that actually served after any failover); only workload:started
// moves to forward time, so this test asserts only that.
func TestHandleHTTP_WorkloadStartedAtForward(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-release // no response byte until the test releases us
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	// Deferred order matters (LIFO): doRelease runs BEFORE slow.Close so the
	// blocked upstream handler is unblocked before the server is torn down —
	// otherwise Close hangs on the active connection, including on t.Fatal.
	defer slow.Close()
	defer doRelease()

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "busy-node", slow.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 11434)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama"}`)))
	}()

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the forwarded request")
	}

	// The upstream has the request but has sent no response byte, so a
	// first-byte emission could not have fired. workload:started must still be
	// on the wire — that is the forward-time emission.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !rec.has("workload:started") {
		time.Sleep(5 * time.Millisecond)
	}
	if !rec.has("workload:started") {
		t.Fatal("workload:started not emitted at forward time")
	}

	doRelease()
	<-done
}
