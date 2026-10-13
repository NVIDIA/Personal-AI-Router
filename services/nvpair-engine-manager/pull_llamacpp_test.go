// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		_, err := fmt.Fprint(w, ": ready\n\n")
		if !assert.NoError(t, err, "write SSE greeting") {
			return
		}
		if !assert.NoError(t, http.NewResponseController(w).Flush(), "flush SSE greeting") {
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
			if !assert.NoError(t, err, "read start request") {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			var body struct {
				Model string `json:"model"`
			}
			if !assert.NoError(t, json.Unmarshal(data, &body), "decode start request") || !assert.Equal(t, llamaCPPPullTestModel, body.Model) {
				http.Error(w, "wrong model", http.StatusBadRequest)
				return
			}
			close(f.started)
			if f.start != nil {
				f.start(w, r)
				return
			}
			f.downloading.Store(true)
			_, err = fmt.Fprint(w, `{"success":true}`)
			assert.NoError(t, err, "write start response")
		case http.MethodGet:
			f.inventories.Add(1)
			if f.inventory != nil {
				f.inventory(w, r)
				return
			}
			_, err := fmt.Fprintf(w, `{"data":[{"id":%q,"status":{"value":"downloading"}},{"id":"other/model","status":{"value":"downloading"}}]}`, llamaCPPPullTestModel)
			assert.NoError(t, err, "write inventory")
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/models/unload", func(w http.ResponseWriter, r *http.Request) {
		f.unloads.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body), "decode stop request") {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if !assert.Equal(t, http.MethodPost, r.Method) || !assert.Equal(t, llamaCPPPullTestModel, body.Model) {
			http.Error(w, "wrong download", http.StatusBadRequest)
			return
		}
		if f.unload != nil {
			f.unload(w, r)
			return
		}
		f.downloading.Store(false)
		_, err := fmt.Fprint(w, `{"success":true}`)
		assert.NoError(t, err, "write stop response")
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
		require.FailNow(t, "timed out waiting for pull request")
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
	require.ErrorIs(t, f.pull(ctx), context.Canceled)
	require.False(t, f.downloading.Load(), "download must stop")
	require.Equal(t, int32(1), f.unloads.Load())
}

func TestLlamaCPPPullInactivityStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	f.ex.pullProgressTimeout = 100 * time.Millisecond
	require.ErrorIs(t, f.pull(context.Background()), errPullProgressTimeout)
	require.False(t, f.downloading.Load(), "download must stop")
	require.Equal(t, int32(1), f.unloads.Load())
}

func TestLlamaCPPPullStreamEOFStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	go func() {
		<-f.started
		close(f.endStream)
	}()
	require.ErrorContains(t, f.pull(context.Background()), "progress stream ended before completion")
	require.False(t, f.downloading.Load(), "download must stop")
	require.Equal(t, int32(1), f.unloads.Load())
}

func TestLlamaCPPPullStreamReadErrorStopsDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	f.stream = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, err := fmt.Fprint(w, ": ready\n\n")
		if !assert.NoError(t, err, "write SSE greeting") {
			return
		}
		if !assert.NoError(t, http.NewResponseController(w).Flush(), "flush SSE greeting") {
			return
		}
		select {
		case <-f.started:
		case <-r.Context().Done():
		}
	}
	require.ErrorContains(t, f.pull(context.Background()), "unexpected EOF")
	require.False(t, f.downloading.Load(), "download must stop")
	require.Equal(t, int32(1), f.unloads.Load())
}

func TestLlamaCPPPullCancellationWaitsForStartAcceptance(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	releaseStart := make(chan struct{}, 1)
	defer close(releaseStart)
	f.start = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-releaseStart:
		case <-r.Context().Done():
			assert.Fail(t, "start handshake was cancelled before acceptance")
			return
		}
		f.downloading.Store(true)
		_, err := fmt.Fprint(w, `{"success":true}`)
		assert.NoError(t, err, "write accepted response")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.pull(ctx) }()
	waitLlamaCPPPullSignal(t, f.started)
	cancel()
	select {
	case err := <-result:
		require.FailNowf(t, "pull returned before start acceptance", "%v", err)
	case <-time.After(30 * time.Millisecond):
	}
	// Send instead of closing so the deferred close also releases the handler if
	// an assertion fails before this point.
	releaseStart <- struct{}{}
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, f.downloading.Load())
		require.Equal(t, int32(1), f.unloads.Load())
	case <-time.After(5 * time.Second):
		require.FailNow(t, "pull did not return after acceptance and cleanup")
	}
}

func TestLlamaCPPPullCancelledBeforeStartDoesNotDownload(t *testing.T) {
	f := newLlamaCPPPullFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, f.pull(ctx), context.Canceled)
	select {
	case <-f.started:
		require.FailNow(t, "cancelled pull sent a start request")
	default:
	}
	require.Zero(t, f.unloads.Load(), "cancelled pull attempted cleanup without starting")
	require.Zero(t, f.inventories.Load(), "cancelled pull attempted cleanup without starting")
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
			require.Error(t, err, "invalid model params were accepted")
			select {
			case <-f.started:
				require.FailNow(t, "invalid params sent a start request")
			default:
			}
			require.Zero(t, f.inventories.Load(), "invalid params triggered cleanup")
			require.Zero(t, f.unloads.Load(), "invalid params triggered cleanup")
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
				_, err := fmt.Fprint(w, body)
				assert.NoError(t, err, "write rejected start")
			}
			require.Error(t, f.pull(context.Background()), "rejected start returned success")
			require.True(t, f.downloading.Load(), "rejected start touched another download")
			require.Zero(t, f.unloads.Load(), "rejected start touched another download")
			require.Zero(t, f.inventories.Load(), "rejected start touched another download")
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
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "acceptance and cancellation could not be confirmed")
			require.True(t, f.downloading.Load(), "unconfirmed start unloaded an unowned download")
			require.Zero(t, f.unloads.Load(), "unconfirmed start unloaded an unowned download")
			require.Zero(t, f.inventories.Load(), "unconfirmed start unloaded an unowned download")
		})
	}
	test("malformed acknowledgement", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		_, err := fmt.Fprint(w, "not JSON")
		assert.NoError(t, err, "write malformed acknowledgement")
	})
	test("missing success flag", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		_, err := fmt.Fprint(w, `{}`)
		assert.NoError(t, err, "write incomplete acknowledgement")
	})
	test("null success flag", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		_, err := fmt.Fprint(w, `{"success":null}`)
		assert.NoError(t, err, "write null acknowledgement")
	})
	test("truncated acknowledgement", func(t *testing.T, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, err := fmt.Fprint(w, `{"success":`)
		assert.NoError(t, err, "write truncated acknowledgement")
	})
	test("start header timeout", func(_ *testing.T, _ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	test("start body timeout", func(t *testing.T, w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, http.NewResponseController(w).Flush(), "flush start headers") {
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
				if !assert.NoError(t, http.NewResponseController(w).Flush(), "flush SSE headers") {
					return
				}
				select {
				case <-f.started:
				case <-r.Context().Done():
					return
				}
				_, err := fmt.Fprintf(w, "data: {\"model\":%q,\"event\":%q}\n\n", llamaCPPPullTestModel, event)
				assert.NoError(t, err, "write terminal event")
			}
			err := f.pull(context.Background())
			if event == "download_finished" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Zero(t, f.unloads.Load(), "terminal event triggered cleanup")
			require.Zero(t, f.inventories.Load(), "terminal event triggered cleanup")
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
				_, err := fmt.Fprint(w, body)
				assert.NoError(t, err, "write completed inventory")
			}
			err := f.pull(context.Background())
			require.ErrorContains(t, err, "progress stream ended before completion")
			require.NotContains(t, err.Error(), "could not confirm")
			require.Zero(t, f.unloads.Load(), "completed model must be preserved")
			require.Equal(t, int32(1), f.inventories.Load())
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
				_, err := fmt.Fprint(w, `{"success":true}`)
				assert.NoError(t, err, "write start response")
			}
			f.unload = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, err := fmt.Fprint(w, body)
				assert.NoError(t, err, "write cleanup failure")
			}
			err := f.pull(ctx)
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "could not confirm download stopped")
			require.True(t, f.downloading.Load(), "failed cleanup was treated as a confirmed stop")
			require.Equal(t, int32(1), f.unloads.Load(), "failed cleanup must not be retried")
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
			require.ErrorIs(t, err, errPullProgressTimeout)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.ErrorContains(t, err, "could not confirm download stopped")
			require.True(t, f.downloading.Load(), "cleanup timeout was treated as a confirmed stop")
		})
	}
}

func TestLlamaCPPPullInvalidInventoryDoesNotConfirmStop(t *testing.T) {
	for _, body := range []string{"not JSON", `{}`, `{"data":null}`, `{"data":{}}`, fmt.Sprintf(`{"data":[{"id":%q}]}`, llamaCPPPullTestModel)} {
		t.Run(body, func(t *testing.T) {
			f := newLlamaCPPPullFixture(t)
			close(f.endStream)
			f.inventory = func(w http.ResponseWriter, _ *http.Request) {
				_, err := fmt.Fprint(w, body)
				assert.NoError(t, err, "write invalid inventory")
			}
			err := f.pull(context.Background())
			require.ErrorContains(t, err, "could not confirm download stopped")
			require.Zero(t, f.unloads.Load(), "invalid inventory must not confirm a stop")
		})
	}
}
