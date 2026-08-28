// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process test for remote engine management over the engine-manager ec
// surface. Two real nvpair-engine-manager binaries are put into the same "cluster"
// by minting a self-signed Ed25519 leaf for each (with the urn:nvpair:node:<uuid>
// URI SAN nvpair-shared/clustertrust keys on) and cross-pinning them, exactly as
// nvpair-cluster-manager would after pairing. Node B serves the ec surface over
// pin-based mTLS; node A dials it. The peer directory A resolves the target in
// is seeded by pushing a discovery:nodes snapshot to A's stdin (the same frame
// the broker relay would push), so the test needs no broker.
package tests

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// mintClusterIdentity writes a self-signed cluster leaf (node.crt + node.key)
// for uuid into dir and returns the certificate PEM (for cross-pinning).
func mintClusterIdentity(t *testing.T, dir, uuid string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	san, _ := url.Parse("urn:nvpair:node:" + uuid)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: uuid},
		URIs:                  []*url.URL{san},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o600); err != nil {
		t.Fatalf("write node.crt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("write node.key: %v", err)
	}
	return string(certPEM)
}

// writePin writes a trusted/<peerUUID>.json pin for a peer's cert into dir,
// mirroring nvpair-cluster-manager's on-disk pin format.
func writePin(t *testing.T, dir, peerUUID, peerCertPEM string) {
	t.Helper()
	trustedDir := filepath.Join(dir, "trusted")
	if err := os.MkdirAll(trustedDir, 0o700); err != nil {
		t.Fatalf("mkdir trusted: %v", err)
	}
	pin, _ := json.Marshal(map[string]string{"nodeUuid": peerUUID, "certPem": peerCertPEM})
	if err := os.WriteFile(filepath.Join(trustedDir, peerUUID+".json"), pin, 0o600); err != nil {
		t.Fatalf("write pin: %v", err)
	}
}

// writeActiveAdmission marks dir as belonging to a cluster without adding any
// peer pins. This models a live cluster-of-one membership.
func writeActiveAdmission(t *testing.T, dir string) {
	t.Helper()
	admission, err := json.Marshal(map[string]any{
		"counter":   1,
		"activated": 1,
		"clusterId": "remote-engine-test-cluster",
		"epoch":     1,
	})
	if err != nil {
		t.Fatalf("marshal admission: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "admission.json"), admission, 0o600); err != nil {
		t.Fatalf("write admission.json: %v", err)
	}
}

// TestRemoteEngineGetInstalled drives engine:remote-get-installed from node A to
// node B over the pin-gated ec mTLS surface and asserts B's engine list comes
// back.
func TestRemoteEngineGetInstalled(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	const uuidA, uuidB = "test-node-a", "test-node-b"
	certA := mintClusterIdentity(t, dirA, uuidA)
	certB := mintClusterIdentity(t, dirB, uuidB)
	writePin(t, dirA, uuidB, certB) // A trusts B
	writePin(t, dirB, uuidA, certA) // B trusts A

	// Node B: serve the ec surface over mTLS. Give it a live stdin so it stays
	// up (EOF would shut it down) and drain its stdout so it can't block.
	portB := freePort(t)
	_, bCleanup := startEngineManagerServer(t, dirB, portB)
	t.Cleanup(bCleanup)
	waitForPort(t, "127.0.0.1", portB, 10*time.Second)

	// Node A: the client. It resolves targets from its ec peer directory, which
	// we seed by pushing a discovery:nodes snapshot (below) rather than a broker.
	aStdin, aMsgs, aCleanup := startEngineManagerStdio(t, dirA)
	t.Cleanup(aCleanup)
	waitForMethod(t, aMsgs, "engine:ready", 10*time.Second)

	// Seed A's ec peer directory with node B. handleMessage processes this
	// synchronously before the next frame, so the directory is populated by the
	// time the remote request is read.
	snapshot := fmt.Sprintf(`{"jsonrpc":"2.0","method":"discovery:nodes","params":{"nodes":[`+
		`{"hostUuid":"nodeB","name":"nodeB","ip":"127.0.0.1","clusterUuid":%q,"trusted":true,`+
		`"services":{"ec":{"port":%d}},"lastSeen":0}]}}`, uuidB, portB)
	writeRawFrame(t, aStdin, snapshot)

	// Fire the remote read and await the response.
	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":1,"method":"engine:remote-get-installed","params":{"node":"nodeB"}}`)
	resp := waitForResponse(t, aMsgs, 15*time.Second)
	if resp.Error != nil {
		t.Fatalf("remote-get-installed errored: %+v", resp.Error)
	}
	var res struct {
		Engines []struct {
			Engine string `json:"engine"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode result %s: %v", resp.Result, err)
	}
	if len(res.Engines) == 0 {
		t.Fatalf("expected B to report at least one engine, got %s", resp.Result)
	}
	t.Logf("node A read %d engine(s) from node B over ec mTLS", len(res.Engines))
}

// TestRemoteEngineRejectsUntrusted verifies the ec pin gate: node A can dial B
// (it pins B's server cert) but B does NOT pin A, so the request is refused with
// a 403 that surfaces as a JSON-RPC error rather than an engine list.
func TestRemoteEngineRejectsUntrusted(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	const uuidA, uuidB = "untrusted-a", "server-b"
	_ = mintClusterIdentity(t, dirA, uuidA)
	certB := mintClusterIdentity(t, dirB, uuidB)
	writePin(t, dirA, uuidB, certB) // A trusts B (so A can complete the handshake)
	writeActiveAdmission(t, dirB)   // B is clustered, but still does not trust A
	// Deliberately do NOT pin A into B: B must reject A at the per-request gate.

	portB := freePort(t)
	_, bCleanup := startEngineManagerServer(t, dirB, portB)
	t.Cleanup(bCleanup)
	waitForPort(t, "127.0.0.1", portB, 10*time.Second)

	aStdin, aMsgs, aCleanup := startEngineManagerStdio(t, dirA)
	t.Cleanup(aCleanup)
	waitForMethod(t, aMsgs, "engine:ready", 10*time.Second)

	snapshot := fmt.Sprintf(`{"jsonrpc":"2.0","method":"discovery:nodes","params":{"nodes":[`+
		`{"hostUuid":"nodeB","name":"nodeB","ip":"127.0.0.1","clusterUuid":%q,"trusted":true,`+
		`"services":{"ec":{"port":%d}},"lastSeen":0}]}}`, uuidB, portB)
	writeRawFrame(t, aStdin, snapshot)
	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":1,"method":"engine:remote-get-installed","params":{"node":"nodeB"}}`)

	resp := waitForResponse(t, aMsgs, 15*time.Second)
	if resp.Error == nil {
		t.Fatalf("expected an error from an unpinned caller, got result %s", resp.Result)
	}
	t.Logf("unpinned caller correctly refused: %+v", resp.Error)
}

// startEngineManagerServer launches an engine-manager serving the ec surface on
// controlPort with the given cluster dir. It keeps stdin open (so the process
// stays alive) and drains stdout. Returns stdin and a cleanup.
func startEngineManagerServer(t *testing.T, clusterDir string, controlPort int) (io.WriteCloser, func()) {
	t.Helper()
	cmd := exec.Command(engineMgrBin,
		"--control-port", fmt.Sprintf("%d", controlPort),
		"--cluster-dir", clusterDir,
		"--log-level", "warn",
	)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("engine-manager stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("engine-manager stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start engine-manager (server): %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	return stdin, func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
}

// startEngineManagerStdio launches an engine-manager as a JSON-RPC-over-stdio
// client. Returns its stdin, a reader over its stdout frames, and a cleanup.
func startEngineManagerStdio(t *testing.T, clusterDir string) (io.WriteCloser, <-chan jsonrpc.Message, func()) {
	t.Helper()
	cmd := exec.Command(engineMgrBin, "--cluster-dir", clusterDir, "--log-level", "warn")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("engine-manager stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("engine-manager stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start engine-manager (client): %v", err)
	}
	msgs := startMsgReader(stdout)
	return stdin, msgs, func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
}
