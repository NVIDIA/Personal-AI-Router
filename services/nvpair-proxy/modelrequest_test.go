// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/modelrequest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelRequestGate_AdmitsConcurrentRequestsForTheSameModel(t *testing.T) {
	var gate modelRequestGate
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	first, err := gate.acquire(ctx, "model-a")
	require.NoError(t, err)
	defer first()
	second, err := gate.acquire(ctx, "model-a")
	require.NoError(t, err)
	second()
}

func TestModelRequestGate_WaitsForEveryActiveResponseBeforeSwitching(t *testing.T) {
	var gate modelRequestGate
	first, err := gate.acquire(t.Context(), "model-a")
	require.NoError(t, err)
	second, err := gate.acquire(t.Context(), "model-a")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	entered := make(chan func(), 1)
	go func() {
		release, err := gate.acquire(ctx, "model-b")
		if err == nil {
			entered <- release
		}
	}()
	first()
	select {
	case release := <-entered:
		release()
		t.Fatal("switch began while the second response was active")
	case <-time.After(30 * time.Millisecond):
	}
	second()
	select {
	case release := <-entered:
		release()
	case <-ctx.Done():
		t.Fatal("switch did not proceed after both responses finished")
	}
}

func TestModelRequestGate_RemovesCancelledWaiters(t *testing.T) {
	var gate modelRequestGate
	active, err := gate.acquire(t.Context(), "model-a")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = gate.acquire(ctx, "model-b")
	require.ErrorIs(t, err, context.Canceled)
	active()
	nextCtx, nextCancel := context.WithTimeout(t.Context(), time.Second)
	defer nextCancel()
	release, err := gate.acquire(nextCtx, "model-c")
	require.NoError(t, err)
	release()
}

// modelPreparationWriter stands in for the broker. It records model preparation
// requests and can send a reply or leave the request pending for the test.
type modelPreparationWriter struct {
	proxy       *Proxy
	port        int
	requests    chan modelrequest.Preparation
	cancelled   chan modelrequest.Preparation
	deferReply  bool
	failure     string
	beforeReply func() error
}

func (w *modelPreparationWriter) Read(data []byte) (int, error) { return 0, io.EOF }

func (w *modelPreparationWriter) Write(data []byte) (int, error) {
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		return 0, err
	}
	if message.Method != modelrequest.Request && message.Method != modelrequest.Cancel {
		return len(data), nil
	}
	var request modelrequest.Preparation
	if err := json.Unmarshal(message.Params, &request); err != nil {
		return 0, err
	}
	if message.Method == modelrequest.Cancel {
		if w.cancelled != nil {
			w.cancelled <- request
		}
		return len(data), nil
	}
	w.requests <- request
	if w.deferReply {
		return len(data), nil
	}
	if w.beforeReply != nil {
		if err := w.beforeReply(); err != nil {
			return 0, err
		}
	}
	reply := modelrequest.Reply{ID: request.ID, Engine: request.Engine, Port: w.port, Error: w.failure}
	if w.failure != "" {
		reply.Status = http.StatusUnprocessableEntity
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return 0, err
	}
	w.proxy.handleModelPrepared(&Message{Method: modelrequest.ReplyMethod, Params: raw})
	return len(data), nil
}

func modelRequestFacade(t *testing.T, serverURL string, failure string) (*facade, *modelPreparationWriter) {
	t.Helper()
	target, err := url.Parse(serverURL)
	require.NoError(t, err)
	port, err := strconv.Atoi(target.Port())
	require.NoError(t, err)
	writer := &modelPreparationWriter{port: port, requests: make(chan modelrequest.Preparation, 10), failure: failure}
	proxy := NewProxy(NewCodec(writer))
	writer.proxy = proxy
	profile, ok := profileFor("vllm")
	require.True(t, ok)
	f := newFacade(proxy, profile, NewDiscovery(), 8000)
	proxy.facades = map[string]*facade{"vllm": f}
	require.NoError(t, f.setLocalBackend(localBackend{Engine: "vllm", Host: "127.0.0.1", Port: port, LoadOnRequest: true}))
	t.Cleanup(proxy.closeIdleTransports)
	return f, writer
}

func TestPrepareModelTransport_PreparesTheRequestedModelBeforeForwarding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, "ok")
		assert.NoError(t, err)
	}))
	defer server.Close()
	facade, writer := modelRequestFacade(t, server.URL, "")
	request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	response, err := (prepareModelTransport{next: http.DefaultTransport, facade: facade, model: "org/model-a"}).RoundTrip(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	prepared := <-writer.requests
	assert.Equal(t, "vllm", prepared.Engine)
	assert.Equal(t, "org/model-a", prepared.Model)
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func TestRequestTransport_PreparesOnlyNonBrowserJSONInference(t *testing.T) {
	test := func(name string, headers http.Header, wantPreparation bool) {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			f, writer := modelRequestFacade(t, server.URL, "")
			require.NoError(t, f.setLocalModelInventory(modelrequest.LocalInventory{
				Engine: "vllm", ID: "local-node", EnginePort: writer.port,
				Enabled: true, Models: []string{"org/model-a"},
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"org/model-a","messages":[]}`))
			request.RemoteAddr = "127.0.0.1:40000"
			request.Header = headers.Clone()
			recorder := httptest.NewRecorder()

			f.handlePlain(recorder, request)

			assert.Equal(t, http.StatusOK, recorder.Code)
			if wantPreparation {
				require.Len(t, writer.requests, 1)
				assert.Equal(t, "org/model-a", (<-writer.requests).Model)
			} else {
				assert.Empty(t, writer.requests, "forwarding must not trigger model preparation")
			}
			assert.Empty(t, recorder.Header().Get("Access-Control-Allow-Origin"))
		})
	}
	test("JSON without an Origin", http.Header{"Content-Type": {"application/json"}}, true)
	test("JSON with a charset", http.Header{"Content-Type": {"application/json; charset=utf-8"}}, true)
	test("browser Origin", http.Header{"Content-Type": {"application/json"}, "Origin": {"https://website.example"}}, false)
	test("null Origin", http.Header{"Content-Type": {"application/json"}, "Origin": {"null"}}, false)
	test("empty Origin header", http.Header{"Content-Type": {"application/json"}, "Origin": {""}}, false)
	test("plain text", http.Header{"Content-Type": {"text/plain"}}, false)
	test("form submission", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, false)
	test("missing content type", http.Header{}, false)
	test("malformed content type", http.Header{"Content-Type": {"application/json; charset"}}, false)
}

// Browser and non-JSON requests cannot automatically load models, but the
// engine can still accept them for an already loaded model. A later request
// for another model must wait until that response finishes or closes,
// because switching models can restart vLLM and cut the response short.
func TestRequestTransport_DrainsResponsesWithoutModelPreparation(t *testing.T) {
	test := func(name string, headers http.Header) {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				assert.NoError(t, http.NewResponseController(w).Flush())
				<-r.Context().Done()
			}))
			defer server.Close()
			f, writer := modelRequestFacade(t, server.URL, "")
			firstRequest := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
			firstRequest.Header = headers.Clone()
			first, err := f.requestTransport(http.DefaultTransport, firstRequest, "org/a").RoundTrip(firstRequest)
			require.NoError(t, err)
			defer first.Body.Close()
			require.Empty(t, writer.requests, "this request must not prepare a model")

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil).WithContext(ctx)
				request.Header.Set("Content-Type", "application/json")
				response, err := f.requestTransport(http.DefaultTransport, request, "org/b").RoundTrip(request)
				if err == nil {
					err = response.Body.Close()
				}
				finished <- err
			}()
			waitForCond(t, time.Second, "model B to wait for the active response", func() bool {
				f.modelGate.mu.Lock()
				defer f.modelGate.mu.Unlock()
				return len(f.modelGate.queue) == 1
			})
			assert.Empty(t, writer.requests, "switching must wait even when the active request cannot prepare models")

			require.NoError(t, first.Body.Close())
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("model B did not proceed after the response closed")
			}
			require.Len(t, writer.requests, 1)
			assert.Equal(t, "org/b", (<-writer.requests).Model)
		})
	}
	test("browser JSON", http.Header{"Content-Type": {"application/json"}, "Origin": {"https://website.example"}})
	test("non-JSON native request", http.Header{"Content-Type": {"text/plain"}})
}

func TestPrepareModelTransport_ReusesPreparationForTheServingModel(t *testing.T) {
	test := func(name string, models []string, wantPreparations []string) {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			f, writer := modelRequestFacade(t, server.URL, "")
			for _, model := range models {
				request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
				response, err := (prepareModelTransport{next: http.DefaultTransport, facade: f, model: model}).RoundTrip(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
			}

			prepared := make([]string, 0, len(writer.requests))
			for len(writer.requests) > 0 {
				prepared = append(prepared, (<-writer.requests).Model)
			}
			assert.Equal(t, wantPreparations, prepared)
		})
	}
	test("same-model requests reuse preparation", []string{"org/a", "org/a", "org/a"}, []string{"org/a"})
	test("switching back requires preparation", []string{"org/a", "org/b", "org/a"}, []string{"org/a", "org/b", "org/a"})
}

func TestPrepareModelTransport_SharesPreparationForConcurrentSameModelRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	f, writer := modelRequestFacade(t, server.URL, "")
	writer.deferReply = true
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	finished := make(chan error, 8)
	for range cap(finished) {
		go func() {
			request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil).WithContext(ctx)
			response, err := (prepareModelTransport{next: http.DefaultTransport, facade: f, model: "org/a"}).RoundTrip(request)
			if err == nil {
				err = response.Body.Close()
			}
			finished <- err
		}()
	}
	// Keep preparation pending until every caller has joined, so sequential
	// reuse of a cached result cannot satisfy this concurrency test.
	waitForCond(t, time.Second, "all requests to share pending preparation", func() bool {
		f.modelRequests.mu.Lock()
		defer f.modelRequests.mu.Unlock()
		return f.modelRequests.preparing != nil && f.modelRequests.preparing.waiters == cap(finished) && len(writer.requests) == 1
	})
	prepared := <-writer.requests
	sendPreparationReply(t, f, modelrequest.Reply{ID: prepared.ID, Engine: "vllm", Port: writer.port})

	for range cap(finished) {
		select {
		case err := <-finished:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("shared preparation did not release every request")
		}
	}
	assert.Empty(t, writer.requests, "concurrent callers must not send another preparation")
}

func TestPrepareModelTransport_RechecksPreparationAfterLifecycleChanges(t *testing.T) {
	test := func(name string, change func(*testing.T, *facade)) {
		t.Run(name, func(t *testing.T) {
			f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
			backend := f.currentLocalBackend()
			backend.Healthy = true
			require.NoError(t, f.setLocalBackend(backend))
			require.NoError(t, f.prepareRequestedModel(t.Context(), "org/a"))
			require.Len(t, writer.requests, 1)
			<-writer.requests

			change(t, f)
			require.NoError(t, f.prepareRequestedModel(t.Context(), "org/a"))

			require.Len(t, writer.requests, 1)
			assert.Equal(t, "org/a", (<-writer.requests).Model)
		})
	}
	test("engine manager restarts", func(t *testing.T, f *facade) {
		require.True(t, f.host.handleModelPrepared(&Message{Method: modelrequest.Reset}))
	})
	test("engine becomes unhealthy", func(t *testing.T, f *facade) {
		backend := f.currentLocalBackend()
		backend.Healthy = false
		require.NoError(t, f.setLocalBackend(backend))
	})
	test("engine lifecycle changes while still enabled", func(t *testing.T, f *facade) {
		require.NoError(t, f.setLocalModelInventory(modelrequest.LocalInventory{
			Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: true,
		}))
	})
}

func TestPrepareModelTransport_DoesNotReusePreparationAcrossAnOverlappingLifecycleChange(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	writer.beforeReply = func() error { return f.setLocalBackend(f.currentLocalBackend()) }
	require.NoError(t, f.prepareRequestedModel(t.Context(), "org/a"))
	writer.beforeReply = nil

	require.NoError(t, f.prepareRequestedModel(t.Context(), "org/a"))

	assert.Len(t, writer.requests, 2)
}

func TestPrepareModelTransport_PreservesPreparationAcrossAnInventoryRefresh(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	backend := f.currentLocalBackend()
	backend.Healthy = true
	require.NoError(t, f.setLocalBackend(backend))
	require.NoError(t, f.prepareRequestedModel(t.Context(), "org/a"))

	require.NoError(t, f.setLocalModelInventory(modelrequest.LocalInventory{
		Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: true, Models: []string{"org/a", "org/new-download"},
	}))
	require.NoError(t, f.prepareRequestedModel(t.Context(), "org/a"))

	assert.Len(t, writer.requests, 1)
}

// A request joining an existing load must be able to time out independently
// without stopping the load that another request still needs.
func TestPrepareModelTransport_CancelsARequestWaitingForSharedPreparation(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()
	writer.beforeReply = func() error {
		close(started)
		<-resume
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- f.prepareRequestedModel(t.Context(), "org/a") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the initial preparation did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()

	err := f.prepareRequestedModel(ctx, "org/a")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	release()
	require.NoError(t, <-first)
	assert.Len(t, writer.requests, 1)
}

// Observing Done synchronizes with a caller waiting for shared preparation,
// before another caller is cancelled or the engine manager replies.
type preparationWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *preparationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func sendPreparationReply(t *testing.T, f *facade, reply modelrequest.Reply) {
	t.Helper()
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	require.True(t, f.host.handleModelPrepared(&Message{Method: modelrequest.ReplyMethod, Params: raw}))
}

// A client that started a shared load can disconnect while another client
// still needs the model. Keep the original load alive for the remaining request.
func TestPrepareRequestedModel_KeepsLoadingForRemainingRequests(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	writer.deferReply = true
	writer.cancelled = make(chan modelrequest.Preparation, 2)
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	first := make(chan error, 1)
	go func() { first <- f.prepareRequestedModel(firstCtx, "org/a") }()
	waitForCond(t, time.Second, "the initial model preparation", func() bool { return len(writer.requests) == 1 })
	request := <-writer.requests
	secondCtx := &preparationWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- f.prepareRequestedModel(secondCtx, "org/a") }()
	select {
	case <-secondCtx.waiting:
	case <-time.After(time.Second):
		t.Fatal("the second request did not wait for preparation")
	}

	cancelFirst()

	waitForCond(t, time.Second, "the initiating request to return", func() bool { return len(first) == 1 })
	require.ErrorIs(t, <-first, context.Canceled)
	assert.Empty(t, writer.cancelled, "the remaining request still needs the model")
	sendPreparationReply(t, f, modelrequest.Reply{ID: request.ID, Engine: "vllm", Port: writer.port})
	waitForCond(t, time.Second, "the remaining request to finish preparation", func() bool { return len(second) == 1 })
	require.NoError(t, <-second)
	assert.Empty(t, writer.requests, "both requests must share one preparation")
	assert.Empty(t, writer.cancelled)
}

// Once no request needs the model, stop loading and clear the pending state
// so abandoned work does not consume resources or block a later request.
func TestPrepareRequestedModel_CancelsLoadingAfterTheLastRequestLeaves(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	writer.deferReply = true
	writer.cancelled = make(chan modelrequest.Preparation, 2)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- f.prepareRequestedModel(ctx, "org/a") }()
	waitForCond(t, time.Second, "the model preparation", func() bool { return len(writer.requests) == 1 })
	request := <-writer.requests

	cancel()

	waitForCond(t, time.Second, "the cancelled request to return", func() bool { return len(finished) == 1 })
	require.ErrorIs(t, <-finished, context.Canceled)
	waitForCond(t, time.Second, "the model preparation cancellation", func() bool { return len(writer.cancelled) == 1 })
	assert.Equal(t, request, <-writer.cancelled)
	waitForCond(t, time.Second, "pending preparation cleanup", func() bool {
		f.modelRequests.mu.Lock()
		defer f.modelRequests.mu.Unlock()
		return len(f.modelRequests.pending) == 0 && f.modelRequests.preparing == nil
	})
}

// A failed load must give every waiting request the same error. Retrying it
// for each request would repeat expensive startup that is already known to fail.
func TestPrepareRequestedModel_SharesPreparationFailuresWithWaitingRequests(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	writer.deferReply = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- f.prepareRequestedModel(ctx, "org/a") }()
	waitForCond(t, time.Second, "the initial model preparation", func() bool { return len(writer.requests) == 1 })
	request := <-writer.requests
	secondCtx := &preparationWaitContext{Context: ctx, waiting: make(chan struct{})}
	second := make(chan error, 1)
	go func() { second <- f.prepareRequestedModel(secondCtx, "org/a") }()
	select {
	case <-secondCtx.waiting:
	case <-time.After(time.Second):
		t.Fatal("the second request did not wait for preparation")
	}

	sendPreparationReply(t, f, modelrequest.Reply{ID: request.ID, Engine: "vllm", Error: "model does not fit in memory", Status: http.StatusUnprocessableEntity})

	waitForCond(t, time.Second, "both preparation failures", func() bool { return len(first) == 1 && len(second) == 1 })
	test := func(name string, result error) {
		t.Run(name, func(t *testing.T) {
			var failure modelPreparationFailure
			require.ErrorAs(t, result, &failure)
			assert.Equal(t, http.StatusUnprocessableEntity, failure.status)
			assert.Equal(t, "model does not fit in memory", failure.message)
		})
	}
	test("initiating request", <-first)
	test("waiting request", <-second)
	assert.Empty(t, writer.requests, "a shared failure must not restart preparation for another waiter")
}

func TestPrepareModelTransport_RechecksPreparationAfterAnEngineError(t *testing.T) {
	test := func(name string, status int) {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			f, writer := modelRequestFacade(t, server.URL, "")
			for range 2 {
				request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
				response, err := (prepareModelTransport{next: http.DefaultTransport, facade: f, model: "org/a"}).RoundTrip(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
			}
			assert.Len(t, writer.requests, 2)
		})
	}
	test("model no longer served", http.StatusNotFound)
	test("engine unavailable", http.StatusServiceUnavailable)
}

// A failed model load should return the engine manager's 422 and its diagnostic
// to the client so the client knows why inference could not start.
func TestPrepareModelTransport_DoesNotForwardWhenPreparationFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached inference despite failed model preparation")
	}))
	t.Cleanup(server.Close)
	const failure = "model does not fit in memory"
	facade, _ := modelRequestFacade(t, server.URL, failure)
	request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)

	response, err := (prepareModelTransport{next: http.DefaultTransport, facade: facade, model: "org/model-a"}).RoundTrip(request)
	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()
	var reply struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&reply))
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode)
	assert.Equal(t, failure, reply.Error)
}

func TestPrepareModelTransport_HoldsTheModelUntilTheResponseBodyCloses(t *testing.T) {
	finished := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finished) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, http.NewResponseController(w).Flush())
		select {
		case <-finished:
		case <-r.Context().Done():
		}
	}))
	defer func() { once.Do(func() { close(finished) }); server.Close() }()
	facade, writer := modelRequestFacade(t, server.URL, "")
	request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	first, err := (prepareModelTransport{next: http.DefaultTransport, facade: facade, model: "org/model-a"}).RoundTrip(request)
	require.NoError(t, err)
	<-writer.requests
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	secondDone := make(chan error, 1)
	go func() {
		response, err := (prepareModelTransport{next: http.DefaultTransport, facade: facade, model: "org/model-b"}).RoundTrip(request.WithContext(ctx))
		if err == nil {
			err = response.Body.Close()
		}
		secondDone <- err
	}()
	select {
	case <-writer.requests:
		t.Fatal("second model prepared before the first response closed")
	case <-time.After(30 * time.Millisecond):
	}
	require.NoError(t, first.Body.Close())
	select {
	case prepared := <-writer.requests:
		assert.Equal(t, "org/model-b", prepared.Model)
	case <-ctx.Done():
		t.Fatal("second model did not prepare after response closed")
	}
	once.Do(func() { close(finished) })
	require.NoError(t, <-secondDone)
}

func TestModelPreparationReply_WorkerRestartSettlesPendingRequests(t *testing.T) {
	facade, _ := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	pending := make(chan modelrequest.Reply, 1)
	facade.modelRequests.pending = map[string]chan modelrequest.Reply{"waiting-request": pending}

	require.True(t, facade.host.handleModelPrepared(&Message{Method: modelrequest.Reset}))

	reply := <-pending
	assert.Equal(t, "waiting-request", reply.ID)
	assert.Equal(t, http.StatusServiceUnavailable, reply.Status)
	assert.NotEmpty(t, reply.Error)
}

func TestLocalModelInventory_RoutesANewDownloadBeforeDiscoveryRefresh(t *testing.T) {
	f, _ := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	f.discovery.SetSubscribed([]Node{{ID: "local-node", Host: "127.0.0.1", Port: 8000, Addresses: []string{"127.0.0.1"}, Models: []string{"org/old-model"}}})
	inventory, err := json.Marshal(modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: true, Models: []string{"org/old-model", "org/new-model"}})
	require.NoError(t, err)
	id := json.RawMessage(`1`)

	f.host.handleMessage(&Message{ID: &id, Method: "vllm:node/set-local-models", Params: inventory})

	candidates := f.resolveCandidates("org/new-model")
	require.Len(t, candidates, 1)
	assert.Equal(t, "local-node", candidates[0].id)
	assert.Equal(t, "http://127.0.0.1:8001", candidates[0].url.String())
	assert.True(t, f.discovery.Has("local-node"))
}

func TestLocalModelInventory_EmptySnapshotOverridesStaleDiscovery(t *testing.T) {
	f, _ := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	f.discovery.SetSubscribed([]Node{{ID: "local-node", Host: "127.0.0.1", Port: 8000, Addresses: []string{"127.0.0.1"}, Models: []string{"org/deleted-model"}}})
	inventory, err := json.Marshal(modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: true, Models: []string{}})
	require.NoError(t, err)
	id := json.RawMessage(`1`)

	f.host.handleMessage(&Message{ID: &id, Method: "vllm:node/set-local-models", Params: inventory})

	assert.Empty(t, f.resolveCandidates("org/deleted-model"))
	assert.True(t, f.currentLocalBackend().LoadOnRequest)
}

func TestAvailableModelList_DoesNotStartAnEjectedEngine(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	f.discovery.SetLocal(Node{ID: "local-node", Models: []string{"org/first", "org/second"}})
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8000/v1/models", nil)
	recorder := httptest.NewRecorder()

	require.True(t, f.serveLocalAvailableModels(recorder, request))

	assert.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Data, 2)
	assert.Equal(t, "org/first", response.Data[0].ID)
	assert.Equal(t, "org/second", response.Data[1].ID)
	assert.Empty(t, writer.requests, "Listing models must not prepare one")
	assert.Empty(t, recorder.Header().Get("Access-Control-Allow-Origin"))
}

func TestAvailableModelList_PreservesTheNativeEnginesBrowserPolicy(t *testing.T) {
	f, _ := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	f.discovery.SetLocal(Node{ID: "local-node", Models: []string{"org/first"}})
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8000/v1/models", nil)
	request.Header.Set("Origin", "http://browser.example")

	assert.False(t, f.serveLocalAvailableModels(httptest.NewRecorder(), request))
}

func TestAvailableModelList_EnabledEmptyCacheReturnsAnEmptyList(t *testing.T) {
	f, writer := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	inventory, err := json.Marshal(modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: true, Models: []string{}})
	require.NoError(t, err)
	id := json.RawMessage(`1`)
	f.host.handleMessage(&Message{ID: &id, Method: "vllm:node/set-local-models", Params: inventory})
	recorder := httptest.NewRecorder()

	f.handleHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8000/v1/models", nil))

	assert.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.NotNil(t, response.Data)
	assert.Empty(t, response.Data)
	assert.Empty(t, writer.requests)
}

func modelRequestCluster(t *testing.T) (*facade, string, *modelPreparationWriter) {
	t.Helper()
	callerDir, servingDir := t.TempDir(), t.TempDir()
	clustertrusttest.Join(t, callerDir, "cluster", "caller")
	clustertrusttest.Join(t, servingDir, "cluster", "serving")
	pin := func(destination, source, id string) {
		t.Helper()
		certificate, err := os.ReadFile(filepath.Join(source, "node.crt"))
		require.NoError(t, err)
		raw, err := json.Marshal(map[string]string{"nodeUuid": id, "certPem": string(certificate)})
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(destination, "trusted"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(destination, "trusted", id+".json"), raw, 0o600))
	}
	pin(callerDir, servingDir, "serving")
	pin(servingDir, callerDir, "caller")
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "org/requested-model", request.Model)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"model": request.Model}))
	}))
	t.Cleanup(native.Close)
	serving, preparations := modelRequestFacade(t, native.URL, "")
	serving.host.mesh = clustertrust.Open(servingDir)
	ingress := httptest.NewUnstartedServer(http.HandlerFunc(serving.handleClusterIngress))
	ingress.TLS = serving.host.mesh.ServerTLSConfig()
	ingress.StartTLS()
	t.Cleanup(ingress.Close)
	caller := testProxy(serving.profile, NewDiscovery(), serving.profile.StandalonePort).soleFacade()
	caller.host.mesh = clustertrust.Open(callerDir)
	t.Cleanup(caller.host.closeIdleTransports)
	node := nodeFor(t, "serving", ingress.URL)
	node.ClusterUUID = "serving"
	node.Models = []string{"org/requested-model"}
	caller.discovery.SetSubscribed([]Node{node})
	return caller, ingress.URL, preparations
}

func TestClusterIngress_PreparesTheRequestedModelOnTheServingNode(t *testing.T) {
	caller, _, preparations := modelRequestCluster(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"org/requested-model","messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()

	caller.handlePlain(recorder, request)

	assert.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, preparations.requests, 1)
	prepared := <-preparations.requests
	assert.Equal(t, "org/requested-model", prepared.Model)
}

func TestClusterIngress_RejectsOversizedRequestsBeforeModelPreparation(t *testing.T) {
	caller, ingressURL, preparations := modelRequestCluster(t)
	client := &http.Client{Transport: caller.host.candidateTransport(candidate{peerUUID: "serving"}), Timeout: 5 * time.Second}
	body := `{"model":"org/requested-model","messages":[]}` + strings.Repeat(" ", maxInferenceBodyBytes)

	response, err := client.Post(ingressURL+"/v1/chat/completions", "application/json", strings.NewReader(body))

	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	assert.Empty(t, preparations.requests)
}

func TestLocalModelInventory_RestoresCachedRoutesImmediatelyAfterEnabling(t *testing.T) {
	f, _ := modelRequestFacade(t, "http://127.0.0.1:8001", "")
	require.NoError(t, f.setLocalModelInventory(modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: true, Models: []string{"org/cached"}}))
	require.NoError(t, f.setLocalModelInventory(modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: false}))
	require.Empty(t, f.resolveCandidates("org/cached"))
	require.NoError(t, f.setLocalModelInventory(modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 8001, Enabled: true}))
	candidates := f.resolveCandidates("org/cached")
	require.Len(t, candidates, 1)
	assert.Equal(t, "http://127.0.0.1:8001", candidates[0].url.String())
}

func TestModelRequestGate_DoesNotLetNewSameModelRequestsOvertakeAQueuedSwitch(t *testing.T) {
	var gate modelRequestGate
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	active, err := gate.acquire(ctx, "model-a")
	require.NoError(t, err)
	defer active()
	enteredB := make(chan func(), 1)
	go func() {
		release, err := gate.acquire(ctx, "model-b")
		if err == nil {
			enteredB <- release
		}
	}()
	require.Eventually(t, func() bool { gate.mu.Lock(); defer gate.mu.Unlock(); return len(gate.queue) == 1 }, time.Second, time.Millisecond)
	enteredA := make(chan func(), 1)
	go func() {
		release, err := gate.acquire(ctx, "model-a")
		if err == nil {
			enteredA <- release
		}
	}()
	require.Eventually(t, func() bool { gate.mu.Lock(); defer gate.mu.Unlock(); return len(gate.queue) == 2 }, time.Second, time.Millisecond)
	active()
	var releaseB func()
	select {
	case releaseB = <-enteredB:
	case <-ctx.Done():
		t.Fatal("queued model B never acquired admission")
	}
	select {
	case release := <-enteredA:
		release()
		t.Fatal("later model A overtook the queued switch")
	default:
	}
	releaseB()
	select {
	case release := <-enteredA:
		release()
	case <-ctx.Done():
		t.Fatal("later model A never acquired admission")
	}
}

func TestAvailableModelList_DoesNotListAPeerModelBeforeItIsRoutable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, `{"object":"list","data":[{"id":"org/existing"},{"id":"org/new-download"}]}`)
		assert.NoError(t, err)
	}))
	defer server.Close()
	f, _ := modelRequestFacade(t, server.URL, "")
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	peer := Node{ID: "peer", Host: u.Hostname(), Port: port, Addresses: []string{u.Hostname()}, Models: []string{"org/existing"}}
	f.discovery.AddManual(peer)
	list := func() []string {
		t.Helper()
		recorder := httptest.NewRecorder()
		f.handleHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		require.Equal(t, http.StatusOK, recorder.Code)
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
		ids := make([]string, 0, len(result.Data))
		for _, item := range result.Data {
			ids = append(ids, item.ID)
		}
		return ids
	}
	assert.Equal(t, []string{"org/existing"}, list())
	assert.Empty(t, f.resolveCandidates("org/new-download"))
	peer.Models = append(peer.Models, "org/new-download")
	f.discovery.AddManual(peer)
	assert.Equal(t, []string{"org/existing", "org/new-download"}, list())
	assert.Len(t, f.resolveCandidates("org/new-download"), 1)
}
