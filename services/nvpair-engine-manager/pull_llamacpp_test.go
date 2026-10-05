// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	inventories atomic.Int32
	start       http.HandlerFunc
	inventory   http.HandlerFunc
	unload      http.HandlerFunc
	stream      http.HandlerFunc
}

func newLlamaCPPPullFixture(t *testing.T) *llamaCPPPullFixture {
	t.Helper()
	f := &llamaCPPPullFixture{started: make(chan struct{}), endStream: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
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
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read start request: %v", err)
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			var body struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(data, &body); err != nil || body.Model != llamaCPPPullTestModel {
				t.Errorf("decode start request = %+v, error %v", body, err)
				http.Error(w, "wrong model", http.StatusBadRequest)
				return
			}
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
			f.inventories.Add(1)
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

func TestLlamaCPPPullCancellationWaitsForStartAcceptance(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	releaseStart := make(chan struct{}, 1)
	defer close(releaseStart)
	f.start = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-releaseStart:
		case <-r.Context().Done():
			t.Error("start handshake was cancelled before acceptance")
			return
		}
		f.downloading.Store(true)
		if _, err := fmt.Fprint(w, `{"success":true}`); err != nil {
			t.Errorf("write accepted response: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.pull(ctx) }()
	waitLlamaCPPPullSignal(t, f.started)
	cancel()
	select {
	case err := <-result:
		t.Fatalf("pull returned before start acceptance: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	// Send instead of closing so the deferred close also releases the handler if
	// an assertion fails before this point.
	releaseStart <- struct{}{}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || f.downloading.Load() || f.unloads.Load() != 1 {
			t.Fatalf("error=%v downloading=%t unloads=%d", err, f.downloading.Load(), f.unloads.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pull did not return after acceptance and cleanup")
	}
}

func TestLlamaCPPPullCancelledBeforeStartDoesNotDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.pull(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pull error = %v, want cancellation", err)
	}
	select {
	case <-f.started:
		t.Fatal("cancelled pull sent a start request")
	default:
	}
	if f.unloads.Load() != 0 || f.inventories.Load() != 0 {
		t.Fatal("cancelled pull attempted cleanup without starting")
	}
}

func TestLlamaCPPPullRejectsInvalidModelParamsBeforeStarting(t *testing.T) {
	for _, tc := range []struct{ name, params string }{
		{"different model", `{"model":"other/model"}`},
		{"missing model", `{}`},
		{"invalid JSON", `{"model":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			_, err := f.ex.PullModelStream(context.Background(), "fake", llamaCPPPullTestModel, json.RawMessage(tc.params))
			if err == nil {
				t.Fatal("invalid model params were accepted")
			}
			select {
			case <-f.started:
				t.Fatal("invalid params sent a start request")
			default:
			}
			if f.inventories.Load() != 0 || f.unloads.Load() != 0 {
				t.Fatal("invalid params triggered cleanup")
			}
		})
	}
}

func TestLlamaCPPPullRejectedStartPreservesOtherDownload(t *testing.T) {
	test := func(name string, status int, body string) {
		t.Run(name, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			f.downloading.Store(true)
			f.start = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Errorf("write rejected start: %v", err)
				}
			}
			if err := f.pull(context.Background()); err == nil {
				t.Fatal("rejected start returned success")
			}
			if !f.downloading.Load() || f.unloads.Load() != 0 || f.inventories.Load() != 0 {
				t.Fatal("rejected start touched another download")
			}
		})
	}
	test("HTTP rejection", http.StatusConflict, `{"error":"already exists"}`)
	test("negative acknowledgement", http.StatusOK, `{"success":false}`)
}

func TestLlamaCPPPullUnconfirmedStartDoesNotUnload(t *testing.T) {
	test := func(name string, respond func(*testing.T, http.ResponseWriter, *http.Request)) {
		t.Run(name, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			f.ex.pullStartTimeout = 100 * time.Millisecond
			f.downloading.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.start = func(w http.ResponseWriter, r *http.Request) {
				cancel()
				respond(t, w, r)
			}
			err := f.pull(ctx)
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "acceptance and cancellation could not be confirmed") {
				t.Fatalf("pull error = %v, want unconfirmed acceptance preserving cancellation", err)
			}
			if !f.downloading.Load() || f.unloads.Load() != 0 || f.inventories.Load() != 0 {
				t.Fatal("unconfirmed start unloaded an unowned download")
			}
		})
	}
	test("malformed acknowledgement", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, "not JSON"); err != nil {
			t.Errorf("write malformed acknowledgement: %v", err)
		}
	})
	test("missing success flag", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{}`); err != nil {
			t.Errorf("write incomplete acknowledgement: %v", err)
		}
	})
	test("null success flag", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"success":null}`); err != nil {
			t.Errorf("write null acknowledgement: %v", err)
		}
	})
	test("truncated acknowledgement", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		if _, err := fmt.Fprint(w, `{"success":`); err != nil {
			t.Errorf("write truncated acknowledgement: %v", err)
		}
	})
	test("start header timeout", func(_ *testing.T, _ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	test("start body timeout", func(t *testing.T, w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush start headers: %v", err)
			return
		}
		<-r.Context().Done()
	})
}

func TestLlamaCPPPullTerminalEventsSkipCleanup(t *testing.T) {
	for _, event := range []string{"download_finished", "download_failed"} {
		t.Run(event, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			f.stream = func(w http.ResponseWriter, r *http.Request) {
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("flush SSE headers: %v", err)
					return
				}
				select {
				case <-f.started:
				case <-r.Context().Done():
					return
				}
				if _, err := fmt.Fprintf(w, "data: {\"model\":%q,\"event\":%q}\n\n", llamaCPPPullTestModel, event); err != nil {
					t.Errorf("write terminal event: %v", err)
				}
			}
			err := f.pull(context.Background())
			if (err == nil) != (event == "download_finished") {
				t.Fatalf("terminal %s returned error %v", event, err)
			}
			if f.unloads.Load() != 0 || f.inventories.Load() != 0 {
				t.Fatal("terminal event triggered cleanup")
			}
		})
	}
}

func TestLlamaCPPPullCleanupPreservesCompletedModels(t *testing.T) {
	for _, status := range []string{"downloaded", "loaded", "unloaded", "missing"} {
		t.Run(status, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			close(f.endStream)
			f.inventory = func(w http.ResponseWriter, _ *http.Request) {
				f.downloading.Store(false)
				body := `{"data":[{"id":"other/model","status":{"value":"downloading"}}]}`
				if status != "missing" {
					body = fmt.Sprintf(`{"data":[{"id":%q,"status":{"value":%q}}]}`, llamaCPPPullTestModel, status)
				}
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Errorf("write completed inventory: %v", err)
				}
			}
			err := f.pull(context.Background())
			if err == nil || !strings.Contains(err.Error(), "progress stream ended before completion") || strings.Contains(err.Error(), "could not confirm") {
				t.Fatalf("pull error = %v, want only premature SSE termination", err)
			}
			if f.unloads.Load() != 0 || f.inventories.Load() != 1 {
				t.Fatalf("inventories=%d unloads=%d, want completed model preserved", f.inventories.Load(), f.unloads.Load())
			}
		})
	}
}

func TestLlamaCPPPullCleanupFailurePreservesCancellation(t *testing.T) {
	test := func(name string, status int, body string) {
		t.Run(name, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.start = func(w http.ResponseWriter, _ *http.Request) {
				f.downloading.Store(true)
				cancel()
				if _, err := fmt.Fprint(w, `{"success":true}`); err != nil {
					t.Errorf("write start response: %v", err)
				}
			}
			f.unload = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Errorf("write cleanup failure: %v", err)
				}
			}
			err := f.pull(ctx)
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "could not confirm download stopped") {
				t.Fatalf("pull error = %v, want cancellation and cleanup failure", err)
			}
			if !f.downloading.Load() || f.unloads.Load() != 1 {
				t.Fatal("failed cleanup was treated as a confirmed stop or retried")
			}
		})
	}
	test("HTTP rejection", http.StatusServiceUnavailable, "unavailable")
	test("malformed acknowledgement", http.StatusOK, "invalid JSON")
	test("negative acknowledgement", http.StatusOK, `{"success":false}`)
}

func TestLlamaCPPPullCleanupTimeoutPreservesWatchdogCause(t *testing.T) {
	for _, route := range []string{"inventory", "unload"} {
		t.Run(route, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			f.ex.pullProgressTimeout = 100 * time.Millisecond
			f.ex.pullCleanupTimeout = 100 * time.Millisecond
			hang := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
			if route == "inventory" {
				f.inventory = hang
			} else {
				f.unload = hang
			}
			err := f.pull(context.Background())
			if !errors.Is(err, errPullProgressTimeout) || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "could not confirm download stopped") {
				t.Fatalf("pull error = %v, want watchdog and cleanup timeout", err)
			}
			if !f.downloading.Load() {
				t.Fatal("cleanup timeout was treated as a confirmed stop")
			}
		})
	}
}

func TestLlamaCPPPullInvalidInventoryDoesNotConfirmStop(t *testing.T) {
	for _, body := range []string{"not JSON", `{}`, `{"data":null}`, `{"data":{}}`, fmt.Sprintf(`{"data":[{"id":%q}]}`, llamaCPPPullTestModel)} {
		t.Run(body, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			close(f.endStream)
			f.inventory = func(w http.ResponseWriter, _ *http.Request) {
				if _, err := fmt.Fprint(w, body); err != nil {
					t.Errorf("write invalid inventory: %v", err)
				}
			}
			err := f.pull(context.Background())
			if err == nil || !strings.Contains(err.Error(), "could not confirm download stopped") || f.unloads.Load() != 0 {
				t.Fatalf("error=%v unloads=%d, want failed inventory validation", err, f.unloads.Load())
			}
		})
	}
}
