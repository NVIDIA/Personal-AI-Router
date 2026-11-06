// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

type captureRW struct {
	frames chan []byte
}

func newCaptureRW() *captureRW {
	return &captureRW{frames: make(chan []byte, 64)}
}

func (rw *captureRW) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (rw *captureRW) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	rw.frames <- cp
	return len(p), nil
}

type fakeRoundTripper struct {
	mu       sync.RWMutex
	handlers map[string]func(*http.Request) (*http.Response, error)
}

func newFakeRoundTripper() *fakeRoundTripper {
	return &fakeRoundTripper{handlers: make(map[string]func(*http.Request) (*http.Response, error))}
}

func (rt *fakeRoundTripper) set(method, host, path string, fn func(*http.Request) (*http.Response, error)) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.handlers[method+" "+host+path] = fn
}

func (rt *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	key := req.Method + " " + req.URL.Host + req.URL.Path
	rt.mu.RLock()
	fn := rt.handlers[key]
	rt.mu.RUnlock()
	if fn == nil {
		return nil, errors.New("unexpected request: " + key)
	}
	return fn(req)
}

func httpJSON(status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func newTestManager() (*Manager, *captureRW, *fakeRoundTripper) {
	rw := newCaptureRW()
	m := NewManager(NewCodec(rw), nil)
	rt := newFakeRoundTripper()
	m.client = &http.Client{Transport: rt, Timeout: time.Second}
	return m, rw, rt
}

// Configure a reachable engine and public hardware endpoint without cluster
// pins. The engine must remain usable, but the worker must never fetch or accept
// the public hardware claim as verified identity or scheduling measurements.
func TestProbeNode_PublicHardwareCannotSupplyServiceMeasurements(t *testing.T) {
	const address = "192.0.2.20"
	const modelName = "model-a"
	m, _, transport := newTestManager()
	entry := ManualEntry{Address: address, Name: "manual"}
	configureHealthyOllama(t, transport, entry.Address, []string{modelName})
	var hardwareReads int
	transport.set(http.MethodGet, address+":14318", "/v1/node-info", func(*http.Request) (*http.Response, error) {
		hardwareReads++
		return httpJSON(http.StatusOK, `{"hostUuid":"victim","telemetryValid":true}`)
	})
	m.nodes[nodeID(entry)] = &trackedNode{entry: entry}

	m.probeNode(entry)

	status := m.nodes[nodeID(entry)].status
	assert.Zero(t, hardwareReads)
	assert.Empty(t, status.HostUUID)
	assert.False(t, status.TelemetryValid)
	assert.True(t, status.OllamaUp)
	assert.Equal(t, []string{modelName}, status.OllamaModels)
}

func configureHealthyOllama(t *testing.T, rt *fakeRoundTripper, addr string, models []string) {
	t.Helper()
	host := net.JoinHostPort(addr, "11434")
	rt.set(http.MethodGet, host, "/", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, host, "/api/tags", func(*http.Request) (*http.Response, error) {
		var payload struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		for _, name := range models {
			payload.Models = append(payload.Models, struct {
				Name string `json:"name"`
			}{Name: name})
		}
		data, err := json.Marshal(payload)
		assert.NoError(t, err, "marshal model inventory")
		return httpJSON(http.StatusOK, string(data))
	})
}

// configureHealthyLMStudio registers a 200 GET /v1/models on addr:1234
// returning the given model ids in the OpenAI list shape, so probeLMStudio
// reports the node up with those models.
func configureHealthyLMStudio(t *testing.T, rt *fakeRoundTripper, addr string, models []string) {
	t.Helper()
	host := net.JoinHostPort(addr, "1234")
	rt.set(http.MethodGet, host, "/v1/models", func(*http.Request) (*http.Response, error) {
		var payload struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		payload.Object = "list"
		for _, id := range models {
			payload.Data = append(payload.Data, struct {
				ID string `json:"id"`
			}{ID: id})
		}
		data, err := json.Marshal(payload)
		assert.NoError(t, err, "marshal LM Studio model inventory")
		return httpJSON(http.StatusOK, string(data))
	})
}

// TestProbeLMStudioReportsModels covers the new LM Studio probe: a reachable
// server reports up with its model ids parsed from /v1/models, and an absent
// one (no handler registered, so the request errors) reports down.
func TestProbeLMStudio_ReportsModels(t *testing.T) {
	m, _, rt := newTestManager()
	configureHealthyLMStudio(t, rt, "node.local", []string{"qwen2.5-7b", "llama-3.1-8b"})

	up, models := m.probeLMStudio("node.local", lmStudioPort)
	require.True(t, up, "expected lmstudio up")
	assert.Equal(t, []string{"qwen2.5-7b", "llama-3.1-8b"}, models, "models")

	downUp, downModels := m.probeLMStudio("absent.local", lmStudioPort)
	require.False(t, downUp, "expected absent lmstudio down, got up (%v, %v)", downUp, downModels)
	assert.Nil(t, downModels, "expected absent lmstudio down, got up (%v, %v)", downUp, downModels)
}

func requestMessage(t *testing.T, id int, method string, params any) *Message {
	t.Helper()
	idData, err := json.Marshal(id)
	require.NoError(t, err, "marshal request ID")
	idRaw := json.RawMessage(idData)
	paramsRaw, err := json.Marshal(params)
	require.NoError(t, err, "marshal request params")
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: paramsRaw}
}

func requestMessageRaw(t *testing.T, id int, method string, params json.RawMessage) *Message {
	t.Helper()
	idData, err := json.Marshal(id)
	require.NoError(t, err, "marshal request ID")
	idRaw := json.RawMessage(idData)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: params}
}

func notificationMessage(t *testing.T, method string, params any) *Message {
	t.Helper()
	paramsRaw, err := json.Marshal(params)
	require.NoError(t, err, "marshal notification params")
	return &Message{JSONRPC: "2.0", Method: method, Params: paramsRaw}
}

func readCaptureFrame(t *testing.T, rw *captureRW) Message {
	t.Helper()
	select {
	case data := <-rw.frames:
		var msg Message
		require.NoError(t, json.Unmarshal(data, &msg), "decode frame %q", data)
		return msg
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for frame")
		return Message{}
	}
}

func readCaptureUntil(t *testing.T, rw *captureRW, match func(Message) bool) Message {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case data := <-rw.frames:
			var msg Message
			require.NoError(t, json.Unmarshal(data, &msg), "decode frame %q", data)
			if match(msg) {
				return msg
			}
		case <-deadline:
			require.FailNow(t, "timed out waiting for matching frame")
			return Message{}
		}
	}
}

func assertNoCaptureMethod(t *testing.T, rw *captureRW, method string) {
	t.Helper()
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case data := <-rw.frames:
			var msg Message
			require.NoError(t, json.Unmarshal(data, &msg), "decode frame %q", data)
			require.NotEqual(t, method, msg.Method, "unexpected (%v, %v)", method, msg)
		case <-timer.C:
			return
		}
	}
}

func decodeResult[T any](t *testing.T, msg Message) T {
	t.Helper()
	require.Nil(t, msg.Error, "unexpected RPC error")
	var result T
	require.NoError(t, json.Unmarshal(msg.Result, &result), "decode result")
	return result
}

func decodeParams[T any](t *testing.T, msg Message) T {
	t.Helper()
	var result T
	require.NoError(t, json.Unmarshal(msg.Params, &result), "decode params")
	return result
}

func responseWithID(t *testing.T, id int) func(Message) bool {
	t.Helper()
	return func(msg Message) bool {
		if msg.ID == nil {
			return false
		}
		var got int
		require.NoError(t, json.Unmarshal(*msg.ID, &got), "decode response ID")
		return got == id
	}
}

func methodIs(method string) func(Message) bool {
	return func(msg Message) bool { return msg.Method == method }
}

func sampleInfo() NodeInfoResponse {
	return NodeInfoResponse{
		NodeInfo: noderec.NodeInfo{
			GPUs: []GPUInfo{{
				Name:               "RTX 6000",
				VramBytes:          48 << 30,
				VramUsedBytes:      12 << 30,
				UtilizationPercent: 42,
			}},
			CPU:            &CPUInfo{Name: "Threadripper", Cores: 64, UtilizationPercent: 7},
			Memory:         &MemoryInfo{TotalBytes: 128 << 30, UsedBytes: 32 << 30},
			TelemetryValid: true,
			MSSince:        137,
		},
	}
}

func TestNodeID_PrefersConfiguredNameOverAddressFallback(t *testing.T) {
	assert.Equal(t, "workstation", nodeID(ManualEntry{Name: "workstation", Address: "10.0.0.5"}), "named nodeID")
	assert.Equal(t, "manual:10.0.0.5", nodeID(ManualEntry{Address: "10.0.0.5"}), "unnamed nodeID")
}

func TestNodeAdd_RespondsWithInitialStatusAndDiscoversProbeResult(t *testing.T) {
	m, rw, rt := newTestManager()
	configureHealthyOllama(t, rt, "node.local", []string{"llama3", "mistral"})

	m.handleMessage(requestMessage(t, 1, "node/add", ManualEntry{Address: "node.local", Name: "lab"}))

	var discovered Message
	resp := readCaptureUntil(t, rw, func(msg Message) bool {
		if msg.Method == "node/discovered" {
			discovered = msg
		}
		return responseWithID(t, 1)(msg)
	})
	initial := decodeResult[ManualNodeStatus](t, resp)
	assert.Equal(t, "lab", initial.ID, "initial status (%v)", initial)
	assert.Equal(t, "node.local", initial.Address, "initial status (%v)", initial)
	assert.Equal(t, 11434, initial.OllamaPort, "default ports not set (%v)", initial)
	assert.Equal(t, 1234, initial.LMStudioPort)
	assert.Equal(t, 14318, initial.NodeInfoPort, "default ports not set (%v)", initial)
	assert.False(t, initial.OllamaUp, "initial status should be unprobed (%v)", initial)
	assert.False(t, initial.LMStudioUp)
	assert.False(t, initial.NodeInfoUp, "initial status should be unprobed (%v)", initial)

	if discovered.Method == "" {
		discovered = readCaptureUntil(t, rw, methodIs("node/discovered"))
	}
	status := decodeParams[ManualNodeStatus](t, discovered)
	assert.True(t, status.OllamaUp, "discovered status did not include healthy services (%v)", status)
	assert.False(t, status.NodeInfoUp)
	assert.Equal(t, []string{"llama3", "mistral"}, status.OllamaModels)
	assert.Empty(t, status.GPUs)
	assert.False(t, status.TelemetryValid)
}

func TestNodeAdd_RejectsInvalidParameters(t *testing.T) {
	m, rw, _ := newTestManager()

	test := func(name string, request *Message) {
		t.Run(name, func(t *testing.T) {
			m.handleMessage(request)
			resp := readCaptureFrame(t, rw)
			require.NotNil(t, resp.Error)
			assert.Equal(t, -32602, resp.Error.Code)
		})
	}
	test("malformed params", requestMessageRaw(t, 1, "node/add", json.RawMessage(`"bad"`)))
	test("missing address", requestMessage(t, 2, "node/add", ManualEntry{}))

	require.Empty(t, m.listNodes(), "validation errors added nodes")
}

func TestNodesList_ReturnsCurrentStatuses(t *testing.T) {
	m, rw, rt := newTestManager()
	configureHealthyOllama(t, rt, "node.local", []string{"llama3"})

	m.handleMessage(requestMessage(t, 1, "node/add", ManualEntry{Address: "node.local"}))
	_ = readCaptureUntil(t, rw, methodIs("node/discovered"))

	m.handleMessage(requestMessage(t, 2, "nodes/list", nil))
	resp := readCaptureUntil(t, rw, responseWithID(t, 2))
	var result struct {
		Nodes []ManualNodeStatus `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &result), "decode nodes/list")
	require.Len(t, result.Nodes, 1, "nodes/list length")
	assert.True(t, result.Nodes[0].OllamaUp, "nodes/list status")
	assert.False(t, result.Nodes[0].NodeInfoUp)
}

func TestNodeRemove_ReturnsRemovedAndNotifies(t *testing.T) {
	m, rw, _ := newTestManager()
	m.nodes["lab"] = &trackedNode{
		entry:  ManualEntry{Name: "lab", Address: "node.local"},
		status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318},
	}

	m.handleMessage(requestMessage(t, 1, "node/remove", map[string]string{"id": "lab"}))
	removed := readCaptureFrame(t, rw)
	assert.Equal(t, "node/removed", removed.Method, "first remove frame (%v)", removed)
	status := decodeParams[ManualNodeStatus](t, removed)
	assert.Equal(t, "lab", status.ID, "removed params (%v)", status)
	resp := readCaptureUntil(t, rw, responseWithID(t, 1))
	result := decodeResult[map[string]bool](t, resp)
	assert.True(t, result["removed"], "removed result (%v)", result)
	require.Empty(t, m.listNodes(), "node still listed after removal")

	m.handleMessage(requestMessage(t, 2, "node/remove", map[string]string{"id": "lab"}))
	resp = readCaptureUntil(t, rw, responseWithID(t, 2))
	result = decodeResult[map[string]bool](t, resp)
	assert.False(t, result["removed"], "second removal result (%v)", result)
	assertNoCaptureMethod(t, rw, "node/removed")
}

func TestAddThenRemoveBeforeInitialProbe_DoesNotRediscover(t *testing.T) {
	m, rw, rt := newTestManager()
	started := make(chan struct{})
	release := make(chan struct{})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/", func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[]}`)
	})

	status := m.addNode(ManualEntry{Address: "node.local", Name: "lab"})
	assert.Equal(t, "lab", status.ID, "add status (%v)", status)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "initial probe did not start")
	}

	assert.True(t, m.removeNode("lab"), "removeNode returned false")
	_ = readCaptureUntil(t, rw, methodIs("node/removed"))
	close(release)

	assertNoCaptureMethod(t, rw, "node/discovered")
	require.Empty(t, m.listNodes(), "node rediscovered in state")
}

func TestProbeNode_EmitsUpdatedOnStateChange(t *testing.T) {
	m, rw, rt := newTestManager()
	m.nodes["lab"] = &trackedNode{entry: ManualEntry{Name: "lab", Address: "node.local"}, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}

	configureHealthyOllama(t, rt, "node.local", []string{"llama3"})
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	first := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.Equal(t, []string{"llama3"}, first.OllamaModels, "first update models")

	configureHealthyOllama(t, rt, "node.local", []string{"mistral"})
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	second := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.Equal(t, []string{"mistral"}, second.OllamaModels, "second update models")
}

func TestProbeNode_EmitsNothingWhenStateIsUnchanged(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{entry: entry, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}
	configureHealthyOllama(t, rt, "node.local", []string{"llama3"})

	m.probeNode(entry)
	_ = readCaptureUntil(t, rw, methodIs("node/updated"))
	m.probeNode(entry)

	assertNoCaptureMethod(t, rw, "node/updated")
}

func TestProbeNode_DiscardsInvalidAuthenticatedHardware(t *testing.T) {
	test := func(name string, respond func(*testing.T, http.ResponseWriter, []byte)) {
		t.Run(name, func(t *testing.T) {
			f := newManualPeerFixture(t)
			engineTransport, ok := f.manager.client.Transport.(*fakeRoundTripper)
			require.True(t, ok)
			configureHealthyLMStudio(t, engineTransport, f.entry.Address, []string{"model-a"})
			f.manager.probeNode(f.entry)
			initial := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, f.frames, methodIs("node/updated")))
			require.Equal(t, noderec.IdentityPinned, initial.IdentityVerification)
			require.NotEmpty(t, initial.DiscoveryRecordID)
			require.True(t, initial.NodeInfoUp)
			require.NotEmpty(t, initial.GPUs)
			require.NotNil(t, initial.CPU)
			require.NotNil(t, initial.Memory)
			body, err := json.Marshal(f.info)
			require.NoError(t, err)
			requested := make(chan struct{}, 1)
			f.respond = func(w http.ResponseWriter) {
				requested <- struct{}{}
				respond(t, w, body)
			}

			f.manager.probeNode(f.entry)

			select {
			case <-requested:
			default:
				t.Fatal("the authenticated hardware request did not reach the peer")
			}
			updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, f.frames, methodIs("node/updated")))
			assert.Equal(t, noderec.IdentityUnverified, updated.IdentityVerification)
			assert.Empty(t, updated.DiscoveryRecordID)
			assert.Empty(t, updated.HostUUID)
			assert.False(t, updated.NodeInfoUp)
			assert.Empty(t, updated.GPUs)
			assert.Nil(t, updated.CPU)
			assert.Nil(t, updated.Memory)
			assert.False(t, updated.TelemetryValid)
			assert.Zero(t, updated.MSSince)
			assert.True(t, updated.LMStudioUp)
			assert.Equal(t, []string{"model-a"}, updated.LMStudioModels)
		})
	}
	test("malformed JSON", func(t *testing.T, w http.ResponseWriter, _ []byte) {
		_, err := w.Write([]byte(`{not json`))
		assert.NoError(t, err)
	})
	test("truncated response with valid JSON", func(t *testing.T, w http.ResponseWriter, body []byte) {
		// The JSON is complete, but fewer bytes than promised must still fail.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+1))
		_, err := w.Write(body)
		assert.NoError(t, err)
	})
	test("response larger than 64 KiB", func(t *testing.T, w http.ResponseWriter, body []byte) {
		// Valid trailing whitespace isolates the size limit from JSON parsing.
		oversized := append(body, []byte(strings.Repeat(" ", (64<<10)+1-len(body)))...)
		_, err := w.Write(oversized)
		assert.NoError(t, err)
	})
}

const manualPeerUUID = "peer"

type manualPeerFixture struct {
	manager        *Manager
	selfDir        string
	peerDir        string
	frames         *captureRW
	entry          ManualEntry
	info           NodeInfoResponse
	server         *httptest.Server
	beforeResponse func()
	respond        func(http.ResponseWriter)
}

// Create isolated cluster identities pinned in both directions and a local
// mTLS node-info server. The manager uses that real authenticated connection
// while engine probes remain fake, so the fixture never needs a remote host.
func newManualPeerFixture(t *testing.T) *manualPeerFixture {
	t.Helper()
	const clusterID = "test-cluster"
	const selfUUID = "self"
	clientDir, serverDir := t.TempDir(), t.TempDir()
	clustertrusttest.Join(t, clientDir, clusterID, selfUUID)
	clustertrusttest.Join(t, serverDir, clusterID, manualPeerUUID)
	pin := func(destination, source, uuid string) {
		t.Helper()
		certificate, err := os.ReadFile(filepath.Join(source, "node.crt"))
		require.NoError(t, err)
		clustertrusttest.WritePeerPinFromCertificate(t, destination, uuid, certificate)
	}
	pin(clientDir, serverDir, manualPeerUUID)
	pin(serverDir, clientDir, selfUUID)
	manager, frames, _ := newTestManager()
	manager.mesh = clustertrust.Open(clientDir)
	manager.peerClients = clustertrust.NewPeerClientPool(manager.mesh, probeTimeout)
	t.Cleanup(manager.peerClients.CloseIdle)
	f := &manualPeerFixture{
		manager: manager,
		selfDir: clientDir,
		peerDir: serverDir,
		frames:  frames,
		info:    sampleInfo(),
	}
	f.info.HostUUID = manualPeerUUID
	principal := manualPeerUUID
	f.info.ClusterUUID = &principal
	peerMesh := clustertrust.Open(serverDir)
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := peerMesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if f.beforeResponse != nil {
			f.beforeResponse()
		}
		if f.respond != nil {
			f.respond(w)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(f.info))
	}))
	f.server.TLS = peerMesh.ServerTLSConfig()
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	host, port, err := net.SplitHostPort(f.server.Listener.Addr().String())
	require.NoError(t, err)
	portNumber, err := strconv.Atoi(port)
	require.NoError(t, err)
	f.entry = ManualEntry{
		Address: host,
		Name:    "manual-peer",
		TLSPort: portNumber,
	}
	manager.nodes[nodeID(f.entry)] = &trackedNode{entry: f.entry}
	return f
}

func TestProbeNode_DiscardsResultsAfterIdentityOrEntryChanges(t *testing.T) {
	test := func(name string, change func(*testing.T, *manualPeerFixture)) {
		t.Run(name, func(t *testing.T) {
			f := newManualPeerFixture(t)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			f.beforeResponse = func() { close(entered); <-release }
			go func() { f.manager.probeNode(f.entry); close(done) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				unblock()
				t.Fatal("authenticated request did not reach the server")
			}
			change(t, f)
			unblock()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("probe did not finish")
			}

			status := f.manager.listNodes()
			require.Len(t, status, 1)
			assert.NotEqual(t, noderec.IdentityPinned, status[0].IdentityVerification)
			assert.False(t, status[0].TelemetryValid)
			assert.Empty(t, status[0].HostUUID)
		})
	}
	test("local certificate replaced during read", func(t *testing.T, f *manualPeerFixture) {
		clustertrusttest.WriteKeypair(t, f.selfDir, "self")
	})
	test("local node readmitted during read", func(t *testing.T, f *manualPeerFixture) {
		clustertrusttest.WriteAdmission(t, f.selfDir, "test-cluster", 2)
	})
	test("peer pin removed during read", func(t *testing.T, f *manualPeerFixture) {
		clustertrusttest.RemovePeerPin(t, f.selfDir, manualPeerUUID)
	})
	test("peer pin replaced during read", func(t *testing.T, f *manualPeerFixture) {
		clustertrusttest.WritePeerPin(t, f.selfDir, manualPeerUUID)
	})
	test("entry replaced under the same ID during read", func(t *testing.T, f *manualPeerFixture) {
		f.manager.mu.Lock()
		defer f.manager.mu.Unlock()
		// Removing and re-adding the same alias creates a new tracked entry.
		f.manager.nodes[nodeID(f.entry)] = &trackedNode{entry: f.entry}
	})
}

func TestProbeNode_DiscardsInFlightHardwareAfterPeerReadmission(t *testing.T) {
	// A new admission must reject an earlier response even when its certificate
	// is unchanged. Inspect the emitted update before cached reads reconcile trust.
	f := newManualPeerFixture(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	f.beforeResponse = func() { close(entered); <-release }
	go func() { f.manager.probeNode(f.entry); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		unblock()
		t.Fatal("authenticated request did not reach the server")
	}
	pinPath := filepath.Join(f.selfDir, "trusted", manualPeerUUID+".json")
	data, err := os.ReadFile(pinPath)
	require.NoError(t, err)
	var pin map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &pin))
	pin["admissionEpoch"] = json.RawMessage(`2`)
	pin["pinnedAt"] = json.RawMessage(`1000`)
	data, err = json.Marshal(pin)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pinPath, data, 0o600))
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not finish")
	}

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, f.frames, methodIs("node/updated")))
	assert.Equal(t, noderec.IdentityUnverified, updated.IdentityVerification)
	assert.Empty(t, updated.DiscoveryRecordID)
	assert.Empty(t, updated.HostUUID)
	assert.False(t, updated.NodeInfoUp)
	assert.Empty(t, updated.GPUs)
	assert.Nil(t, updated.CPU)
	assert.Nil(t, updated.Memory)
	assert.False(t, updated.TelemetryValid)
	assert.Zero(t, updated.MSSince)
}

func TestListNodes_DiscardsHardwareAfterPinRemoval(t *testing.T) {
	f := newManualPeerFixture(t)
	f.manager.probeNode(f.entry)

	clustertrusttest.RemovePeerPin(t, f.selfDir, manualPeerUUID)
	status := f.manager.listNodes()

	require.Len(t, status, 1)
	assert.Equal(t, noderec.IdentityUnverified, status[0].IdentityVerification)
	assert.False(t, status[0].NodeInfoUp)
	assert.Empty(t, status[0].GPUs)
	assert.False(t, status[0].TelemetryValid)
}

func TestListNodes_PreservesTimeElapsedSinceHardwareProbe(t *testing.T) {
	f := newManualPeerFixture(t)
	f.manager.probeNode(f.entry)
	first := f.manager.listNodes()
	require.Len(t, first, 1)
	time.Sleep(25 * time.Millisecond)

	second := f.manager.listNodes()

	require.Len(t, second, 1)
	assert.GreaterOrEqual(t, second[0].MSSince, first[0].MSSince+25)
}

// Probe a mutually pinned peer and check that its hardware and host identity
// enter the manual status together, with sample age retained across the fetch.
func TestProbeNode_AcceptsMeasurementsFromPinnedPeer(t *testing.T) {
	f := newManualPeerFixture(t)

	f.manager.probeNode(f.entry)

	status := f.manager.listNodes()
	require.Len(t, status, 1)
	assert.Equal(t, noderec.IdentityPinned, status[0].IdentityVerification)
	assert.Equal(t, manualPeerUUID, status[0].HostUUID)
	assert.Equal(t, f.info.GPUs, status[0].GPUs)
	assert.True(t, status[0].TelemetryValid)
	assert.GreaterOrEqual(t, status[0].MSSince, int64(137))
}

func TestProbeNode_DiscardsOldIdentityAfterHostnameReassignment(t *testing.T) {
	f := newManualPeerFixture(t)
	f.manager.probeNode(f.entry)
	initial := f.manager.listNodes()
	require.Len(t, initial, 1)
	require.Equal(t, noderec.IdentityPinned, initial[0].IdentityVerification)

	client, ok := f.manager.peerClients.Client(manualPeerUUID)
	require.True(t, ok)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	engineTransport, ok := f.manager.client.Transport.(*fakeRoundTripper)
	require.True(t, ok)

	// Resolve the manual hostname inside the dialer so reassignment is local
	// and deterministic. The first server stays alive throughout the test.
	var targetMu sync.Mutex
	target := f.server.Listener.Addr().String()
	var dialed []string
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		targetMu.Lock()
		address := target
		dialed = append(dialed, address)
		targetMu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	f.entry.Address = "manual.example"
	f.manager.nodes[nodeID(f.entry)].entry = f.entry
	configureHealthyOllama(t, engineTransport, f.entry.Address, []string{"original-model"})
	f.manager.probeNode(f.entry)
	warmed := f.manager.listNodes()
	require.Len(t, warmed, 1)
	require.Equal(t, manualPeerUUID, warmed[0].HostUUID)
	require.True(t, warmed[0].NodeInfoUp)

	// The replacement has a different certificate. A fresh connection must
	// reject it rather than keep attributing its engine to the original host.
	replacement := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(replacement.Close)
	targetMu.Lock()
	target = replacement.Listener.Addr().String()
	targetMu.Unlock()
	configureHealthyOllama(t, engineTransport, f.entry.Address, []string{"replacement-model"})

	f.manager.probeNode(f.entry)

	status := f.manager.listNodes()
	require.Len(t, status, 1)
	assert.Equal(t, []string{"replacement-model"}, status[0].OllamaModels)
	assert.Equal(t, noderec.IdentityUnverified, status[0].IdentityVerification)
	assert.False(t, status[0].NodeInfoUp)
	assert.Empty(t, status[0].HostUUID)
	assert.Empty(t, status[0].GPUs)
	assert.False(t, status[0].TelemetryValid)
	targetMu.Lock()
	assert.Equal(t, []string{f.server.Listener.Addr().String(), replacement.Listener.Addr().String()}, dialed)
	targetMu.Unlock()
}

// Make the manual endpoint present our own certificate. Even a successful TLS
// connection must not let network measurements impersonate the owned local
// worker or grant this manual entry verified local identity.
func TestProbeNode_LocalIdentityCannotSupplyMeasurementsOverHTTP(t *testing.T) {
	f := newManualPeerFixture(t)
	// The entered endpoint now presents our own identity. Local measurements
	// must come from the owned worker, even when this TLS handshake succeeds.
	f.manager.mesh = clustertrust.Open(f.peerDir)
	f.manager.peerClients = clustertrust.NewPeerClientPool(f.manager.mesh, probeTimeout)
	t.Cleanup(f.manager.peerClients.CloseIdle)

	f.manager.probeNode(f.entry)

	status := f.manager.listNodes()
	require.Len(t, status, 1)
	assert.Equal(t, noderec.IdentityUnverified, status[0].IdentityVerification)
	assert.False(t, status[0].TelemetryValid)
	assert.Empty(t, status[0].HostUUID)
}

// Repeated authenticated reads must each publish a snapshot, even when hardware
// values stay identical, so downstream caches continue to observe live samples.
func TestProbeNode_PublishesUnchangedAuthenticatedMeasurements(t *testing.T) {
	const updatedMethod = "node/updated"
	f := newManualPeerFixture(t)
	// Invalid measurements always have age zero, so neither age nor inventory
	// changes can accidentally trigger this periodic notification.
	f.info.TelemetryValid, f.info.MSSince = false, 0
	f.manager.probeNode(f.entry)
	first := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, f.frames, methodIs(updatedMethod)))
	assert.Equal(t, f.manager.nodes[nodeID(f.entry)].observedAt.UnixMilli(), first.ObservedAt)
	assert.Equal(t, first.ObservedAt, f.manager.listNodes()[0].ObservedAt, "cached delivery keeps the original observation time")

	f.manager.probeNode(f.entry)

	message := readCaptureUntil(t, f.frames, methodIs(updatedMethod))
	status := decodeParams[ManualNodeStatus](t, message)
	assert.Equal(t, noderec.IdentityPinned, status.IdentityVerification)
	assert.False(t, status.TelemetryValid)
	assert.Equal(t, f.manager.nodes[nodeID(f.entry)].observedAt.UnixMilli(), status.ObservedAt)
}

// Keep a valid pinned TLS connection but vary the response's host and principal
// claims. A conflicting or missing identity must leave the manual entry
// unverified and discard its hardware, despite the successful TLS handshake.
func TestProbeNode_RejectsConflictingPinnedResponseIdentity(t *testing.T) {
	test := func(name, host, principal string) {
		t.Run(name, func(t *testing.T) {
			f := newManualPeerFixture(t)
			f.info.HostUUID, f.info.ClusterUUID = host, &principal

			f.manager.probeNode(f.entry)

			status := f.manager.listNodes()
			require.Len(t, status, 1)
			assert.Equal(t, noderec.IdentityUnverified, status[0].IdentityVerification)
			assert.Empty(t, status[0].HostUUID)
			assert.False(t, status[0].TelemetryValid)
		})
	}
	test("another host", "different-host", manualPeerUUID)
	test("another principal", manualPeerUUID, "different-principal")
	test("missing principal", manualPeerUUID, "")
}

func TestProbeNode_PublishesCPUAndMemoryAvailabilityChanges(t *testing.T) {
	test := func(name string, beforeCPU, afterCPU *CPUInfo, beforeMemory, afterMemory *MemoryInfo) {
		t.Run(name, func(t *testing.T) {
			f := newManualPeerFixture(t)
			f.info.CPU, f.info.Memory = beforeCPU, beforeMemory
			f.info.TelemetryValid, f.info.MSSince = false, 0
			f.manager.probeNode(f.entry)
			readCaptureUntil(t, f.frames, methodIs("node/updated"))

			f.info.CPU, f.info.Memory = afterCPU, afterMemory
			f.manager.probeNode(f.entry)

			updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, f.frames, methodIs("node/updated")))
			assert.Equal(t, noderec.IdentityPinned, updated.IdentityVerification)
			assert.True(t, updated.NodeInfoUp)
			assert.Equal(t, afterCPU, updated.CPU)
			assert.Equal(t, afterMemory, updated.Memory)
		})
	}
	test("CPU becomes available", nil, &CPUInfo{Name: "cpu", Cores: 8}, nil, nil)
	test("CPU becomes unavailable", &CPUInfo{Name: "cpu", Cores: 8}, nil, nil, nil)
	test("memory becomes available", nil, nil, nil, &MemoryInfo{TotalBytes: 10, UsedBytes: 5})
	test("memory becomes unavailable", nil, nil, &MemoryInfo{TotalBytes: 10, UsedBytes: 5}, nil)
}

func TestUnknownMethod_ReturnsMethodNotFound(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(t, 1, "bogus", nil))
	resp := readCaptureFrame(t, rw)
	require.NotNil(t, resp.Error, "unknown method error")
	assert.Equal(t, -32601, resp.Error.Code, "unknown method error")
}

func TestLogSetLevel_AppliesRequestedVerbosity(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(t, 1, applog.SetLevelMethod, applog.SetLevelParams{Level: "debug"}))
	resp := readCaptureFrame(t, rw)
	result := decodeResult[map[string]string](t, resp)
	assert.Equal(t, "debug", result["level"], "log/set-level result (%v)", result)

	m.handleMessage(requestMessage(t, 2, applog.SetLevelMethod, applog.SetLevelParams{Level: "not-a-level"}))
	resp = readCaptureFrame(t, rw)
	require.NotNil(t, resp.Error, "invalid log/set-level error")
	assert.Equal(t, -32602, resp.Error.Code, "invalid log/set-level error")
}

func TestShutdownRequest_CancelsRun(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	mgr := NewManager(NewCodec(server), nil)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- mgr.Run(ctx)
	}()

	reader := bufio.NewReader(client)
	ready := readPipeFrame(t, client, reader)
	assert.Equal(t, "ready", ready.Method, "first frame (%v)", ready)

	writePipeRequest(t, client, 7, "shutdown", nil)
	resp := readPipeFrame(t, client, reader)
	assert.True(t, responseWithID(t, 7)(resp), "shutdown response (%v)", resp)
	require.Nil(t, resp.Error, "shutdown response (%v)", resp)

	select {
	case err := <-done:
		require.NoError(t, err, "Run returned error")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Run did not return after shutdown")
	}
}

func TestNotification_IsIgnored(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(notificationMessage(t, "node/add", ManualEntry{Address: "node.local"}))
	require.Empty(t, m.listNodes(), "notification mutated state")
	assertNoCaptureMethod(t, rw, "")
}

func readPipeFrame(t *testing.T, conn net.Conn, reader *bufio.Reader) Message {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)), "set read deadline")
	line, err := reader.ReadBytes('\n')
	require.NoError(t, err, "read pipe frame")
	var msg Message
	require.NoError(t, json.Unmarshal(line, &msg), "decode pipe frame %q", line)
	return msg
}

func writePipeRequest(t *testing.T, conn net.Conn, id int, method string, params any) {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		require.NoError(t, err, "marshal params")
	}
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  raw,
	}
	data, err := json.Marshal(msg)
	require.NoError(t, err, "marshal request")
	data = append(data, '\n')
	_, err = conn.Write(data)
	require.NoError(t, err, "write request")
}

func manualPreviewEntry(t *testing.T, server *httptest.Server) (ManualEntry, ManualNodeStatus) {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	portNumber, err := strconv.Atoi(port)
	require.NoError(t, err)
	entry := ManualEntry{Address: host, Name: "preview-host"}
	return entry, ManualNodeStatus{
		ID: nodeID(entry), Name: entry.Name, Address: host,
		IdentityVerification: noderec.IdentityUnverified,
		NodeInfoPort:         portNumber,
	}
}

// A hardware preview is useful even while the node's services are unavailable.
// A successful public read must update only display data, without verifying the
// entry, supplying service measurements, or hiding an ongoing service outage.
func TestManagerPreviewAll_UpdatesDisplayWithoutChangingServiceState(t *testing.T) {
	public := sampleInfo().NodeInfo
	public.HostUUID = "forged-peer"
	principal := "forged-principal"
	public.ClusterUUID = &principal
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/node-info", r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(public))
	}))
	t.Cleanup(server.Close)
	entry, status := manualPreviewEntry(t, server)
	m, frames, _ := newTestManager()
	m.nodes[status.ID] = &trackedNode{entry: entry, status: status, consecutiveFails: 2}

	m.previewAll(context.Background())

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, frames, methodIs("node/updated")))
	require.NotNil(t, updated.Preview)
	assert.Equal(t, public.GPUs, updated.Preview.GPUs)
	assert.Equal(t, public.CPU, updated.Preview.CPU)
	assert.Equal(t, public.Memory, updated.Preview.Memory)

	// A public response can supply hardware details, but none of its identity
	// or service metadata is allowed to become an authenticated observation.
	assert.Empty(t, updated.Preview.HostUUID)
	assert.Nil(t, updated.Preview.ClusterUUID)
	assert.Nil(t, updated.Preview.Peer)
	assert.Equal(t, noderec.IdentityUnverified, updated.IdentityVerification)
	assert.Empty(t, updated.HostUUID)
	assert.Empty(t, updated.DiscoveryRecordID)

	assert.Empty(t, updated.GPUs)
	assert.Nil(t, updated.CPU)
	assert.Nil(t, updated.Memory)
	assert.False(t, updated.TelemetryValid)

	// Preview success must not turn a service outage into a healthy status or
	// reset the independent service probe-failure counter.
	assert.False(t, updated.NodeInfoUp)
	assert.False(t, updated.OllamaUp)
	assert.False(t, updated.LMStudioUp)
	assert.Equal(t, 2, m.nodes[status.ID].consecutiveFails)
}

// Five manual entries need previews, but only four reads may run at once.
// Hold the first responses open to check the limit, then release them and verify
// that the remaining entry is read rather than skipped from the batch.
func TestManagerPreviewAll_LimitsConcurrentReadsWithoutSkippingEntries(t *testing.T) {
	const readerLimit = 4
	started := make(chan struct{}, 5)
	release := make(chan struct{})
	releaseReads := sync.OnceFunc(func() { close(release) })
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(sampleInfo().NodeInfo))
	}))
	t.Cleanup(server.Close)
	baseEntry, baseStatus := manualPreviewEntry(t, server)
	m, _, _ := newTestManager()
	for i := range 5 {
		entry, status := baseEntry, baseStatus
		entry.Name = fmt.Sprintf("preview-%d", i)
		status.ID, status.Name = nodeID(entry), entry.Name
		m.nodes[status.ID] = &trackedNode{entry: entry, status: status}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.previewAll(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		releaseReads()
		<-done
	})

	for range readerLimit {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("four manual preview requests did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("a fifth manual preview request started while all four readers were busy")
	case <-time.After(100 * time.Millisecond):
	}

	releaseReads()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("manual preview batch did not finish after responses were released")
	}

	assert.Equal(t, int32(5), reads.Load())
}

// A failed public read must preserve the last display sample with its original
// age. The next batch retries immediately, and a successful retry replaces that
// preview without changing the independent service probe-failure counter.
func TestManagerPreviewAll_PreservesCachedPreviewUntilRetrySucceeds(t *testing.T) {
	var available atomic.Bool
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		if !available.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(noderec.NodeInfo{
			GPUs: []GPUInfo{{Name: "new preview GPU"}}, TelemetryValid: true, MSSince: 137,
		}))
	}))
	t.Cleanup(server.Close)
	entry, status := manualPreviewEntry(t, server)
	preview := sampleInfo().NodeInfo
	status.Preview = &preview
	observedAt := time.Now()
	m, frames, _ := newTestManager()
	m.nodes[status.ID] = &trackedNode{
		entry: entry, status: status, previewObservedAt: observedAt, consecutiveFails: 2,
	}

	m.previewAll(context.Background())

	afterFailure := m.nodes[status.ID].statusAt(observedAt.Add(5 * time.Second))
	require.NotNil(t, afterFailure.Preview)
	assert.Equal(t, preview.GPUs, afterFailure.Preview.GPUs)
	assert.Equal(t, int64(5137), afterFailure.Preview.MSSince)
	assert.Equal(t, 2, m.nodes[status.ID].consecutiveFails)

	select {
	case frame := <-frames.frames:
		t.Fatalf("failed preview read emitted a status update: %s", frame)
	default:
	}

	available.Store(true)
	m.previewAll(context.Background())

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, frames, methodIs("node/updated")))
	require.NotNil(t, updated.Preview)
	assert.Equal(t, []GPUInfo{{Name: "new preview GPU"}}, updated.Preview.GPUs)
	assert.Equal(t, int32(2), reads.Load())

	assert.Equal(t, 2, m.nodes[status.ID].consecutiveFails)
}

// An entry may be unverified because it has never authenticated or because its
// earlier trust was withdrawn. Only the former may use public previews; explicit
// TLS or mTLS settings also prevent plaintext collection.
func TestManagerPreviewAll_ReadsPublicHardwareOnlyForEligibleEntries(t *testing.T) {
	test := func(name string, tlsPort int, mtls bool, verification noderec.IdentityVerification, everAuthenticated bool, wantReads int32) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				assert.NoError(t, json.NewEncoder(w).Encode(sampleInfo().NodeInfo))
			}))
			t.Cleanup(server.Close)
			entry, status := manualPreviewEntry(t, server)
			entry.TLSPort, entry.MTLS = tlsPort, mtls
			status.IdentityVerification = verification
			m, _, _ := newTestManager()
			m.nodes[status.ID] = &trackedNode{entry: entry, status: status, everAuthenticated: everAuthenticated}

			m.previewAll(context.Background())

			assert.Equal(t, wantReads, reads.Load())
		})
	}

	test("unpaired entry permits a display preview", 0, false, noderec.IdentityUnverified, false, 1)
	test("explicit TLS port prevents plaintext preview", 14318, false, noderec.IdentityUnverified, false, 0)
	test("explicit mutual TLS hint prevents plaintext preview", 0, true, noderec.IdentityUnverified, false, 0)
	test("verified peer does not use public hardware", 0, false, noderec.IdentityPinned, false, 0)
	test("withdrawn or failed peer does not fall back to public hardware", 0, false, noderec.IdentityUnverified, true, 0)
}

// A target can become obsolete while waiting for a free reader. It must be
// checked before HTTP starts, so a queued preview cannot contact a replacement
// entry or a peer that has already authenticated.
func TestManagerPreview_SkipsTargetsRetiredBeforeRead(t *testing.T) {
	test := func(name string, retire func(*Manager, string)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				assert.NoError(t, json.NewEncoder(w).Encode(sampleInfo().NodeInfo))
			}))
			t.Cleanup(server.Close)
			entry, status := manualPreviewEntry(t, server)
			m, _, _ := newTestManager()
			tracked := &trackedNode{entry: entry, status: status}
			m.nodes[status.ID] = tracked
			target := previewTarget{id: status.ID, entry: entry, port: status.NodeInfoPort, tracked: tracked}

			retire(m, status.ID)
			m.refreshPreview(context.Background(), target)

			assert.Zero(t, reads.Load())
		})
	}

	test("removed entry", func(m *Manager, id string) { delete(m.nodes, id) })
	test("replacement with the same configuration", func(m *Manager, id string) {
		old := m.nodes[id]
		m.nodes[id] = &trackedNode{entry: old.entry, status: old.status}
	})
	test("authenticated entry", func(m *Manager, id string) { m.nodes[id].status.IdentityVerification = noderec.IdentityPinned })
}

// newBlockedPreviewServer owns the response gate and releases it before closing
// the server, including when a test fails before it can unblock the request.
func newBlockedPreviewServer(t *testing.T, info noderec.NodeInfo) (server *httptest.Server, waitForRequest, release func()) {
	t.Helper()
	started, released := make(chan struct{}), make(chan struct{})
	release = sync.OnceFunc(func() { close(released) })
	markStarted := sync.OnceFunc(func() { close(started) })
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markStarted()
		select {
		case <-released:
		case <-r.Context().Done():
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(info))
	}))
	t.Cleanup(func() {
		release()
		server.Close()
	})
	waitForRequest = func() {
		t.Helper()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("public hardware request did not reach the server")
		}
	}
	return server, waitForRequest, release
}

// Complete an HTTP response while status processing is locked. Both the emitted
// preview and later status reads must include that wait in the sample's age.
func TestManagerPreviewAll_PreservesAgeWhileWaitingForStatusLock(t *testing.T) {
	server, waitForRequest, unblock := newBlockedPreviewServer(t, noderec.NodeInfo{TelemetryValid: true, MSSince: 137})
	entry, status := manualPreviewEntry(t, server)
	m, frames, _ := newTestManager()
	m.nodes[status.ID] = &trackedNode{entry: entry, status: status}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.previewAll(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		unblock()
		<-done
	})
	waitForRequest()

	m.mu.Lock()
	releasedAt := time.Now()
	unblock()
	// Let HTTP complete while publication is blocked. Use the actual interval
	// below so a slow test runner does not depend on an arbitrary upper bound.
	time.Sleep(100 * time.Millisecond)
	beforeUnlock := time.Now()
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("preview refresh did not finish after status processing resumed")
	}

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, frames, methodIs("node/updated")))
	require.NotNil(t, updated.Preview)
	// Request and cache durations are rounded to milliseconds separately.
	assert.GreaterOrEqual(t, updated.Preview.MSSince, int64(137)+beforeUnlock.Sub(releasedAt).Milliseconds()-1)

	later := m.listNodes()
	require.Len(t, later, 1)
	require.NotNil(t, later[0].Preview)
	assert.GreaterOrEqual(t, later[0].Preview.MSSince, updated.Preview.MSSince)
}

// Hold the HTTP response until the manual entry is removed, replaced, or
// authenticated. Its late hardware must be discarded, including when a new
// entry has the same ID and address as the one that started the request.
func TestManagerPreviewAll_DiscardsResultsAfterEntryOrVerificationChanges(t *testing.T) {
	test := func(name string, change func(*Manager, string)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			server, waitForRequest, unblock := newBlockedPreviewServer(t, sampleInfo().NodeInfo)
			entry, status := manualPreviewEntry(t, server)
			m, frames, _ := newTestManager()
			m.nodes[status.ID] = &trackedNode{entry: entry, status: status}
			done := make(chan struct{})
			go func() {
				m.previewAll(context.Background())
				close(done)
			}()
			waitForRequest()

			m.mu.Lock()
			change(m, status.ID)
			m.mu.Unlock()
			unblock()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("preview refresh did not finish")
			}

			select {
			case frame := <-frames.frames:
				t.Fatalf("retired preview emitted a frame: %s", frame)
			default:
			}

			if node := m.nodes[status.ID]; node != nil {
				assert.Nil(t, node.status.Preview)
			}
		})
	}

	test("removed entry", func(m *Manager, id string) { delete(m.nodes, id) })
	test("replacement with the same configuration", func(m *Manager, id string) {
		old := m.nodes[id]
		m.nodes[id] = &trackedNode{entry: old.entry, status: old.status}
	})
	test("changed address", func(m *Manager, id string) { m.nodes[id].entry.Address = "192.0.2.50" })
	test("promoted peer", func(m *Manager, id string) { m.nodes[id].status.IdentityVerification = noderec.IdentityPinned })
}

// Status replies must include elapsed preview age without modifying the cached
// sample. Otherwise, repeated reads would count the same elapsed time more than
// once and make the reported sample age grow too quickly.
func TestTrackedNodeStatusAt_AddsPreviewAgeWithoutChangingCachedSample(t *testing.T) {
	start := time.Now()
	preview := sampleInfo().NodeInfo
	node := &trackedNode{
		status:            ManualNodeStatus{Preview: &preview},
		previewObservedAt: start,
	}

	status := node.statusAt(start.Add(5 * time.Second))

	require.NotNil(t, status.Preview)
	assert.Equal(t, int64(5137), status.Preview.MSSince)
	assert.Equal(t, start.UnixMilli(), status.ObservedAt)
	assert.Equal(t, status.ObservedAt, node.statusAt(start.Add(7*time.Second)).ObservedAt)

	assert.Equal(t, int64(137), node.status.Preview.MSSince)

	// Equal hardware from a new read still gets a new chart observation.
	node.previewObservedAt = start.Add(9 * time.Second)
	assert.Equal(t, node.previewObservedAt.UnixMilli(), node.statusAt(start.Add(10*time.Second)).ObservedAt)
}

func TestProbeNode_ReplacesPublicPreviewWithAuthenticatedHardware(t *testing.T) {
	f := newManualPeerFixture(t)
	preview := sampleInfo().NodeInfo
	f.manager.nodes[nodeID(f.entry)].status.Preview = &preview

	f.manager.probeNode(f.entry)

	status := f.manager.listNodes()
	require.Len(t, status, 1)
	require.Equal(t, noderec.IdentityPinned, status[0].IdentityVerification)
	assert.Nil(t, status[0].Preview)

	assert.Equal(t, f.info.GPUs, status[0].GPUs)
}

// A saved hardware preview does not establish current service availability.
// Failed service probes must preserve the display preview while advancing the
// outage counter and reporting the failure when its threshold is reached.
func TestProbeNode_RetainsDisplayPreviewWithoutTreatingItAsReachability(t *testing.T) {
	m, frames, _ := newTestManager()
	entry := ManualEntry{Address: "node.local", Name: "preview-only"}
	preview := sampleInfo().NodeInfo
	m.nodes[nodeID(entry)] = &trackedNode{
		entry: entry,
		status: ManualNodeStatus{
			ID: nodeID(entry), IdentityVerification: noderec.IdentityUnverified,
			Address: entry.Address, Preview: &preview,
		},
		previewObservedAt: time.Now(),
		consecutiveFails:  2,
	}

	m.probeNode(entry)

	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, frames, methodIs("node/updated")))
	require.NotNil(t, updated.Preview)
	assert.Equal(t, preview.GPUs, updated.Preview.GPUs)

	assert.Empty(t, updated.GPUs)
	assert.Empty(t, updated.HostUUID)
	assert.False(t, updated.NodeInfoUp)
	assert.False(t, updated.TelemetryValid)

	// Cached display hardware does not cancel a service outage. This third
	// failed service probe must still reach the outage-reporting threshold.
	assert.Equal(t, 3, m.nodes[nodeID(entry)].consecutiveFails)
	readCaptureUntil(t, frames, methodIs("errors:report"))
}
