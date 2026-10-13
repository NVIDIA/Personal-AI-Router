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

	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
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
	// The engine and its port arrive over facade/enable below; only the cluster
	// dir is process-scoped enough to stay on argv.
	cmd := exec.Command(proxyBin, "--cluster-dir", clusterDir)
	cmd.Env = append(os.Environ(),
		"HOME="+cfg, "XDG_CONFIG_HOME="+cfg, "APPDATA="+cfg, "LOCALAPPDATA="+cfg,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "proxy stdin pipe")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "proxy stdout pipe")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start(), "start proxy")
	p := &proxyProc{t: t, cmd: cmd, stdin: stdin, msgs: startMsgReader(stdout), nextID: 1}

	// The mTLS ingress under test is engine-agnostic; Ollama is an arbitrary
	// pick. The bound port comes from the enable response rather than the ready
	// notification, which is where the broker reads it too.
	resp := p.call("facade/enable", map[string]any{
		"engine":              "ollama",
		"port":                listenPort,
		"ignorePersistedPort": true,
	})
	var enabled struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &enabled), "facade/enable result")
	require.NotEqual(t, 0, enabled.Port, "facade/enable result")
	p.port = enabled.Port
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
			require.True(p.t, ok, "proxy stdout closed unexpectedly")
			if want(m) {
				return m
			}
			p.buf = append(p.buf, m)
		case <-timer.C:
			require.FailNow(p.t, "timed out waiting on proxy message")
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
	_, err := p.stdin.Write(b)
	require.NoError(p.t, err, "write %s", method)
	resp := p.pump(func(m jsonrpc.Message) bool { return m.Method == "" && idEquals(m.ID, id) }, 15*time.Second)
	require.Nil(p.t, resp.Error, "%s returned a JSON-RPC error", method)
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
	_, err := p.stdin.Write(b)
	require.NoError(p.t, err, "notify %s", method)
}

// setLocalBackend points one facade's cluster ingress + local self candidate at
// a loopback engine. Addressed to that engine, the way the broker sends it.
func (p *proxyProc) setLocalBackend(engine, host string, port int, healthy bool) {
	p.call(engines.AddressMethod(engine, "node/set-local-backend"), map[string]any{
		"engine": engine, "host": host, "port": port, "healthy": healthy,
	})
}

// pushOllamaPeer feeds the proxy a discovery:nodes snapshot advertising a single
// remote peer that runs ollama at proxyPort, tagged as a trusted cluster member
// keyed by clusterUUID (the peer's cluster cert principal).
func (p *proxyProc) pushOllamaPeer(name, ip, clusterUUID string, proxyPort int, models []string) {
	p.notify(engines.AddressMethod("ollama", "discovery:nodes"), map[string]any{
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
		resp := p.call(engines.AddressMethod("ollama", "nodes/list"), nil)
		var r struct {
			Nodes []json.RawMessage `json:"nodes"`
		}
		if json.Unmarshal(resp.Result, &r) == nil && len(r.Nodes) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.FailNow(t, "proxy never registered the pushed discovery node")
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
	require.NoError(t, err, "parse fake engine url")
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
		require.Equal(t, http.StatusOK, resp.StatusCode, "A->B inference status (%v)", body)
		require.Contains(t, string(body), "hello from the backend", "A->B response did not come from B's engine")
		require.Equal(t, before+1, atomic.LoadInt32(bGenerates), "request must reach B's engine")
	})

	// 2. Foreign node C is rejected by B's mTLS ingress: it can handshake (B
	//    requires any client cert) but its cert is not pinned, so it gets 403.
	t.Run("foreign C rejected by mTLS ingress", func(t *testing.T) {
		client := mtlsClientWithIdentity(t, clusterC)
		resp, err := client.Post(fmt.Sprintf("https://127.0.0.1:%d/api/generate", proxyB.port),
			"application/json", bytes.NewReader(genBody))
		require.NoError(t, err, "foreign C dial B ingress")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			b, _ := io.ReadAll(resp.Body)
			require.FailNow(t, fmt.Sprintf("foreign C status = %d, want 403; body=%s", resp.StatusCode, b))
		}
	})

	// 3. Deleting A's pin from B's trust store rejects A on the next request:
	//    pins are reloaded per request, so a removed member loses access at once.
	t.Run("deleting pin rejects immediately", func(t *testing.T) {
		pin := filepath.Join(clusterB, "trusted", aInfo.NodeUUID+".json")
		require.FileExists(t, pin, "expected A's pin in B's trust store")
		require.NoError(t, os.Remove(pin), "remove A's pin")
		before := atomic.LoadInt32(bGenerates)
		resp := postInference(t, fmt.Sprintf("http://127.0.0.1:%d/api/generate", proxyA.port), genBody)
		defer resp.Body.Close()
		require.NotEqual(t, http.StatusOK, resp.StatusCode, "A->B still succeeded (status")
		require.Equal(t, before, atomic.LoadInt32(bGenerates), "B's engine must not be reached after its pin for A is deleted")
	})
}

// postInference issues a plaintext POST to a proxy's loopback facade.
func postInference(t *testing.T, url string, body []byte) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	require.NoError(t, err, "POST (%v)", url)
	return resp
}

// mtlsClientWithIdentity builds an https client that presents the cluster
// identity (node.crt/node.key) found in clusterDir and accepts any server cert
// (the test asserts the server's rejection of the client, not the reverse).
func mtlsClientWithIdentity(t *testing.T, clusterDir string) *http.Client {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(filepath.Join(clusterDir, "node.crt"), filepath.Join(clusterDir, "node.key"))
	require.NoError(t, err, "load identity from (%v)", clusterDir)
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
