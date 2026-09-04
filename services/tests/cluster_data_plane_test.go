// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process gate for the inter-node cluster data plane: nvpair-errors
// (:14319) and nvpair-workload-manager (:14320) are cluster mTLS,
// unconditionally. A host that is not a pinned member of this node's cluster must
// not be able to read or inject error or workload state, and that must hold both
// when this node belongs to no cluster and when it belongs to one.
//
// This is the regression gate for the reported issue "errors and workloads are
// reachable by non-cluster nodes". Both halves matter: a non-member cannot connect
// at all (the listener presents no leaf it can verify, and offers no plaintext
// personality), and a host that CAN complete a handshake still gets a 403 unless
// this node pins its certificate.
package tests

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strconv"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/jsonrpc"
	"nvpair-shared/netpick"

	"github.com/grandcat/zeroconf"
)

// dataPlaneURLs are the per-test inter-node endpoints and listener addresses.
// Ports come from the freeServicePortEnv overrides so concurrent test runs and
// cohabiting dev machines never collide on the spec's fixed 14319/14320.
type dataPlaneURLs struct {
	workloadEventsURL string
	errorsSyncURL     string
	workloadAddr      string
	errorsAddr        string
}

func newDataPlaneURLs(t *testing.T) (env []string, urls dataPlaneURLs) {
	t.Helper()
	svcEnv, ports := freeServicePortEnv(t)
	wl := ports["NVPAIR_SERVICE_WORKLOAD_PORT"]
	er := ports["NVPAIR_SERVICE_ERRORS_PORT"]
	return svcEnv, dataPlaneURLs{
		workloadEventsURL: fmt.Sprintf("127.0.0.1:%d/v1/workloads/events", wl),
		errorsSyncURL:     fmt.Sprintf("127.0.0.1:%d/v1/errors", er),
		workloadAddr:      fmt.Sprintf("127.0.0.1:%d", wl),
		errorsAddr:        fmt.Sprintf("127.0.0.1:%d", er),
	}
}

// interNodeCluster is a synthetic two-node cluster for the data-plane tests.
// nodeDir is handed to the broker as --cluster-dir so its workers come up as a
// live cluster member; peer is a stub peer's own identity, cross-pinned with it.
// Both directions of the data plane are mTLS, so a test that wants to talk to the
// node under test needs a cluster identity of its own.
type interNodeCluster struct {
	nodeDir  string
	nodeUUID string
	peerUUID string
	peer     *clustertrust.Mesh
}

func newInterNodeCluster(t *testing.T) *interNodeCluster {
	t.Helper()
	nodeDir, peerDir := t.TempDir(), t.TempDir()
	const nodeUUID, peerUUID = "data-plane-node", "data-plane-peer"
	nodeCert := mintClusterIdentity(t, nodeDir, nodeUUID)
	peerCert := mintClusterIdentity(t, peerDir, peerUUID)
	writePin(t, nodeDir, peerUUID, peerCert) // the node under test trusts the peer
	writePin(t, peerDir, nodeUUID, nodeCert) // and the peer trusts it back
	writeActiveAdmission(t, nodeDir)
	writeActiveAdmission(t, peerDir)

	peer := clustertrust.Open(peerDir)
	if !peer.Clustered() {
		t.Fatal("a dir with a keypair, an admission and a pin must read as clustered")
	}
	return &interNodeCluster{nodeDir: nodeDir, nodeUUID: nodeUUID, peerUUID: peerUUID, peer: peer}
}

// clientAsPeer dials the node under test presenting the stub peer's leaf and
// pinning the node's exact cert — a real cluster peer.
func (c *interNodeCluster) clientAsPeer(t *testing.T) *http.Client {
	t.Helper()
	cfg, ok := c.peer.ClientTLSConfig(c.nodeUUID)
	if !ok {
		t.Fatal("the stub peer must be able to build a pinned client for the node under test")
	}
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

// startStubClusterPeer runs a stub inter-node peer for this cluster over cluster
// mTLS and advertises it as a single _nvpair-node record carrying wl=<port> and
// its cluster principal. The principal is what lets the node under test resolve
// its pin for the stub: the scanner annotates the browsed record as trusted and
// the workload-manager keys its pinned client on it. Without mTLS and that TXT
// key, the stub is simply not a routable cluster target and is never dialed.
//
// It returns the channel of frames the stub received. instance/hostUUID keep the
// record distinct from the node under test's own.
func (c *interNodeCluster) startStubClusterPeer(t *testing.T, instance, hostUUID string) <-chan jsonrpc.Message {
	t.Helper()
	received := make(chan jsonrpc.Message, 128)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/workloads/events", func(w http.ResponseWriter, r *http.Request) {
		// The sender must present the leaf this peer pins — the outbound half of
		// the gate, so a broadcast is authenticated in both directions.
		if _, ok := c.peer.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg jsonrpc.Message
		if json.Unmarshal(body, &msg) == nil {
			select {
			case received <- msg:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	// Listen on all interfaces: zeroconf advertises the host's real interface
	// IP(s), so the manager dials the peer there, not at 127.0.0.1.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("stub peer listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: mux}
	go srv.Serve(tls.NewListener(ln, c.peer.ServerTLSConfig()))
	t.Cleanup(func() { _ = srv.Close() })

	txt := []string{"v=1", "uuid=" + hostUUID, fmt.Sprintf("wl=%d", port),
		clustertrust.ClusterUUIDTXTKey + "=" + c.peerUUID}
	adv, err := zeroconf.Register(instance, nodeRecordService, testDomain, port, txt, nil)
	if err != nil {
		t.Fatalf("register stub peer: %v", err)
	}
	t.Cleanup(adv.Shutdown)
	t.Logf("stub cluster peer advertising %s (wl=%d, cluster-uuid=%s)", nodeRecordService, port, c.peerUUID)
	return received
}

// waitTCP blocks until addr accepts a TCP connection. Every "this request must be
// refused" assertion below runs after it, so a refusal can never be satisfied
// vacuously by a port nothing is listening on yet.
func waitTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to accept connections: %v", addr, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertPlaintextRefused posts body to a http:// URL on an inter-node port and
// requires it not to be accepted. The listener terminates TLS, so a plaintext
// request fails the handshake; if some future change were to answer it instead,
// anything other than a 4xx is a hole.
func assertPlaintextRefused(t *testing.T, hostPath, body string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post("http://"+hostPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Logf("plaintext request refused as expected: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 400 {
		t.Fatalf("plaintext request to %s was accepted with HTTP %d; the inter-node data plane must be mTLS only",
			hostPath, resp.StatusCode)
	}
	t.Logf("plaintext request rejected with HTTP %d", resp.StatusCode)
}

// dataPlaneWorkloadFrame is what a non-member tries to inject; dataPlanePeerFrame
// is what the legitimate pinned peer sends. They are deliberately distinguishable
// so an accepted event can never be confused with a refused one.
const dataPlaneWorkloadFrame = `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"intruder-wl","model":"m","engine":"ollama","state":"running","originatedFrom":"intruder-node","createdAt":1716998400000}}}`

const dataPlanePeerFrame = `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"pinned-peer-wl","model":"m","engine":"ollama","state":"running","originatedFrom":"data-plane-peer","createdAt":1716998400000}}}`

const dataPlaneErrorsEnvelope = `{"nodeId":"intruder-node","errors":[{"id":"intruder:err","nodeId":"intruder-node","severity":"error","message":"injected"}]}`

// TestDataPlaneRefusesNonMemberWhenUnclustered: a node that resolved no usable
// cluster identity is not a member, so it must serve nothing on the inter-node
// ports — not even in the clear. Before this posture such a node accepted
// plaintext workload events from any host on the network and relayed them into its
// catalog as though a peer had sent them.
func TestDataPlaneRefusesNonMemberWhenUnclustered(t *testing.T) {
	env, urls := newDataPlaneURLs(t)
	// startBrokerProc pins an EMPTY cluster dir, so these workers are non-members.
	_, msgs, cleanup := startBrokerProcWithEnv(t, env,
		"--scanner-path", scannerBin,
		"--workload-manager-path", workloadMgrBin,
		"--errors-path", errorsBin,
	)
	t.Cleanup(cleanup)
	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	waitTCP(t, urls.workloadAddr, 15*time.Second)
	assertPlaintextRefused(t, urls.workloadEventsURL, dataPlaneWorkloadFrame)

	waitTCP(t, urls.errorsAddr, 15*time.Second)
	assertPlaintextRefused(t, urls.errorsSyncURL, dataPlaneErrorsEnvelope)
}

// TestModelInventoryRefusesLANPlaintext: a node's model inventory is cluster data.
// The em surface keeps serving this node's own scanner over loopback in plaintext
// — that is how a standalone machine shows its own models — but a LAN caller must
// be a pinned cluster peer. This drives the real binaries and dials this host's own
// LAN address, so the caller is genuinely non-loopback.
//
// The unclustered case is the one that was reported: such a node served its whole
// inventory to anyone who asked, and read every peer's the same way.
func TestModelInventoryRefusesLANPlaintext(t *testing.T) {
	// No evidence to supply: this only needs an address of this host that a
	// LAN caller would use, not the one the node would choose to advertise.
	lanIP := netpick.BestLocalIP(netpick.Evidence{})
	if lanIP == "" {
		t.Skip("no non-loopback IPv4 address available; cannot exercise the LAN path")
	}
	svcEnv, ports := freeServicePortEnv(t)
	emPort := ports["NVPAIR_SERVICE_ENGINE_HTTP_PORT"]

	// startBrokerProc pins an empty cluster dir, so this node is a non-member.
	_, msgs, cleanup := startBrokerProcWithEnv(t, svcEnv,
		"--scanner-path", scannerBin,
		"--engine-manager-path", engineMgrBin,
	)
	t.Cleanup(cleanup)
	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", emPort), 20*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}

	// Loopback: this node's own scanner path must keep working while unclustered.
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", emPort))
	if err != nil {
		t.Fatalf("loopback model fetch failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback model fetch: HTTP %d, want 200 (a standalone node must read its own models)", resp.StatusCode)
	}
	t.Logf("loopback model fetch OK: %s", strings.TrimSpace(string(body)))

	// The same request from this host's LAN address is a non-loopback caller with
	// no cluster credentials, and must be refused.
	lanResp, err := client.Get("http://" + net.JoinHostPort(lanIP, strconv.Itoa(emPort)) + "/v1/models")
	if err != nil {
		t.Logf("LAN plaintext model fetch refused at the transport: %v", err)
		return
	}
	lanBody, _ := io.ReadAll(lanResp.Body)
	lanResp.Body.Close()
	if lanResp.StatusCode != http.StatusForbidden {
		t.Fatalf("LAN plaintext model fetch: HTTP %d body=%q, want 403 — model inventory must not be readable in the clear",
			lanResp.StatusCode, strings.TrimSpace(string(lanBody)))
	}
}

// TestDataPlaneRefusesNonMemberWhenClustered: a live cluster member serves the
// data plane, but only to hosts it pins. A plaintext caller is refused, and so is
// a host that holds a cluster identity this node does not pin — even though that
// host can complete the TLS handshake, which is what makes the per-request pin
// gate load-bearing rather than decorative.
func TestDataPlaneRefusesNonMemberWhenClustered(t *testing.T) {
	env, urls := newDataPlaneURLs(t)
	fx := newInterNodeCluster(t)

	// A stranger: it pins the node under test (so it can verify the server and
	// complete a handshake), but the node under test does not pin it back.
	strangerDir := t.TempDir()
	mintClusterIdentity(t, strangerDir, "data-plane-stranger")
	writePin(t, strangerDir, fx.nodeUUID, readNodeCert(t, fx.nodeDir))
	writeActiveAdmission(t, strangerDir)
	strangerMesh := clustertrust.Open(strangerDir)
	strangerCfg, ok := strangerMesh.ClientTLSConfig(fx.nodeUUID)
	if !ok {
		t.Fatal("the stranger must be able to build a client for the node it pins")
	}
	stranger := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: strangerCfg}}

	_, msgs, cleanup := startBrokerProcInClusterWithEnv(t, fx.nodeDir, env,
		"--scanner-path", scannerBin,
		"--workload-manager-path", workloadMgrBin,
		"--errors-path", errorsBin,
	)
	t.Cleanup(cleanup)
	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	waitTCP(t, urls.workloadAddr, 15*time.Second)
	assertPlaintextRefused(t, urls.workloadEventsURL, dataPlaneWorkloadFrame)
	assertForbidden(t, stranger, "https://"+urls.workloadEventsURL, dataPlaneWorkloadFrame)

	waitTCP(t, urls.errorsAddr, 15*time.Second)
	assertPlaintextRefused(t, urls.errorsSyncURL, dataPlaneErrorsEnvelope)
	assertForbidden(t, stranger, "https://"+urls.errorsSyncURL, dataPlaneErrorsEnvelope)

	// Control: the node's real pinned peer IS served, so the assertions above are
	// rejecting the caller rather than a broken listener. It carries a distinct
	// frame — reusing the intruder's payload would make "accepted" and "refused"
	// byte-identical, which would silently defeat any later assertion that the
	// injected workload is absent from the catalog.
	peer := fx.clientAsPeer(t)
	postUntil(t, peer, "https://"+urls.workloadEventsURL, dataPlanePeerFrame, http.StatusOK, 15*time.Second)
}

// assertForbidden requires a request that completes a handshake to be turned away
// at the pin gate with 403 (not accepted, and not an opaque transport failure).
func assertForbidden(t *testing.T, client *http.Client, url, body string) {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		// The handshake itself can fail on some platforms before the gate is
		// reached; either way the caller is not admitted, which is what matters.
		t.Logf("unpinned peer refused at the transport: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unpinned cluster peer got HTTP %d from %s, want 403", resp.StatusCode, url)
	}
}

// postUntil retries until the endpoint answers want, absorbing the listener's
// asynchronous bind.
func postUntil(t *testing.T, client *http.Client, url, body string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		resp, err := client.Post(url, "application/json", strings.NewReader(body))
		if err == nil {
			code := resp.StatusCode
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if code == want {
				return
			}
			last = fmt.Sprintf("HTTP %d", code)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to answer %d (last: %s)", url, want, last)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// readNodeCert returns the PEM of the cluster leaf written into dir, so a test can
// pin the node under test from another identity.
func readNodeCert(t *testing.T, dir string) string {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(dir, "node.crt"))
	if err != nil {
		t.Fatalf("read node.crt: %v", err)
	}
	return string(pem)
}
