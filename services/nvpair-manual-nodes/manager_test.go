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
	if !up {
		t.Fatal("expected lmstudio up")
	}
	if len(models) != 2 || models[0] != "qwen2.5-7b" || models[1] != "llama-3.1-8b" {
		t.Fatalf("models = %#v", models)
	}

	downUp, downModels := m.probeLMStudio("absent.local", lmStudioPort)
	if downUp || downModels != nil {
		t.Fatalf("expected absent lmstudio down, got up=%v models=%#v", downUp, downModels)
	}
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
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("decode frame %q: %v", data, err)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame")
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
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("decode frame %q: %v", data, err)
			}
			if match(msg) {
				return msg
			}
		case <-deadline:
			t.Fatal("timed out waiting for matching frame")
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
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("decode frame %q: %v", data, err)
			}
			if msg.Method == method {
				t.Fatalf("unexpected %s notification: %+v", method, msg)
			}
		case <-timer.C:
			return
		}
	}
}

func decodeResult[T any](t *testing.T, msg Message) T {
	t.Helper()
	if msg.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", msg.Error)
	}
	var result T
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatalf("decode result %q: %v", msg.Result, err)
	}
	return result
}

func decodeParams[T any](t *testing.T, msg Message) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(msg.Params, &result); err != nil {
		t.Fatalf("decode params %q: %v", msg.Params, err)
	}
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
	if got := nodeID(ManualEntry{Name: "workstation", Address: "10.0.0.5"}); got != "workstation" {
		t.Fatalf("named nodeID = %q", got)
	}
	if got := nodeID(ManualEntry{Address: "10.0.0.5"}); got != "manual:10.0.0.5" {
		t.Fatalf("unnamed nodeID = %q", got)
	}
}

func TestNodeAddRespondsWithInitialStatusThenDiscoversProbeResult(t *testing.T) {
	m, rw, rt := newTestManager()
	configureHealthyNode(rt, "node.local", []string{"llama3", "mistral"}, sampleInfo())

	m.handleMessage(requestMessage(1, "node/add", ManualEntry{Address: "node.local", Name: "lab"}))

	resp := readCaptureUntil(t, rw, responseWithID(1))
	initial := decodeResult[ManualNodeStatus](t, resp)
	if initial.ID != "lab" || initial.Address != "node.local" {
		t.Fatalf("initial status = %+v", initial)
	}
	if initial.OllamaPort != 11434 || initial.NodeInfoPort != 14318 {
		t.Fatalf("default ports not set: %+v", initial)
	}
	if initial.OllamaUp || initial.NodeInfoUp {
		t.Fatalf("initial status should be unprobed: %+v", initial)
	}

	discovered := readCaptureUntil(t, rw, methodIs("node/discovered"))
	status := decodeParams[ManualNodeStatus](t, discovered)
	if !status.OllamaUp || !status.NodeInfoUp {
		t.Fatalf("discovered status did not include healthy services: %+v", status)
	}
	if len(status.OllamaModels) != 2 || status.OllamaModels[0] != "llama3" || status.OllamaModels[1] != "mistral" {
		t.Fatalf("models = %#v", status.OllamaModels)
	}
	if len(status.GPUs) != 1 || status.GPUs[0].Name != "RTX 6000" {
		t.Fatalf("gpus = %#v", status.GPUs)
	}
	if status.CPU == nil || status.CPU.Cores != 64 {
		t.Fatalf("cpu = %#v", status.CPU)
	}
	if status.Memory == nil || status.Memory.UsedBytes != 32<<30 {
		t.Fatalf("memory = %#v", status.Memory)
	}
	if !status.TelemetryValid || status.MSSince != 137 {
		t.Fatalf("telemetry = valid:%v age:%d, want true/137", status.TelemetryValid, status.MSSince)
	}
}

func TestNodeAddValidationErrors(t *testing.T) {
	m, rw, _ := newTestManager()

	m.handleMessage(requestMessageRaw(1, "node/add", json.RawMessage(`"bad"`)))
	resp := readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("malformed params error = %+v", resp.Error)
	}

	m.handleMessage(requestMessage(2, "node/add", ManualEntry{}))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("missing address error = %+v", resp.Error)
	}

	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("validation errors added nodes: %#v", got)
	}
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
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode nodes/list: %v", err)
	}
	if len(result.Nodes) != 1 {
		t.Fatalf("nodes/list length = %d", len(result.Nodes))
	}
	if !result.Nodes[0].OllamaUp || !result.Nodes[0].NodeInfoUp {
		t.Fatalf("nodes/list status = %+v", result.Nodes[0])
	}
}

func TestNodeRemoveReturnsRemovedAndNotifies(t *testing.T) {
	m, rw, _ := newTestManager()
	m.nodes["lab"] = &trackedNode{
		entry:  ManualEntry{Name: "lab", Address: "node.local"},
		status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318},
	}

	m.handleMessage(requestMessage(1, "node/remove", map[string]string{"id": "lab"}))
	removed := readCaptureFrame(t, rw)
	if removed.Method != "node/removed" {
		t.Fatalf("first remove frame = %+v", removed)
	}
	status := decodeParams[ManualNodeStatus](t, removed)
	if status.ID != "lab" {
		t.Fatalf("removed params = %+v", status)
	}
	resp := readCaptureUntil(t, rw, responseWithID(1))
	result := decodeResult[map[string]bool](t, resp)
	if !result["removed"] {
		t.Fatalf("removed result = %#v", result)
	}
	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("node still listed after removal: %#v", got)
	}

	m.handleMessage(requestMessage(2, "node/remove", map[string]string{"id": "lab"}))
	resp = readCaptureUntil(t, rw, responseWithID(2))
	result = decodeResult[map[string]bool](t, resp)
	if result["removed"] {
		t.Fatalf("second removal result = %#v", result)
	}
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
	if status.ID != "lab" {
		t.Fatalf("add status = %+v", status)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("initial probe did not start")
	}

	if !m.removeNode("lab") {
		t.Fatal("removeNode returned false")
	}
	_ = readCaptureUntil(t, rw, methodIs("node/removed"))
	close(release)

	assertNoCaptureMethod(t, rw, "node/discovered")
	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("node rediscovered in state: %#v", got)
	}
}

func TestProbeNodeEmitsUpdatedOnStateChange(t *testing.T) {
	m, rw, rt := newTestManager()
	m.nodes["lab"] = &trackedNode{entry: ManualEntry{Name: "lab", Address: "node.local"}, status: ManualNodeStatus{ID: "lab", Address: "node.local", OllamaPort: 11434, NodeInfoPort: 14318}}

	configureHealthyNode(rt, "node.local", []string{"llama3"}, sampleInfo())
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	first := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if len(first.OllamaModels) != 1 || first.OllamaModels[0] != "llama3" {
		t.Fatalf("first update models = %#v", first.OllamaModels)
	}

	configureHealthyNode(rt, "node.local", []string{"mistral"}, sampleInfo())
	m.probeNode(ManualEntry{Name: "lab", Address: "node.local"})
	second := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if len(second.OllamaModels) != 1 || second.OllamaModels[0] != "mistral" {
		t.Fatalf("second update models = %#v", second.OllamaModels)
	}
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
	if updated.OllamaUp || updated.NodeInfoUp {
		t.Fatalf("services should be down: %+v", updated)
	}
	if len(updated.OllamaModels) != 0 || len(updated.GPUs) != 0 || updated.CPU != nil || updated.Memory != nil {
		t.Fatalf("failed probe retained stale fields: %+v", updated)
	}
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
	if first.HostUUID != "node-uuid" {
		t.Fatalf("first probe HostUUID = %q, want node-uuid", first.HostUUID)
	}

	// node-info goes down while Ollama stays up: the UUID must be preserved.
	rt.set(http.MethodGet, net.JoinHostPort("node.local", "14318"), "/v1/node-info", func(*http.Request) (*http.Response, error) {
		return nil, errors.New("node-info down")
	})
	m.probeNode(entry)
	down := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if down.NodeInfoUp {
		t.Fatal("node-info should be down")
	}
	if down.HostUUID != "node-uuid" {
		t.Fatalf("HostUUID dropped on node-info failure: %q", down.HostUUID)
	}

	// node-info recovers: still the same UUID (no flap).
	configureHealthyNode(rt, "node.local", []string{"llama3"}, info)
	m.probeNode(entry)
	up := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/updated")))
	if up.HostUUID != "node-uuid" {
		t.Fatalf("HostUUID after recovery = %q, want node-uuid", up.HostUUID)
	}
}

func TestCPUAndMemoryNilAwareEquality(t *testing.T) {
	if !cpuEqual(nil, nil) || !memoryEqual(nil, nil) {
		t.Fatal("nil values should compare equal")
	}
	if cpuEqual(nil, &CPUInfo{}) || memoryEqual(nil, &MemoryInfo{}) {
		t.Fatal("nil and non-nil values should differ")
	}
	if !cpuEqual(&CPUInfo{Name: "cpu", Cores: 8}, &CPUInfo{Name: "cpu", Cores: 8}) {
		t.Fatal("equal CPU values differed")
	}
	if cpuEqual(&CPUInfo{Name: "cpu", Cores: 8}, &CPUInfo{Name: "cpu", Cores: 16}) {
		t.Fatal("different CPU values compared equal")
	}
	if !memoryEqual(&MemoryInfo{TotalBytes: 10, UsedBytes: 5}, &MemoryInfo{TotalBytes: 10, UsedBytes: 5}) {
		t.Fatal("equal memory values differed")
	}
	if memoryEqual(&MemoryInfo{TotalBytes: 10, UsedBytes: 5}, &MemoryInfo{TotalBytes: 10, UsedBytes: 6}) {
		t.Fatal("different memory values compared equal")
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(1, "bogus", nil))
	resp := readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("unknown method error = %+v", resp.Error)
	}
}

func TestLogSetLevelRequest(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(requestMessage(1, applog.SetLevelMethod, applog.SetLevelParams{Level: "debug"}))
	resp := readCaptureFrame(t, rw)
	result := decodeResult[map[string]string](t, resp)
	if result["level"] != "debug" {
		t.Fatalf("log/set-level result = %#v", result)
	}

	m.handleMessage(requestMessage(2, applog.SetLevelMethod, applog.SetLevelParams{Level: "not-a-level"}))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("invalid log/set-level error = %+v", resp.Error)
	}
}

func TestShutdownRequestCancelsRun(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	mgr, err := NewManager(NewCodec(server), tlsClientOptions{}, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- mgr.Run(ctx)
	}()

	reader := bufio.NewReader(client)
	ready := readPipeFrame(t, client, reader)
	if ready.Method != "ready" {
		t.Fatalf("first frame = %+v", ready)
	}

	writePipeRequest(t, client, 7, "shutdown", nil)
	resp := readPipeFrame(t, client, reader)
	if !responseWithID(7)(resp) || resp.Error != nil {
		t.Fatalf("shutdown response = %+v", resp)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

func TestNotificationIsIgnored(t *testing.T) {
	m, rw, _ := newTestManager()
	m.handleMessage(notificationMessage("node/add", ManualEntry{Address: "node.local"}))
	if got := m.listNodes(); len(got) != 0 {
		t.Fatalf("notification mutated state: %#v", got)
	}
	assertNoCaptureMethod(t, rw, "")
}

func readPipeFrame(t *testing.T, conn net.Conn, reader *bufio.Reader) Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read pipe frame: %v", err)
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		t.Fatalf("decode pipe frame %q: %v", line, err)
	}
	return msg
}

func writePipeRequest(t *testing.T, conn net.Conn, id int, method string, params any) {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
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
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write request: %v", err)
	}
}
