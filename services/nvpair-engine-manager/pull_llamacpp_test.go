// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const llamaCPPPullTestModel = "owner/repo:Q4_K_M"

// The download belongs to the router, not to the SSE request. Only unload
// clears downloading, so disconnecting the stream alone cannot pass a test.
type llamaCPPPullFixture struct {
	ex          *Executor
	server      *httptest.Server
	started     chan struct{}
	endStream   chan struct{}
	downloading atomic.Bool
	unloads     atomic.Int32
	start       http.HandlerFunc
	inventory   http.HandlerFunc
	unload      http.HandlerFunc
	stream      http.HandlerFunc
}

func newLlamaCPPPullFixture(t *testing.T) *llamaCPPPullFixture {
	t.Helper()
	f := &llamaCPPPullFixture{started: make(chan struct{}), endStream: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/models/sse", func(w http.ResponseWriter, r *http.Request) {
		if f.stream != nil {
			f.stream(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprint(w, ": ready\n\n"); err != nil {
			t.Errorf("write SSE greeting: %v", err)
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush SSE greeting: %v", err)
			return
		}
		select {
		case <-r.Context().Done():
		case <-f.endStream:
		}
	})
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			close(f.started)
			if f.start != nil {
				f.start(w, r)
				return
			}
			f.downloading.Store(true)
			if _, err := fmt.Fprint(w, `{"success":true}`); err != nil {
				t.Errorf("write start response: %v", err)
			}
		case http.MethodGet:
			if f.inventory != nil {
				f.inventory(w, r)
				return
			}
			if _, err := fmt.Fprintf(w, `{"data":[{"id":%q,"status":{"value":"downloading"}},{"id":"other/model","status":{"value":"downloading"}}]}`, llamaCPPPullTestModel); err != nil {
				t.Errorf("write inventory: %v", err)
			}
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/models/unload", func(w http.ResponseWriter, r *http.Request) {
		f.unloads.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode stop request: %v", err)
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost || body.Model != llamaCPPPullTestModel {
			t.Errorf("stop request = %s %+v, want POST for %s", r.Method, body, llamaCPPPullTestModel)
			http.Error(w, "wrong download", http.StatusBadRequest)
			return
		}
		if f.unload != nil {
			f.unload(w, r)
			return
		}
		f.downloading.Store(false)
		if _, err := fmt.Fprint(w, `{"success":true}`); err != nil {
			t.Errorf("write stop response: %v", err)
		}
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	f.ex = newHTTPPullTestExecutor(t, f.server, Action{
		HTTP:             &ActionHTTP{Method: http.MethodPost, Path: "/models"},
		ProgressProtocol: pullProgressProtocolLlamaCPPModelsSSE,
	})
	return f
}

func (f *llamaCPPPullFixture) pull(ctx context.Context) error {
	_, err := f.ex.PullModelStream(ctx, "fake", llamaCPPPullTestModel, nil)
	return err
}

func waitLlamaCPPPullSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pull request")
	}
}

func TestLlamaCPPPullCancellationStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-f.started
		cancel()
	}()
	if err := f.pull(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pull error = %v, want cancellation", err)
	}
	if f.downloading.Load() || f.unloads.Load() != 1 {
		t.Fatalf("downloading=%t unloads=%d, want stopped with one unload", f.downloading.Load(), f.unloads.Load())
	}
}

func TestLlamaCPPPullInactivityStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	f.ex.pullProgressTimeout = 100 * time.Millisecond
	if err := f.pull(context.Background()); !errors.Is(err, errPullProgressTimeout) {
		t.Fatalf("pull error = %v, want inactivity timeout", err)
	}
	if f.downloading.Load() || f.unloads.Load() != 1 {
		t.Fatalf("downloading=%t unloads=%d, want stopped with one unload", f.downloading.Load(), f.unloads.Load())
	}
}

func TestLlamaCPPPullStreamEOFStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	go func() {
		<-f.started
		close(f.endStream)
	}()
	if err := f.pull(context.Background()); err == nil || !strings.Contains(err.Error(), "progress stream ended before completion") {
		t.Fatalf("pull error = %v, want premature stream EOF", err)
	}
	if f.downloading.Load() || f.unloads.Load() != 1 {
		t.Fatalf("downloading=%t unloads=%d, want stopped with one unload", f.downloading.Load(), f.unloads.Load())
	}
}

func TestLlamaCPPPullStreamReadErrorStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	f.stream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		if _, err := fmt.Fprint(w, ": ready\n\n"); err != nil {
			t.Errorf("write SSE greeting: %v", err)
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush SSE greeting: %v", err)
			return
		}
		select {
		case <-f.started:
		case <-r.Context().Done():
		}
	}
	if err := f.pull(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("pull error = %v, want truncated SSE read", err)
	}
	if f.downloading.Load() || f.unloads.Load() != 1 {
		t.Fatalf("downloading=%t unloads=%d, want stopped with one unload", f.downloading.Load(), f.unloads.Load())
	}
}
