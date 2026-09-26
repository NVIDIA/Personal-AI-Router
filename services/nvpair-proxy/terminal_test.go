// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Coverage for a workload always reaching a terminal state.
//
// A job card that never leaves "running" is worse than one that reports a
// failure: the UI has no way to retire it and the scheduler keeps counting it
// as pending. These drive the paths that unwind past the normal terminal
// emission and assert one is emitted anyway.
//
// Run for every engine because the handler is shared. It came in with
// llama.cpp, whose streaming exposed it, but nothing about it is llama-specific
// and the other two engines had the same gap.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A committed response whose upstream body is truncated makes ReverseProxy
// abort the handler with ErrAbortHandler, which unwinds past every terminal
// path in the request. The workload must still be finalized, exactly once,
// for every engine. How that terminal is classified is pinned separately by
// TestHandleHTTP_UpstreamDiesMidStream_EmitsTerminal: a stream that already
// committed a 2xx terminates as completed, so this test asserts delivery and
// cardinality, not the label.
func TestTruncatedUpstreamStillFinalizesTheWorkload(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// A declared length the body never satisfies, then a clean close.
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, "data: partial\n\n")
		}))
		defer upstream.Close()

		rec := &recRW{}
		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "truncating-node", upstream.URL, tc.advertisedModel))
		p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

		// Served through a real listener rather than a recorder: the abort is
		// raised by net/http while streaming a committed response, which a
		// ResponseRecorder never reaches.
		finished := make(chan struct{})
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(finished)
			p.soleFacade().handleHTTP(w, r)
		}))
		defer front.Close()

		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Post(front.URL+tc.inferencePath, "application/json",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true}`, tc.requestedModel)))
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if err == nil {
			t.Fatal("a truncated upstream was presented to the client as a complete response")
		}

		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("the handler never unwound after the upstream truncated")
		}

		terminals := func() int { return rec.count("workload:completed") + rec.count("workload:errored") }
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && terminals() == 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if got := terminals(); got != 1 {
			t.Fatalf("terminal workload events = %d, want exactly one after a truncated upstream", got)
		}
	})
}
