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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/applog"
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
	m, err := NewManager(NewCodec(rw), tlsClientOptions{}, nil)
	if err != nil {
		panic(err)
	}
	rt := newFakeRoundTripper()
	m.client = &http.Client{Transport: rt, Timeout: time.Second}
	m.tlsClient = &http.Client{Transport: rt, Timeout: time.Second}
	return m, rw, rt
}

func configureHealthyNode(rt *fakeRoundTripper, addr string, models []string, info NodeInfoResponse) {
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
		data, _ := json.Marshal(payload)
		return httpJSON(http.StatusOK, string(data))
	})

	infoHost := net.JoinHostPort(addr, "14318")
	rt.set(http.MethodGet, infoHost, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})
}

// configureHealthyLMStudio registers a 200 GET /v1/models on addr:1234
// returning the given model ids in the OpenAI list shape, so probeLMStudio
// reports the node up with those models.
func configureHealthyLMStudio(rt *fakeRoundTripper, addr string, models []string) {
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
		data, _ := json.Marshal(payload)
		return httpJSON(http.StatusOK, string(data))
	})
}

// TestProbeLMStudioReportsModels covers the new LM Studio probe: a reachable
// server reports up with its model ids parsed from /v1/models, and an absent
// one (no handler registered, so the request errors) reports down.
func TestProbeLMStudioReportsModels(t *testing.T) {
	m, _, rt := newTestManager()
	configureHealthyLMStudio(rt, "node.local", []string{"qwen2.5-7b", "llama-3.1-8b"})

	up, models := m.probeLMStudio("node.local", lmStudioPort)
	require.True(t, up, "expected lmstudio up")
	assert.Equal(t, []string{"qwen2.5-7b", "llama-3.1-8b"}, models, "models")

	downUp, downModels := m.probeLMStudio("absent.local", lmStudioPort)
	require.False(t, downUp, "expected absent lmstudio down, got up (%v, %v)", downUp, downModels)
	assert.Empty(t, downModels, "expected absent lmstudio down, got up (%v, %v)", downUp, downModels)
}

func requestMessage(id int, method string, params any) *Message {
	idData, _ := json.Marshal(id)
	idRaw := json.RawMessage(idData)
	paramsRaw, _ := json.Marshal(params)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: paramsRaw}
}

func requestMessageRaw(id int, method string, params json.RawMessage) *Message {
	idData, _ := json.Marshal(id)
	idRaw := json.RawMessage(idData)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: params}
}

func notificationMessage(method string, params any) *Message {
	paramsRaw, _ := json.Marshal(params)
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

func responseWithID(id int) func(Message) bool {
	return func(msg Message) bool {
		if msg.ID == nil {
			return false
		}
		var got int
		return json.Unmarshal(*msg.ID, &got) == nil && got == id
	}
}

func methodIs(method string) func(Message) bool {
	return func(msg Message) bool { return msg.Method == method }
}

func sampleInfo() NodeInfoResponse {
	return NodeInfoResponse{
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
	}
}

func TestNodeID(t *testing.T) {
	assert.Equal(t, "workstation", nodeID(ManualEntry{Name: "workstation", Address: "10.0.0.5"}), "named nodeID")
	assert.Equal(t, "manual:10.0.0.5", nodeID(ManualEntry{Address: "10.0.0.5"}), "unnamed nodeID")
}

func TestNodeAddRespondsWithInitialStatusThenDiscoversProbeResult(t *testing.T) {
	m, rw, rt := newTestManager()
	configureHealthyNode(rt, "node.local", []string{"llama3", "mistral"}, sampleInfo())

	m.handleMessage(requestMessage(1, "node/add", ManualEntry{Address: "node.local", Name: "lab"}))

	resp := readCaptureUntil(t, rw, responseWithID(1))
	initial := decodeResult[ManualNodeStatus](t, resp)
	assert.Equal(t, "lab", initial.ID, "initial status (%v)", initial)
	assert.Equal(t, "node.local", initial.Address, "initial status (%v)", initial)
	assert.Equal(t, 11434, initial.OllamaPort, "default ports not set (%v)", initial)
	assert.Equal(t, 14318, initial.NodeInfoPort, "default ports not set (%v)", initial)
	assert.False(t, initial.OllamaUp, "initial status should be unprobed (%v)", initial)
	assert.False(t, initial.NodeInfoUp, "initial status should be unprobed (%v)", initial)

	discovered := readCaptureUntil(t, rw, methodIs("node/discovered"))
	status := decodeParams[ManualNodeStatus](t, discovered)
	assert.True(t, status.OllamaUp, "discovered status did not include healthy services (%v)", status)
	assert.True(t, status.NodeInfoUp, "discovered status did not include healthy services (%v)", status)
	assert.Equal(t, []string{"llama3", "mistral"}, status.OllamaModels)
	require.Len(t, status.GPUs, 1)
	assert.Equal(t, "RTX 6000", status.GPUs[0].Name)
	require.NotNil(t, status.CPU)
	assert.Equal(t, uint32(64), status.CPU.Cores)
	require.NotNil(t, status.Memory)
	assert.Equal(t, uint64(32<<30), status.Memory.UsedBytes)
	assert.True(t, status.TelemetryValid, "telemetry = valid")
	assert.Equal(t, int64(137), status.MSSince, "telemetry = valid")
}

func TestNodeAddValidationErrors(t *testing.T) {
	m, rw, _ := newTestManager()

	m.handleMessage(requestMessageRaw(1, "node/add", json.RawMessage(`"bad"`)))
	resp := readCaptureFrame(t, rw)
	require.NotNil(t, resp.Error, "malformed params error")
	assert.Equal(t, -32602, resp.Error.Code, "malformed params error")

	m.handleMessage(requestMessage(2, "node/add", ManualEntry{}))
	resp = readCaptureFrame(t, rw)
	require.NotNil(t, resp.Error, "missing address error")
	assert.Equal(t, -32602, resp.Error.Code, "missing address error")

	require.Empty(t, m.listNodes(), "validation errors added nodes")
}

func TestNodesListReturnsCurrentStatuses(t *testing.T) {
	m, rw, rt := newTestManager()
	configureHealthyNode(rt, "node.local", []string{"llama3"}, sampleInfo())

	m.handleMessage(requestMessage(1, "node/add", ManualEntry{Address: "node.local"}))
	_ = readCaptureUntil(t, rw, methodIs("node/discovered"))

	m.handleMessage(requestMessage(2, "nodes/list", nil))
	resp := readCaptureUntil(t, rw, responseWithID(2))
	var result struct {
		Nodes []ManualNodeStatus `json:"nodes"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &result), "decode nodes/list")
	require.Len(t, result.Nodes, 1, "nodes/list length")
	assert.True(t, result.Nodes[0].OllamaUp, "nodes/list status")
	assert.True(t, result.Nodes[0].NodeInfoUp, "nodes/list status")
}

func TestNodeRemoveReturnsRemovedAndNotifies(t *testing.T) {
	m, rw, _ := newTestManager()
	m.nodes["lab"] = &trackedNode{
		entry:  ManualEntry{Name: "lab", Address: "node.local"},
		status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318},
	}

	m.handleMessage(requestMessage(1, "node/remove", map[string]string{"id": "lab"}))
	removed := readCaptureFrame(t, rw)
	assert.Equal(t, "node/removed", removed.Method, "first remove frame (%v)", removed)
	status := decodeParams[ManualNodeStatus](t, removed)
	assert.Equal(t, "lab", status.ID, "removed params (%v)", status)
	resp := readCaptureUntil(t, rw, responseWithID(1))
	result := decodeResult[map[string]bool](t, resp)
	assert.True(t, result["removed"], "removed result (%v)", result)
	require.Empty(t, m.listNodes(), "node still listed after removal")

	m.handleMessage(requestMessage(2, "node/remove", map[string]string{"id": "lab"}))
	resp = readCaptureUntil(t, rw, responseWithID(2))
	result = decodeResult[map[string]bool](t, resp)
	assert.False(t, result["removed"], "second removal result (%v)", result)
	assertNoCaptureMethod(t, rw, "node/removed")
}

func TestAddThenRemoveBeforeInitialProbeDoesNotRediscover(t *testing.T) {
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
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"GPUs":[]}`)
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

func TestProbeNodeEmitsUpdatedOnStateChange(t *testing.T) {
	m, rw, rt := newTestManager()
	m.nodes["lab"] = &trackedNode{entry: ManualEntry{Name: "lab", Address: "node.local"}, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}

	configureHealthyNode(rt, "node.local", []string{"llama3"}, sampleInfo())
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	first := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.Equal(t, []string{"llama3"}, first.OllamaModels, "first update models")

	configureHealthyNode(rt, "node.local", []string{"mistral"}, sampleInfo())
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	second := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.Equal(t, []string{"mistral"}, second.OllamaModels, "second update models")
}

func TestProbeNodeNoUpdateWhenStable(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{entry: entry, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}
	configureHealthyNode(rt, "node.local", []string{"llama3"}, sampleInfo())

	m.probeNode(entry)
	_ = readCaptureUntil(t, rw, methodIs("node/updated"))
	m.probeNode(entry)

	assertNoCaptureMethod(t, rw, "node/updated")
}

func TestProbeFailuresClearAvailability(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{
		entry: entry,
		status: ManualNodeStatus{
			ID:           "lab",
			Address:      "node.local",
			OllamaUp:     true,
			OllamaPort:   11434,
			OllamaModels: []string{"llama3"},
			NodeInfoUp:   true,
			NodeInfoPort: 14318,
			GPUs:         sampleInfo().GPUs,
			CPU:          sampleInfo().CPU,
			Memory:       sampleInfo().Memory,
		},
	}
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "11434"), "/", func(*http.Request) (*http.Response, error) {
		return nil, errors.New("ollama down")
	})
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{not json`)
	})

	m.probeNode(entry)
	updated := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.False(t, updated.OllamaUp, "services should be down (%v)", updated)
	assert.False(t, updated.NodeInfoUp, "services should be down (%v)", updated)
	require.Empty(t, updated.OllamaModels, "failed probe retained stale fields (%v)", updated)
	require.Empty(t, updated.GPUs, "failed probe retained stale fields (%v)", updated)
	require.Nil(t, updated.CPU, "failed probe retained stale fields (%v)", updated)
	require.Nil(t, updated.Memory, "failed probe retained stale fields (%v)", updated)
}

// TestProbeFailurePreservesHostUUID: a node-info blip
// (while Ollama stays reachable) must NOT blank the learned HostUUID, or the
// broker would rekey the live node UUID -> manual-id -> UUID.
func TestProbeFailurePreservesHostUUID(t *testing.T) {
	m, rw, rt := newTestManager()
	entry := ManualEntry{Name: "lab", Address: "node.local"}
	m.nodes["lab"] = &trackedNode{entry: entry, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}

	info := sampleInfo()
	info.HostUUID = "node-uuid"
	configureHealthyNode(rt, "node.local", []string{"llama3"}, info)

	// First probe learns the UUID.
	m.probeNode(entry)
	first := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.Equal(t, "node-uuid", first.HostUUID, "first probe HostUUID")

	// node-info goes down while Ollama stays up: the UUID must be preserved.
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return nil, errors.New("node-info down")
	})
	m.probeNode(entry)
	down := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.False(t, down.NodeInfoUp, "node-info should be down")
	assert.Equal(t, "node-uuid", down.HostUUID, "HostUUID dropped on node-info failure")

	// node-info recovers: still the same UUID (no flap).
	configureHealthyNode(rt, "node.local", []string{"llama3"}, info)
	m.probeNode(entry)
	up := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	assert.Equal(t, "node-uuid", up.HostUUID, "HostUUID after recovery")
}

func TestCPUAndMemoryNilAwareEquality(t *testing.T) {
	assert.True(t, cpuEqual(nil, nil), "nil values should compare equal")
	assert.True(t, memoryEqual(nil, nil), "nil values should compare equal")
	assert.False(t, cpuEqual(nil, &CPUInfo{}), "nil and non-nil values should differ")
	assert.False(t, memoryEqual(nil, &MemoryInfo{}), "nil and non-nil values should differ")
	assert.True(t, cpuEqual(&CPUInfo{Name: "cpu", Cores: 8}, &CPUInfo{Name: "cpu", Cores: 8}), "equal CPU values differed")
	assert.False(t, cpuEqual(&CPUInfo{Name: "cpu", Cores: 8}, &CPUInfo{Name: "cpu", Cores: 16}), "different CPU values compared equal")
	assert.True(t, memoryEqual(&MemoryInfo{TotalBytes: 10, UsedBytes: 5}, &MemoryInfo{TotalBytes: 10, UsedBytes: 5}), "equal memory values differed")
	assert.False(t, memoryEqual(&MemoryInfo{TotalBytes: 10, UsedBytes: 5}, &MemoryInfo{TotalBytes: 10, UsedBytes: 6}), "different memory values compared equal")
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(1, "bogus", nil))
	resp := readCaptureFrame(t, rw)
	require.NotNil(t, resp.Error, "unknown method error")
	assert.Equal(t, -32601, resp.Error.Code, "unknown method error")
}

func TestLogSetLevelRequest(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(1, applog.SetLevelMethod, applog.SetLevelParams{Level: "debug"}))
	resp := readCaptureFrame(t, rw)
	result := decodeResult[map[string]string](t, resp)
	assert.Equal(t, "debug", result["level"], "log/set-level result (%v)", result)

	m.handleMessage(requestMessage(2, applog.SetLevelMethod, applog.SetLevelParams{Level: "not-a-level"}))
	resp = readCaptureFrame(t, rw)
	require.NotNil(t, resp.Error, "invalid log/set-level error")
	assert.Equal(t, -32602, resp.Error.Code, "invalid log/set-level error")
}

func TestShutdownRequestCancelsRun(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	mgr, err := NewManager(NewCodec(server), tlsClientOptions{}, nil)
	require.NoError(t, err, "NewManager")
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
	assert.True(t, responseWithID(7)(resp), "shutdown response (%v)", resp)
	require.Nil(t, resp.Error, "shutdown response (%v)", resp)

	select {
	case err := <-done:
		require.NoError(t, err, "Run returned error")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Run did not return after shutdown")
	}
}

func TestNotificationIsIgnored(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(notificationMessage("node/add", ManualEntry{Address: "node.local"}))
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
