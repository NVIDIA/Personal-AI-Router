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

type modelPreparationWriter struct {
	proxy    *Proxy
	port     int
	requests chan modelrequest.Preparation
	failure  string
}

func (w *modelPreparationWriter) Read(data []byte) (int, error) { return 0, io.EOF }

func (w *modelPreparationWriter) Write(data []byte) (int, error) {
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		return 0, err
	}
	if message.Method != modelrequest.Request {
		return len(data), nil
	}
	var request modelrequest.Preparation
	if err := json.Unmarshal(message.Params, &request); err != nil {
		return 0, err
	}
	w.requests <- request
	raw, err := json.Marshal(modelrequest.Reply{ID: request.ID, Engine: request.Engine, Port: w.port, Error: w.failure, Status: http.StatusConflict})
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

func TestPrepareModelTransport_DoesNotForwardWhenPreparationFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached inference despite failed model preparation")
	}))
	defer server.Close()
	facade, _ := modelRequestFacade(t, server.URL, "engine is disabled")
	request := httptest.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", nil)
	response, err := (prepareModelTransport{next: http.DefaultTransport, facade: facade, model: "org/model-a"}).RoundTrip(request)
	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()
	assert.Equal(t, http.StatusConflict, response.StatusCode)
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
	inventory, err := json.Marshal(modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 8001, Models: []string{}})
	require.NoError(t, err)
	id := json.RawMessage(`1`)

	f.host.handleMessage(&Message{ID: &id, Method: "vllm:node/set-local-models", Params: inventory})

	assert.Empty(t, f.resolveCandidates("org/deleted-model"))
	assert.False(t, f.currentLocalBackend().LoadOnRequest)
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

func TestClusterIngress_PreparesTheRequestedModelOnTheServingNode(t *testing.T) {
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
	defer native.Close()
	serving, preparations := modelRequestFacade(t, native.URL, "")
	serving.host.mesh = clustertrust.Open(servingDir)
	ingress := httptest.NewUnstartedServer(http.HandlerFunc(serving.handleClusterIngress))
	ingress.TLS = serving.host.mesh.ServerTLSConfig()
	ingress.StartTLS()
	defer ingress.Close()
	caller := testProxy(serving.profile, NewDiscovery(), serving.profile.StandalonePort).soleFacade()
	caller.host.mesh = clustertrust.Open(callerDir)
	defer caller.host.closeIdleTransports()
	node := nodeFor(t, "serving", ingress.URL)
	node.ClusterUUID = "serving"
	node.Models = []string{"org/requested-model"}
	caller.discovery.SetSubscribed([]Node{node})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"org/requested-model","messages":[]}`))
	request.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()

	caller.handlePlain(recorder, request)

	assert.Equal(t, http.StatusOK, recorder.Code)
	require.Len(t, preparations.requests, 1)
	prepared := <-preparations.requests
	assert.Equal(t, "org/requested-model", prepared.Model)
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
