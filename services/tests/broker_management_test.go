// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process tests for the two broker-management hotfix legs:
//
//	A. the broker bridges a reachable supervised manual node into
//	   ollama-proxy (node/add-manual), so inference can route to a node
//	   that never appears over mDNS; and
//	B. the broker accepts a client-originated errors:report and forwards it
//	   into nvpair-errors, reflected in the next errors:update.
//
// Both reuse the supervision harness (startBrokerWith) and binaries built by
// TestMain.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nvpair-shared/appdir"
)

// fakeOllama stands up a minimal Ollama-looking HTTP server on the fixed
// port nvpair-manual-nodes probes (11434): GET / returns 200 (liveness) and
// GET /api/tags returns one model, so a manual node pointed at it reports
// ollama_up=true with a model list. Returns a stop func. Skips the test if
// the port can't be bound (real Ollama or something else holds it — we
// can't control the response then).
func fakeOllama(t *testing.T) func() {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:11434")
	if err != nil {
		t.Skipf("cannot bind fake ollama on 127.0.0.1:11434 (%v); skipping", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"name":"llama3.2:latest"}]}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

// fakeLMStudio stands up a minimal LM Studio-looking OpenAI-API server on the
// fixed port nvpair-manual-nodes probes (1234): GET /v1/models returns 200 with a
// one-model list, so a manual node pointed at it reports lmstudio_up=true.
// Skips the test if the port can't be bound.
func fakeLMStudio(t *testing.T) func() {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:1234")
	if err != nil {
		t.Skipf("cannot bind fake lmstudio on 127.0.0.1:1234 (%v); skipping", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"qwen2.5-7b-instruct","object":"model"}]}`)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

func persistLMStudioProxyPort(t *testing.T, port int) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("APPDATA", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HOME", root)
	path, err := appdir.Path("lmstudio-proxy-port.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"port":%d}`, port)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// fakeNodeInfo stands up a minimal node-info server on the fixed port the
// manual prober hits (14318): GET /v1/node-info returns 200 with the given
// hostUuid, so a manual node pointed at it learns its stable UUID. Skips the
// test if the port can't be bound.
func fakeNodeInfo(t *testing.T, hostUUID string) func() {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:14318")
	if err != nil {
		t.Skipf("cannot bind fake node-info on 127.0.0.1:14318 (%v); skipping", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"GPUs":[],"hostUuid":%q}`, hostUUID))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

// proxyNode mirrors the proxy's nodes/list element (ollama-proxy Node).
type proxyNode struct {
	ID        string   `json:"id"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	TXT       []string `json:"txt"`
}

type proxyNodesResult struct {
	Nodes []proxyNode `json:"nodes"`
}

func proxyNodesHas(t *testing.T, raw json.RawMessage, id string) bool {
	t.Helper()
	var res proxyNodesResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return false
	}
	for _, n := range res.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

// TestBrokerBridgesManualNodeIntoProxy is the leg-A repro: with the broker
// supervising both nvpair-manual-nodes and ollama-proxy, a manual node whose
// Ollama is reachable must be bridged into the proxy (node/add-manual) so it
// shows up in proxy:nodes/list and can be routed to — even though it never
// appears over mDNS.
func TestBrokerBridgesManualNodeIntoProxy(t *testing.T) {
	if portBusy(11435) {
		t.Skip("ollama-proxy port 11435 already in use; skipping")
	}
	stopOllama := fakeOllama(t) // skips if 11434 unavailable
	t.Cleanup(stopOllama)

	stdin, msgs, _, cleanup := startBrokerWith(t,
		"--manual-nodes-path", manualNodesBin,
		"--proxy-path", proxyBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const nodeName = "xproc-manual-proxy-bridge"
	addReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":900,"method":"node/add","params":{"address":"127.0.0.1","name":%q}}`, nodeName) + "\n"
	if _, err := stdin.Write([]byte(addReq)); err != nil {
		t.Fatalf("write node/add: %v", err)
	}

	// Poll the proxy's node set (via the broker's proxy: relay) until the
	// bridged manual node appears. The first probe fires immediately on
	// node/add; re-probes every 10s re-bridge if the proxy wasn't ready yet.
	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	reqID := 901
	sendReq(t, stdin, reqID, "proxy:nodes/list")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before the manual node bridged into the proxy")
			}
			if msg.Method == "" && msg.ID != nil && proxyNodesHas(t, msg.Result, nodeName) {
				t.Logf("manual node %q bridged into proxy nodes/list", nodeName)
				return
			}
		case <-ticker.C:
			reqID++
			sendReq(t, stdin, reqID, "proxy:nodes/list")
		case <-deadline:
			t.Fatalf("timed out waiting for manual node %q in proxy:nodes/list", nodeName)
		}
	}
}

// TestBrokerBridgesManualNodeUnderLearnedUUID: once a
// manual node's node-info reports its stable hostUuid, the proxy candidate must
// be keyed by that UUID — the same operational key the discovery store and
// scheduler use — so scheduler priority (node/set-priority) and scheduledOn
// resolve to it. The candidate must therefore appear in proxy:nodes/list under
// the learned hostUuid, not the user-supplied manual name.
func TestBrokerBridgesManualNodeUnderLearnedUUID(t *testing.T) {
	if portBusy(11435) {
		t.Skip("ollama-proxy port 11435 already in use; skipping")
	}
	stopOllama := fakeOllama(t) // skips if 11434 unavailable
	t.Cleanup(stopOllama)
	stopNodeInfo := fakeNodeInfo(t, "learned-host-uuid") // skips if 14318 unavailable
	t.Cleanup(stopNodeInfo)

	stdin, msgs, _, cleanup := startBrokerWith(t,
		"--manual-nodes-path", manualNodesBin,
		"--proxy-path", proxyBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const nodeName = "xproc-manual-learned-uuid"
	addReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":940,"method":"node/add","params":{"address":"127.0.0.1","name":%q}}`, nodeName) + "\n"
	if _, err := stdin.Write([]byte(addReq)); err != nil {
		t.Fatalf("write node/add: %v", err)
	}

	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	reqID := 941
	sendReq(t, stdin, reqID, "proxy:nodes/list")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before the manual node bridged under its learned UUID")
			}
			if msg.Method == "" && msg.ID != nil {
				if proxyNodesHas(t, msg.Result, "learned-host-uuid") {
					t.Logf("manual node bridged into proxy under learned hostUuid")
					return
				}
				if proxyNodesHas(t, msg.Result, nodeName) {
					t.Fatalf("proxy candidate keyed by manual name %q, not the learned hostUuid (scheduler would not match)", nodeName)
				}
			}
		case <-ticker.C:
			reqID++
			sendReq(t, stdin, reqID, "proxy:nodes/list")
		case <-deadline:
			t.Fatalf("timed out waiting for manual node under learned hostUuid in proxy:nodes/list")
		}
	}
}

// TestBrokerBridgesManualNodeIntoLMStudioProxy is the LM Studio counterpart of
// the Ollama bridge test: with the broker supervising both nvpair-manual-nodes and
// lmstudio-proxy, a manual node whose LM Studio is reachable must be bridged
// into lmstudio-proxy (node/add-manual) so it shows up in
// lmstudio-proxy:nodes/list — even though it never appears over mDNS.
func TestBrokerBridgesManualNodeIntoLMStudioProxy(t *testing.T) {
	configDir := persistLMStudioProxyPort(t, freePort(t))
	stopLM := fakeLMStudio(t) // skips if 1234 unavailable
	t.Cleanup(stopLM)

	stdin, msgs, _, cleanup := startBrokerWithConfigDir(t, configDir,
		"--manual-nodes-path", manualNodesBin,
		"--lmstudio-proxy-path", lmstudioProxyBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const nodeName = "xproc-manual-lmstudio-bridge"
	addReq := fmt.Sprintf(`{"jsonrpc":"2.0","id":920,"method":"node/add","params":{"address":"127.0.0.1","name":%q}}`, nodeName) + "\n"
	if _, err := stdin.Write([]byte(addReq)); err != nil {
		t.Fatalf("write node/add: %v", err)
	}

	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	reqID := 921
	sendReq(t, stdin, reqID, "lmstudio-proxy:nodes/list")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before the manual node bridged into lmstudio-proxy")
			}
			if msg.Method == "" && msg.ID != nil && proxyNodesHas(t, msg.Result, nodeName) {
				t.Logf("manual node %q bridged into lmstudio-proxy nodes/list", nodeName)
				return
			}
		case <-ticker.C:
			reqID++
			sendReq(t, stdin, reqID, "lmstudio-proxy:nodes/list")
		case <-deadline:
			t.Fatalf("timed out waiting for manual node %q in lmstudio-proxy:nodes/list", nodeName)
		}
	}
}

// TestBrokerAcceptsClientErrorsReport is the leg-B repro: a client-originated
// errors:report (notification form) must be forwarded into nvpair-errors and
// reflected in the next errors:update — the same as a supervised worker
// emitting one on its stdout.
func TestBrokerAcceptsClientErrorsReport(t *testing.T) {
	if portBusy(14319) {
		t.Skip("nvpair-errors --peer-sync port 14319 already in use; skipping")
	}

	stdin, msgs, _, cleanup := startBrokerWith(t, "--errors-path", errorsBin)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const id = "client:leg-b-notif"
	report := fmt.Sprintf(`{"jsonrpc":"2.0","method":"errors:report","params":{"id":%q,"message":"client-synthesized","severity":"error"}}`, id) + "\n"
	if _, err := stdin.Write([]byte(report)); err != nil {
		t.Fatalf("write errors:report notification: %v", err)
	}

	// The broker forwards it into nvpair-errors, which stores it and pushes an
	// errors:update the broker relays back unconditionally.
	waitForErrorsUpdateContaining(t, msgs, id, 10*time.Second)
	t.Logf("client errors:report %q reflected in errors:update", id)
}

// TestBrokerAcceptsClientErrorsReportRequest covers the request form of
// leg B: an id-bearing errors:report is acked with a null result and also
// reflected in errors:update.
func TestBrokerAcceptsClientErrorsReportRequest(t *testing.T) {
	if portBusy(14319) {
		t.Skip("nvpair-errors --peer-sync port 14319 already in use; skipping")
	}

	stdin, msgs, _, cleanup := startBrokerWith(t, "--errors-path", errorsBin)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	const (
		reqID = 950
		id    = "client:leg-b-req"
	)
	report := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"errors:report","params":{"id":%q,"message":"client-synthesized","severity":"error"}}`, reqID, id) + "\n"
	if _, err := stdin.Write([]byte(report)); err != nil {
		t.Fatalf("write errors:report request: %v", err)
	}

	// Expect both the ack (result for reqID, no error) and the errors:update.
	gotAck := false
	gotUpdate := false
	deadline := time.After(10 * time.Second)
	for !(gotAck && gotUpdate) {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before the report was acked and reflected")
			}
			switch {
			case msg.Method == "" && msg.ID != nil:
				if string(*msg.ID) == fmt.Sprintf("%d", reqID) {
					if msg.Error != nil {
						t.Fatalf("errors:report request returned an error: %+v", msg.Error)
					}
					gotAck = true
				}
			case msg.Method == methodErrorsUpdate:
				if errorsListHasID(t, msg.Params, id) {
					gotUpdate = true
				}
			}
		case <-deadline:
			t.Fatalf("timed out (ack=%v update=%v) for errors:report request %q", gotAck, gotUpdate, id)
		}
	}
}
