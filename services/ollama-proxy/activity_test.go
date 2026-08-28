// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Response bytes arriving from a node's engine are the only liveness evidence
// that gets stronger as the node gets busier, which is exactly when discovery's
// probes start timing out and evicting it. These tests pin that the proxy
// actually raises it.

func waitFor(t *testing.T, rec *recRW, frame string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.has(frame) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestStreamedBytesReportNodeActivity is the point of the feature: the node that
// served the response is named to the broker so discovery can keep it.
func TestStreamedBytesReportNodeActivity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"done":true}`))
	}))
	defer upstream.Close()

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "serving-node", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 11434)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama"}`)))

	if !waitFor(t, rec, `"method":"node/activity"`) {
		t.Fatal("no node/activity was reported after the upstream streamed a response")
	}
	if !waitFor(t, rec, `"hostUuid":"serving-node"`) {
		t.Fatal("node/activity did not name the node that served the request")
	}
}

// A node that accepted the connection but never wrote a byte has proved nothing
// about being able to do work, so it must not be vouched for. This is the case
// that matters: an accept can succeed on a machine whose user space is wedged.
func TestNoActivityReportedWithoutUpstreamBytes(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }

	received := make(chan struct{}, 1)
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	// LIFO: unblock the handler before tearing the server down, or Close hangs
	// on the live connection.
	defer silent.Close()
	defer doRelease()

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "silent-node", silent.URL, "llama"))
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

	// Give the proxy the same window the positive test uses, so a report would
	// have landed by now if one were going to.
	time.Sleep(200 * time.Millisecond)
	if rec.has(`"method":"node/activity"`) {
		t.Fatal("activity was reported for a node that had not sent a single response byte")
	}

	doRelease()
	<-done
}

// A streaming generation writes hundreds of chunks. Reporting each one would put
// thousands of frames a minute on the broker pipe for no gain, since the scanner
// treats one report as good for a minute.
func TestRepeatedChunksAreCoalescedIntoOneReport(t *testing.T) {
	chunks := 40
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < chunks; i++ {
			_, _ = w.Write([]byte(`{"response":"x"}` + "\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer upstream.Close()

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "streaming-node", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 11434)

	p.handleHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama","stream":true}`)))

	if !waitFor(t, rec, `"method":"node/activity"`) {
		t.Fatal("a streamed response reported no activity at all")
	}
	if got := rec.count(`"method":"node/activity"`); got != 1 {
		t.Fatalf("%d activity reports for %d chunks; want 1 within the throttle interval", got, chunks)
	}
}
