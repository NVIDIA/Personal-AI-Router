// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	assert.Equal(t, 14318, initial.NodeInfoPort, "default ports not set (%v)", initial)
	assert.False(t, initial.OllamaUp, "initial status should be unprobed (%v)", initial)
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
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
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
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
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
	readCaptureUntil(t, f.frames, methodIs(updatedMethod))

	f.manager.probeNode(f.entry)

	message := readCaptureUntil(t, f.frames, methodIs(updatedMethod))
	status := decodeParams[ManualNodeStatus](t, message)
	assert.Equal(t, noderec.IdentityPinned, status.IdentityVerification)
	assert.False(t, status.TelemetryValid)
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
