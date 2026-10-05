// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServer_InterNodeDedupRecordedOnlyAfterSuccessfulEmit verifies that a
// failed broker emit does not record a dedup key, so the peer can retry, while
// a successful emit prevents later duplicates from reaching the broker.
func TestServer_InterNodeDedupRecordedOnlyAfterSuccessfulEmit(t *testing.T) {
	test := func(name string, frame []byte, wantUpserts, wantRemoves int) {
		t.Run(name, func(t *testing.T) {
			self, peer := newPinnedPeerMeshes(t)
			var upserts, removes int
			failEmit := true
			srv := NewServer(0, newDedupIndex(100), self,
				func(*Workload) error {
					if failEmit {
						return errors.New("broker gone")
					}
					upserts++
					return nil
				},
				func(string, string) error {
					if failEmit {
						return errors.New("broker gone")
					}
					removes++
					return nil
				},
			)
			post := serveEventsOverMTLS(t, srv, self, peer)

			assert.Equal(t, http.StatusInternalServerError, post(frame), "failed emit")
			assert.Zero(t, upserts, "failed emit must not upsert")
			assert.Zero(t, removes, "failed emit must not remove")

			failEmit = false
			assert.Equal(t, http.StatusOK, post(frame), "retry")
			assert.Equal(t, wantUpserts, upserts, "upserts after retry")
			assert.Equal(t, wantRemoves, removes, "removes after retry")
			// they are duplicates at this point and shouldn't get sent to do work
			assert.Equal(t, http.StatusOK, post(frame), "duplicate")
			assert.Equal(t, wantUpserts, upserts, "duplicate must not upsert")
			assert.Equal(t, wantRemoves, removes, "duplicate must not remove")
		})
	}

	lifecycleFrame := []byte(`{"jsonrpc":"2.0","method":"workload:started","params":` +
		`{"workloadInfo":{"id":"7","model":"llama3","engine":"ollama",` +
		`"runId":"r1","state":"running","originatedFrom":"uuid-peer"}}}`)
	removeFrame := []byte(`{"jsonrpc":"2.0","method":"workloads:remove",` +
		`"params":{"workloadId":"7","originatedFrom":"uuid-peer"}}`)

	test("lifecycle upsert", lifecycleFrame, 1, 0)
	test("workload removal", removeFrame, 0, 1)
}

// TestServer_ConcurrentDuplicatesEmitOnce verifies that an in-flight event
// reserves its key until the broker emit finishes. Both paths share the same
// dedup index, but lifecycle and removal use distinct keys and emitters.
func TestServer_ConcurrentDuplicatesEmitOnce(t *testing.T) {
	test := func(name string, params []byte, handle func(*Server, http.ResponseWriter, *Message)) {
		t.Run(name, func(t *testing.T) {
			// Pause the first broker emit after it reserves the key. This gives
			// the identical second request a chance to reach the same handler.
			firstEntered := make(chan struct{})
			releaseFirst := make(chan struct{})
			// A failure before the explicit release must not strand that goroutine.
			defer func() {
				select {
				case <-releaseFirst:
				default:
					close(releaseFirst)
				}
			}()
			var emits atomic.Int32
			emit := func() error {
				if emits.Add(1) == 1 {
					close(firstEntered)
					<-releaseFirst
				}
				return nil
			}
			srv := NewServer(0, newDedupIndex(100), nil,
				func(*Workload) error { return emit() },
				func(string, string) error { return emit() },
			)
			msg := &Message{Params: params}
			post := func(result chan<- int) {
				recorder := httptest.NewRecorder()
				handle(srv, recorder, msg)
				result <- recorder.Code
			}
			firstDone := make(chan int, 1)
			secondDone := make(chan int, 1)
			go post(firstDone)
			// Wait until the first request is inside emit, then start its duplicate.
			select {
			case <-firstEntered:
			case <-time.After(2 * time.Second):
				require.FailNow(t, "first request did not reach the broker emit")
			}
			go post(secondDone)
			// The duplicate must wait for the first emit's result. The short
			// timeout gives it an opportunity to expose a premature response.
			select {
			case code := <-secondDone:
				require.FailNow(t, "duplicate completed before first emit", "HTTP %d", code)
			case <-time.After(50 * time.Millisecond):
			}
			// Once the first emit succeeds, both requests may return 200, but
			// only that first request should have called the broker emitter.
			close(releaseFirst)
			for _, result := range []<-chan int{firstDone, secondDone} {
				select {
				case code := <-result:
					assert.Equal(t, http.StatusOK, code)
				case <-time.After(2 * time.Second):
					require.FailNow(t, "request did not finish")
				}
			}
			assert.Equal(t, int32(1), emits.Load(), "broker must emit once")
		})
	}

	lifecycle := []byte(`{"workloadInfo":{"id":"7","model":"llama3","engine":"ollama","runId":"r1","state":"running","originatedFrom":"uuid-peer"}}`)
	remove := []byte(`{"workloadId":"7","originatedFrom":"uuid-peer"}`)
	test("lifecycle upsert", lifecycle, (*Server).handleLifecycle)
	test("workload removal", remove, (*Server).handleRemove)
}
