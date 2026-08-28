// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// secure_inference_test.go is the cross-process proof that inference is gated on
// cluster membership end to end. It pairs two real cluster-managers (A and B) so
// each holds the other's pin on disk, runs each node's real ollama-proxy against
// that cluster dir, and then drives actual traffic:
//
//   - A client on A's loopback facade routes an inference request to B; A dials
//     B's promoted proxy over cluster mTLS and B forwards it to B's loopback
//     engine — a full paired A->B inference over the pin-gated channel.
//   - A foreign node C (its own cluster, never paired with B) is rejected by B's
//     mTLS ingress with 403: it can complete the TLS handshake but its cert is
//     not pinned.
//   - Deleting A's pin from B's trust store causes B to reject A on the very next
//     request (pins are reloaded per request), so a removed member loses inference
//     access immediately without any restart.
//
// The engines here are httptest servers bound to loopback, standing in for
// Ollama; the mTLS identities and pins are the real ones the cluster-manager
// mints and exchanges during pairing, so the crypto path is genuine.

// proxyProc is a running ollama-proxy subprocess driven over stdio.
type proxyProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	msgs   <-chan jsonrpc.Message
	buf    []jsonrpc.Message
	nextID int
	port   int // the listen port the proxy reported via its ready notification
}

// startProxyProc launches ollama-proxy with the given cluster dir on a fresh
// port and blocks until it reports ready. Its persisted-port store is isolated
// into a temp config dir so the test never reads or writes the developer's real
// config.
func startProxyProc(t *testing.T, clusterDir string, listenPort int) *proxyProc {
	t.Helper()
	cfg := t.TempDir()
	cmd := exec.Command(proxyBin,
		"--cluster-dir", clusterDir,
		"--port", strconv.Itoa(listenPort),
		"--ignore-persisted-port",
	)
	cmd.Env = append(os.Environ(),
		"HOME="+cfg, "XDG_CONFIG_HOME="+cfg, "APPDATA="+cfg, "LOCALAPPDATA="+cfg,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("proxy stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("proxy stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	p := &proxyProc{t: t, cmd: cmd, stdin: stdin, msgs: startMsgReader(stdout), nextID: 1}

	ready := p.pump(func(m jsonrpc.Message) bool { return m.Method == "ready" }, 15*time.Second)
	var rp struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(ready.Params, &rp); err != nil || rp.Port == 0 {
		t.Fatalf("proxy ready params = %s (err %v)", ready.Params, err)
	}
	p.port = rp.Port
	return p
}

func (p *proxyProc) stop() {
	_ = p.stdin.Close()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
	}
}

func (p *proxyProc) pump(want func(jsonrpc.Message) bool, timeout time.Duration) jsonrpc.Message {
	p.t.Helper()
	for i, m := range p.buf {
		if want(m) {
			p.buf = append(p.buf[:i], p.buf[i+1:]...)
			return m
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				p.t.Fatal("proxy stdout closed unexpectedly")
			}
			if want(m) {
				return m
			}
			p.buf = append(p.buf, m)
		case <-timer.C:
			p.t.Fatal("timed out waiting on proxy message")
		}
	}
}

func (p *proxyProc) call(method string, params any) jsonrpc.Message {
	p.t.Helper()
	id := p.nextID
	p.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	b = append(b, '\n')
	if _, err := p.stdin.Write(b); err != nil {
		p.t.Fatalf("write %s: %v", method, err)
	}
	resp := p.pump(func(m jsonrpc.Message) bool { return m.Method == "" && idEquals(m.ID, id) }, 15*time.Second)
	if resp.Error != nil {
		p.t.Fatalf("%s returned error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp
}

func (p *proxyProc) notify(method string, params any) {
	p.t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	b = append(b, '\n')
	if _, err := p.stdin.Write(b); err != nil {
		p.t.Fatalf("notify %s: %v", method, err)
	}
}

// setLocalBackend points the proxy's cluster ingress + local self candidate at a
// loopback engine.
func (p *proxyProc) setLocalBackend(engine, host string, port int, healthy bool) {
	p.call("node/set-local-backend", map[string]any{
		"engine": engine, "host": host, "port": port, "healthy": healthy,
	})
}

// pushOllamaPeer feeds the proxy a discovery:nodes snapshot advertising a single
// remote peer that runs ollama at proxyPort, tagged as a trusted cluster member
// keyed by clusterUUID (the peer's cluster cert principal).
func (p *proxyProc) pushOllamaPeer(name, ip, clusterUUID string, proxyPort int, models []string) {
	p.notify("discovery:nodes", map[string]any{
		"nodes": []map[string]any{{
			"hostUuid":       name,
			"name":           name,
			"ip":             ip,
			"clusterUuid":    clusterUUID,
			"trusted":        true,
			"services":       map[string]any{"ol": map[string]any{"port": proxyPort}},
			"modelsByEngine": map[string]any{"ollama": models},
			"lastSeen":       time.Now().Unix(),
		}},
	})
}

// waitForRoutableNode polls the proxy's nodes/list until it reports at least one
// routable node, so the async discovery:nodes push has been applied before the
// test issues a request.
func (p *proxyProc) waitForRoutableNode(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp := p.call("nodes/list", nil)
		var r struct {
			Nodes []json.RawMessage `json:"nodes"`
		}
		if json.Unmarshal(resp.Result, &r) == nil && len(r.Nodes) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("proxy never registered the pushed discovery node")
}

// startFakeOllama runs a loopback httptest server that answers the model-list
// and generate routes an ollama-proxy forwards, counting generate calls so the
// test can prove whether a request actually reached the backend engine.
func startFakeOllama(t *testing.T) (host string, port int, generates *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"models":[{"name":"m:latest","model":"m:latest"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/generate":
			atomic.AddInt32(&n, 1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"m:latest","response":"hello from the backend","done":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake engine url: %v", err)
	}
	p, _ := strconv.Atoi(u.Port())
	return u.Hostname(), p, &n
}

func TestSecureInferenceClusterMTLS(t *testing.T) {
	baseA, baseB, baseC := t.TempDir(), t.TempDir(), t.TempDir()
	clusterA := filepath.Join(baseA, "cluster")
	clusterB := filepath.Join(baseB, "cluster")
	clusterC := filepath.Join(baseC, "cluster")

	cmBPort := freePort(t)
	cmA := startCM(t, baseA, freePort(t))
	defer cmA.stop()
	cmB := startCM(t, baseB, cmBPort)
	defer cmB.stop()
	cmC := startCM(t, baseC, freePort(t))
	defer cmC.stop()

	aInfo := decodeResult[cmNodeID](t, cmA.call("cluster:get-node-id", nil))
	bInfo := decodeResult[cmNodeID](t, cmB.call("cluster:get-node-id", nil))

	// The cluster-manager listeners need a moment to bind before pairing.
	time.Sleep(time.Second)

	// A founds a cluster and pairs with B (mutual pins land on disk). C founds
	// its own cluster and never pairs with A or B — it is a foreign node.
	cmA.call("cluster:create", map[string]any{"clusterFriendlyName": "Secure Lab"})
	pairNodes(t, cmA, cmB, cmBPort, "node-b")
	cmC.call("cluster:create", map[string]any{"clusterFriendlyName": "Foreign Lab"})

	// Each node's engine is a loopback httptest server. Only B needs a working
	// backend for the A->B path; A routes out to B rather than serving locally.
	bEngineHost, bEnginePort, bGenerates := startFakeOllama(t)

	proxyB := startProxyProc(t, clusterB, freePort(t))
	defer proxyB.stop()
	proxyB.setLocalBackend("ollama", bEngineHost, bEnginePort, true)

	proxyA := startProxyProc(t, clusterA, freePort(t))
	defer proxyA.stop()
	// Tell A that B runs ollama at B's promoted proxy port, as a trusted member.
	proxyA.pushOllamaPeer("node-b", "127.0.0.1", bInfo.NodeUUID, proxyB.port, []string{"m:latest"})
	proxyA.waitForRoutableNode(t)

	genBody := []byte(`{"model":"m:latest","prompt":"hi","stream":false}`)

	// 1. Paired A->B inference succeeds over cluster mTLS, reaching B's engine.
	t.Run("paired A to B over mTLS", func(t *testing.T) {
		before := atomic.LoadInt32(bGenerates)
		resp := postInference(t, fmt.Sprintf("http://127.0.0.1:%d/api/generate", proxyA.port), genBody)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("A->B inference status = %d, body=%s", resp.StatusCode, body)
		}
		if !bytes.Contains(body, []byte("hello from the backend")) {
			t.Fatalf("A->B response did not come from B's engine: %s", body)
		}
		if got := atomic.LoadInt32(bGenerates); got != before+1 {
			t.Fatalf("B engine generate count = %d, want %d (request must reach B's backend)", got, before+1)
		}
	})

	// 2. Foreign node C is rejected by B's mTLS ingress: it can handshake (B
	//    requires any client cert) but its cert is not pinned, so it gets 403.
	t.Run("foreign C rejected by mTLS ingress", func(t *testing.T) {
		client := mtlsClientWithIdentity(t, clusterC)
		resp, err := client.Post(fmt.Sprintf("https://127.0.0.1:%d/api/generate", proxyB.port),
			"application/json", bytes.NewReader(genBody))
		if err != nil {
			t.Fatalf("foreign C dial B ingress: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("foreign C status = %d, want 403; body=%s", resp.StatusCode, b)
		}
	})

	// 3. Deleting A's pin from B's trust store rejects A on the next request:
	//    pins are reloaded per request, so a removed member loses access at once.
	t.Run("deleting pin rejects immediately", func(t *testing.T) {
		pin := filepath.Join(clusterB, "trusted", aInfo.NodeUUID+".json")
		if _, err := os.Stat(pin); err != nil {
			t.Fatalf("expected A's pin in B's trust store at %s: %v", pin, err)
		}
		if err := os.Remove(pin); err != nil {
			t.Fatalf("remove A's pin: %v", err)
		}
		before := atomic.LoadInt32(bGenerates)
		resp := postInference(t, fmt.Sprintf("http://127.0.0.1:%d/api/generate", proxyA.port), genBody)
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("A->B still succeeded (status %d) after B dropped A's pin", resp.StatusCode)
		}
		if got := atomic.LoadInt32(bGenerates); got != before {
			t.Fatalf("B engine was reached (%d != %d) after its pin for A was deleted", got, before)
		}
	})
}

// postInference issues a plaintext POST to a proxy's loopback facade.
func postInference(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// mtlsClientWithIdentity builds an https client that presents the cluster
// identity (node.crt/node.key) found in clusterDir and accepts any server cert
// (the test asserts the server's rejection of the client, not the reverse).
func mtlsClientWithIdentity(t *testing.T, clusterDir string) *http.Client {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(filepath.Join(clusterDir, "node.crt"), filepath.Join(clusterDir, "node.key"))
	if err != nil {
		t.Fatalf("load identity from %s: %v", clusterDir, err)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates:       []tls.Certificate{cert},
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS12,
			},
		},
	}
}
