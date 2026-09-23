// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
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

// TestHandleHTTP_WorkloadVisibleBeforeFirstByte is a regression test: a job
// must be on the wire as soon as it is admitted, not when the upstream first
// responds. An engine serializes inference on a single GPU slot, so a burst of
// parallel submissions is dispatched concurrently but streams back one at a
// time; emitting only at first byte made the waiting submissions invisible and
// they appeared one card at a time. Here the upstream accepts the connection
// but never sends a byte until released, so a first-byte emission could not
// have fired — yet the job must already be visible.
//
// The event is workload:submitted, carrying state "queued". "running" is
// reserved for the commit point, when the engine is actually producing content:
// a job waiting its turn inside the engine is not running, and the broker's
// store rejects a backwards transition, so a job that claimed "running" at
// dispatch time could never return to "queued" for a retry.
//
// proxy/request-started also stays at the commit point, so it names the node
// that actually served after any failover.
func TestHandleHTTP_WorkloadVisibleBeforeFirstByte(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
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
		// Deferred order matters (LIFO): doRelease runs BEFORE slow.Close so
		// the blocked upstream handler is unblocked before the server is torn
		// down — otherwise Close hangs on the active connection, including on
		// t.Fatal.
		defer slow.Close()
		defer doRelease()

		rec := &recRW{}
		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "busy-node", slow.URL, tc.advertisedModel))
		p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

		done := make(chan struct{})
		go func() {
			defer close(done)
			p.soleFacade().handleHTTP(httptest.NewRecorder(), httptest.NewRequest(
				http.MethodPost, tc.inferencePath,
				strings.NewReader(fmt.Sprintf(`{"model":%q}`, tc.requestedModel))))
		}()

		select {
		case <-received:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream never received the forwarded request")
		}

		// The upstream has the request but has sent no response byte, so a
		// first-byte emission could not have fired. The job must still be
		// visible, as queued.
		if !waitFor(t, rec, "workload:submitted") {
			t.Fatal("workload:submitted not emitted when the request was admitted")
		}
		if !rec.has(`"state":"queued"`) {
			t.Fatal("an admitted job must be queued until the engine produces content")
		}
		if rec.has("workload:started") {
			t.Fatal("workload:started emitted before the engine produced any content")
		}

		doRelease()
		<-done

		// Released: the engine produced content, so the job is now running and
		// then completes. queued -> running -> completed, never backwards.
		if !waitFor(t, rec, "workload:started") {
			t.Fatal("workload:started not emitted once the engine produced content")
		}
		if !rec.has(`"state":"running"`) {
			t.Fatal("the commit point must transition the job to running")
		}
	})
}
