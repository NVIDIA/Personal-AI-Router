// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process gate for a PAIR peer that can only be reached by a typed
// address — the shape of every peer on a Tailscale tailnet, where no multicast
// crosses and nothing is ever discovered.
//
// Node A is a real broker with its real workers. Node B is a real ollama-proxy
// serving cluster mTLS in front of a fake engine, plus the two HTTP surfaces a
// peer exposes: a plain node-info reporting its identity and service map, and a
// pin-gated engine manager serving its model list. The two nodes cross-pin, as
// they would after pairing.
//
// A is then told about B with nothing but `node/add`, and must end up treating it
// exactly as if it had discovered it:
//
//   - B appears in A's directory with its identity, hardware and models, the
//     models having been read over cluster mTLS from B's engine manager;
//   - A's proxy routes an inference request to B over cluster mTLS and B's
//     engine serves it;
//   - and A never opens a plaintext connection to B's engine ports. That is the
//     regression gate: on a PAIR node those ports are proxy facades that refuse
//     plaintext, so probing them reported a healthy peer as having no engines.
//
// Two real brokers cannot share one loopback — every broker-owned port is a
// compiled-in constant — so B is a stub peer on ephemeral ports, addressed
// through node/add's per-service port overrides. Everything the assertions turn
// on (the mTLS identities, the pins, the proxy, the transport choice) is real.

package tests

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/jsonrpc"
)

// connectionTrap is a listener that accepts and immediately closes, counting
// every connection. It stands in for a port A must never touch.
type connectionTrap struct {
	port  int
	count atomic.Int32
}

func startConnectionTrap(t *testing.T) *connectionTrap {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("trap listen: %v", err)
	}
	trap := &connectionTrap{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			trap.count.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return trap
}

// startPeerNodeInfo serves B's /v1/node-info in plaintext, the way a PAIR node
// under a broker does. Its service map is what identifies B as a PAIR node and
// tells A where B's proxy and engine manager listen.
func startPeerNodeInfo(t *testing.T, hostUUID, clusterUUID string, services map[string]int) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("node-info listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	services["ni"] = port
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"GPUs":           []map[string]any{{"name": "NVIDIA GeForce RTX 4090", "utilization_percent": 11}},
			"telemetryValid": true,
			"msSince":        120,
			"hostUuid":       hostUUID,
			"clusterUuid":    clusterUUID,
			"services":       services,
		})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return port
}

// startPeerEngineManager serves B's /v1/models over cluster mTLS, refusing any
// caller B does not pin — the same gate the real engine manager applies.
func startPeerEngineManager(t *testing.T, mesh *clustertrust.Mesh, models []string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("engine-manager listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := mesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models":         models,
			"modelsByEngine": map[string][]string{"ollama": models},
			"loadedByEngine": map[string][]string{"ollama": {}},
		})
	})
	cfg := mesh.ServerTLSConfig()
	if cfg == nil {
		t.Fatal("the stub peer must be able to serve cluster mTLS")
	}
	srv := &http.Server{Handler: mux, TLSConfig: cfg}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	t.Cleanup(func() { _ = srv.Close() })
	return port
}

// brokerProc drives a real broker over stdio.
type brokerProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	msgs   <-chan jsonrpc.Message
	buf    []jsonrpc.Message
	nextID int
}

func startBrokerWithClusterDir(t *testing.T, clusterDir string) *brokerProc {
	t.Helper()
	cfg := t.TempDir()
	cmd := exec.Command(brokerBin,
		"--cluster-dir", clusterDir,
		"--scanner-path", scannerBin,
		"--node-info-path", nodeInfoBin,
		"--proxy-path", proxyBin,
		"--lmstudio-proxy-path", lmstudioProxyBin,
		"--workload-manager-path", workloadMgrBin,
		"--errors-path", errorsBin,
		"--engine-manager-path", engineMgrBin,
		"--manual-nodes-path", manualNodesBin,
		"--settings-path", nodeSettingsBin,
		"--cluster-manager-path", clusterMgrBin,
		"--scheduler-path", schedulerBin,
	)
	cmd.Env = append(os.Environ(),
		"HOME="+cfg, "XDG_CONFIG_HOME="+cfg, "APPDATA="+cfg, "LOCALAPPDATA="+cfg,
	)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("broker stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("broker stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	b := &brokerProc{t: t, cmd: cmd, stdin: stdin, msgs: startMsgReader(stdout), nextID: 1}
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return b
}

func (b *brokerProc) call(method string, params any) jsonrpc.Message {
	b.t.Helper()
	id := b.nextID
	b.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, _ := json.Marshal(req)
	raw = append(raw, '\n')
	if _, err := b.stdin.Write(raw); err != nil {
		b.t.Fatalf("write %s: %v", method, err)
	}
	resp := b.pump(func(m jsonrpc.Message) bool { return m.Method == "" && idEquals(m.ID, id) }, 20*time.Second)
	if resp.Error != nil {
		b.t.Fatalf("%s returned error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp
}

func (b *brokerProc) pump(want func(jsonrpc.Message) bool, timeout time.Duration) jsonrpc.Message {
	b.t.Helper()
	for i, m := range b.buf {
		if want(m) {
			b.buf = append(b.buf[:i], b.buf[i+1:]...)
			return m
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-b.msgs:
			if !ok {
				b.t.Fatal("broker stdout closed unexpectedly")
			}
			if want(m) {
				return m
			}
			b.buf = append(b.buf, m)
		case <-timer.C:
			b.t.Fatal("timed out waiting on a broker message")
		}
	}
}

func (b *brokerProc) nodes() []availableNode {
	b.t.Helper()
	var result struct {
		Nodes []availableNode `json:"nodes"`
	}
	if err := json.Unmarshal(b.call("discovery:get-nodes", nil).Result, &result); err != nil {
		b.t.Fatalf("decode discovery:get-nodes: %v", err)
	}
	return result.Nodes
}

// awaitNode polls the directory until a node matching want appears.
func (b *brokerProc) awaitNode(hostUUID string, want func(availableNode) bool, why string) availableNode {
	b.t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last []availableNode
	for time.Now().Before(deadline) {
		last = b.nodes()
		for _, n := range last {
			if n.HostUUID == hostUUID && want(n) {
				return n
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	b.t.Fatalf("node %s never %s; directory = %+v", hostUUID, why, last)
	return availableNode{}
}

func TestManualPairPeerBehavesLikeADiscoveredPeer(t *testing.T) {
	baseA, baseB := t.TempDir(), t.TempDir()
	dirA := filepath.Join(baseA, "cluster")
	dirB := filepath.Join(baseB, "cluster")

	// B runs a real cluster-manager so the pairing below is the real EAP-NOOB
	// exchange, and the pins both sides end up holding are the ones it mints.
	portB := freePort(t)
	cmB := startCM(t, baseB, portB)
	t.Cleanup(cmB.stop)

	// A is a real broker with its own real cluster-manager.
	brokerA := startBrokerWithClusterDir(t, dirA)
	brokerA.call("discovery:subscribe", nil)
	brokerA.call("proxy:subscribe", nil)

	// The listeners need a moment to bind before pairing.
	time.Sleep(time.Second)

	// Pair A to B by address and port alone — no nodeId, because A has not
	// discovered B and never will. This is the invite an operator sends after
	// typing a peer's overlay address into Add node.
	pairFromBroker(t, brokerA, cmB, portB)

	bInfo := decodeResult[cmNodeID](t, cmB.call("cluster:get-node-id", nil))
	if bInfo.NodeUUID == "" {
		t.Fatal("the peer must report a node uuid after pairing")
	}
	uuidB := bInfo.NodeUUID

	meshB := clustertrust.Open(dirB)
	meshB.Refresh()
	if !meshB.Clustered() {
		t.Fatal("the peer must read as a cluster member after pairing")
	}

	// B: a real ollama-proxy over cluster mTLS in front of a fake engine.
	engineHost, enginePort, generates := startFakeOllama(t)
	proxyB := startProxyProc(t, dirB, freePort(t))
	t.Cleanup(proxyB.stop)
	proxyB.setLocalBackend("ollama", engineHost, enginePort, true)

	// B's two HTTP surfaces, and two traps standing where a bare host's engines
	// would be. On a PAIR node those ports carry proxy facades; A must not touch
	// them.
	emPort := startPeerEngineManager(t, meshB, []string{"m:latest"})
	ollamaTrap := startConnectionTrap(t)
	lmStudioTrap := startConnectionTrap(t)
	niPort := startPeerNodeInfo(t, uuidB, uuidB, map[string]int{
		"ol": proxyB.port,
		"em": emPort,
		"cl": portB,
	})

	// Everything A is told about B. No discovery, no mDNS: one address and the
	// ports its services sit on.
	brokerA.call("node/add", map[string]any{
		"address": "127.0.0.1",
		"name":    "peer-b",
		"ports": map[string]any{
			"node_info": niPort,
			"ollama":    ollamaTrap.port,
			"lmstudio":  lmStudioTrap.port,
		},
	})

	t.Run("B joins A's directory as a trusted peer with its models", func(t *testing.T) {
		node := brokerA.awaitNode(uuidB, func(n availableNode) bool {
			return len(n.Models) > 0
		}, "appeared with a model list")
		if !node.Trusted {
			t.Errorf("node.trusted = false, want true: A holds B's pin")
		}
		if !node.Clustered {
			t.Errorf("node.clustered = false, want true: B reported a cluster principal")
		}
		if node.Models[0] != "m:latest" {
			t.Errorf("models = %v, want B's inventory read over cluster mTLS", node.Models)
		}
		if node.IPAddress != "127.0.0.1" {
			t.Errorf("ipAddress = %q, want the address the operator typed", node.IPAddress)
		}
	})

	t.Run("A routes inference to B over cluster mTLS", func(t *testing.T) {
		port := awaitRoutablePeerAtProxy(t, brokerA, uuidB)
		before := atomic.LoadInt32(generates)
		resp := postInference(t, fmt.Sprintf("http://127.0.0.1:%d/api/generate", port),
			[]byte(`{"model":"m:latest","prompt":"hi","stream":false}`))
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("A->B inference status = %d, body = %s", resp.StatusCode, payload)
		}
		if !bytes.Contains(payload, []byte("hello from the backend")) {
			t.Fatalf("response did not come from B's engine: %s", payload)
		}
		if got := atomic.LoadInt32(generates); got != before+1 {
			t.Fatalf("B engine generate count = %d, want %d", got, before+1)
		}
	})

	// The regression gate. A PAIR node's engine ports are proxy facades that
	// refuse plaintext from anything but loopback; probing them reports a healthy
	// peer as having no engines, and bridging them gives the proxy a second route
	// to the same node that can only ever 403.
	t.Run("A never opens a plaintext connection to B's engine ports", func(t *testing.T) {
		if n := ollamaTrap.count.Load(); n != 0 {
			t.Errorf("%d plaintext connections to B's ollama port, want 0", n)
		}
		if n := lmStudioTrap.count.Load(); n != 0 {
			t.Errorf("%d plaintext connections to B's lmstudio port, want 0", n)
		}
	})
}

// pairFromBroker drives a PIN pairing from the broker's own cluster-manager to a
// standalone peer, addressed by nothing but an address and a port. It is the
// invite an operator sends after typing a peer's overlay address into Add node:
// no nodeId, because the peer was never discovered and never will be.
func pairFromBroker(t *testing.T, b *brokerProc, joiner *cmProc, joinerPort int) {
	t.Helper()
	var invite struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	resp := b.call("cluster:invite-node", map[string]any{
		"address": "127.0.0.1",
		"port":    joinerPort,
	})
	if err := json.Unmarshal(resp.Result, &invite); err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	if invite.State != "pending" || len(invite.Pin) != 6 {
		t.Fatalf("invite-node = %+v, want pending with a six-digit pin", invite)
	}
	joiner.waitNotify("cluster:invite-received")
	var accepted struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(joiner.call("cluster:respond-to-invite", map[string]any{
		"inviteId": invite.InviteID, "accept": true, "pin": invite.Pin,
	}).Result, &accepted); err != nil {
		t.Fatalf("decode respond-to-invite: %v", err)
	}
	if accepted.State != "paired" {
		t.Fatalf("respond-to-invite state = %q, want paired", accepted.State)
	}
}

// awaitRoutablePeerAtProxy waits until the broker's ollama-proxy lists the peer
// as a routing target and returns the proxy's listen port.
func awaitRoutablePeerAtProxy(t *testing.T, b *brokerProc, hostUUID string) int {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var lastNodes json.RawMessage
	for time.Now().Before(deadline) {
		var status struct {
			Port int `json:"port"`
		}
		if err := json.Unmarshal(b.call("proxy:get-status", nil).Result, &status); err == nil && status.Port > 0 {
			var listed struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
			}
			resp := b.call("proxy:nodes/list", nil)
			lastNodes = resp.Result
			if json.Unmarshal(resp.Result, &listed) == nil {
				for _, n := range listed.Nodes {
					if n.ID == hostUUID {
						return status.Port
					}
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("the proxy never listed %s as a routing target; nodes = %s", hostUUID, lastNodes)
	return 0
}
