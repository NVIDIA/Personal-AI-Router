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
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/jsonrpc"
)

// mintClusterIdentity writes a self-signed cluster leaf (node.crt + node.key)
// for uuid into dir and returns the certificate PEM (for cross-pinning).
func mintClusterIdentity(t *testing.T, dir, uuid string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "generate key")
	san, err := url.Parse("urn:nvpair:node:" + uuid)
	require.NoError(t, err)
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
	require.NoError(t, err, "create cert")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err, "marshal key")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.crt"), certPEM, 0o600), "write node.crt")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.key"), keyPEM, 0o600), "write node.key")
	return string(certPEM)
}

// writePin writes a trusted/<peerUUID>.json pin for a peer's cert into dir,
// mirroring nvpair-cluster-manager's on-disk pin format.
func writePin(t *testing.T, dir, peerUUID, peerCertPEM string) {
	t.Helper()
	trustedDir := filepath.Join(dir, "trusted")
	require.NoError(t, os.MkdirAll(trustedDir, 0o700), "mkdir trusted")
	pin, err := json.Marshal(map[string]string{"nodeUuid": peerUUID, "certPem": peerCertPEM})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(trustedDir, peerUUID+".json"), pin, 0o600), "write pin")
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
	require.NoError(t, err, "marshal admission")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "admission.json"), admission, 0o600), "write admission.json")
}

// TestRemoteEngineGetInstalled drives engine:remote-get-installed from node A to
// node B over the pin-gated ec mTLS surface and asserts B's engine list comes
// back.
func TestRemoteEngine_ReturnsInstalledEnginesForPeer(t *testing.T) {
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
	require.Nil(t, resp.Error, "remote-get-installed errored")
	var res struct {
		Engines []struct {
			Engine string `json:"engine"`
		} `json:"engines"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &res), "decode result")
	require.NotEmpty(t, res.Engines, "expected B to report at least one engine")
	t.Logf("node A read %d engine(s) from node B over ec mTLS", len(res.Engines))
}

// TestRemoteEngineRejectsUntrusted verifies the ec pin gate: node A can dial B
// (it pins B's server cert) but B does NOT pin A, so the request is refused with
// a 403 that surfaces as a JSON-RPC error rather than an engine list.
func TestRemoteEngine_RejectsUntrusted(t *testing.T) {
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
	require.NotNil(t, resp.Error, "expected an error from an unpinned caller, got result")
	t.Logf("unpinned caller correctly refused: %+v", resp.Error)
}

// engine:remote-cancel-pull goes from node A to node B across the same
// pin-gated ec surface a download is started over. Nothing is downloading on
// B, which is the case worth pinning: the gate that holds a cancel behind the
// pull it names has no attempt to wait for, so it must let the cancel through
// instead of parking it for its whole window, and B must answer a cancel for
// nothing as a no-op rather than an error.
func TestRemoteEngineCancelPull_AnswersAtOnceWithNothingToStop(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	const uuidA, uuidB = "cancel-node-a", "cancel-node-b"
	certA := mintClusterIdentity(t, dirA, uuidA)
	certB := mintClusterIdentity(t, dirB, uuidB)
	writePin(t, dirA, uuidB, certB) // A trusts B
	writePin(t, dirB, uuidA, certA) // B trusts A

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

	t.Run("a cancel for a download nobody started is answered, not stalled", func(t *testing.T) {
		writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":1,"method":"engine:remote-cancel-pull",`+
			`"params":{"node":"nodeB","engine":"ollama","model":"demo"}}`)

		// Comfortably inside the gate's window: were an absent pull to park the
		// cancel there, this would time out rather than return.
		resp := waitForResponse(t, aMsgs, 30*time.Second)
		require.Nil(t, resp.Error, "remote-cancel-pull errored")
		assertCancelAcknowledged(t, resp)
	})

	// The model is what names the download, so a cancel without one has no
	// target. It is refused before anything is sent to the peer.
	t.Run("a cancel naming no model is refused", func(t *testing.T) {
		writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":2,"method":"engine:remote-cancel-pull",`+
			`"params":{"node":"nodeB","engine":"ollama"}}`)

		resp := waitForResponse(t, aMsgs, 15*time.Second)
		require.NotNil(t, resp.Error, "a cancel with no model was accepted: %s", resp.Result)
		assert.Equal(t, -32602, resp.Error.Code, "a cancel with no model was refused for the wrong reason: %+v", resp.Error)
	})
}

// A cancel from node A stops a download that is under way on node B. B's
// engine stands in for Ollama: its pull writes a partial blob and then holds
// the transfer open, so the cancel has a live transfer to stop and a file of
// this download's to clean up. Both of A's requests settle on the download's
// end — the cancel acknowledged, the pull reporting "cancelled" — and the
// cancel is not acknowledged while the partial is still on disk.
func TestRemoteEngineCancelPull_StopsTheDownloadAndRemovesItsPartial(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	const uuidA, uuidB = "active-cancel-a", "active-cancel-b"
	certA := mintClusterIdentity(t, dirA, uuidA)
	certB := mintClusterIdentity(t, dirB, uuidB)
	writePin(t, dirA, uuidB, certB) // A trusts B
	writePin(t, dirB, uuidA, certA) // B trusts A

	// B adopts the daemon rather than launching it, so it looks for the blobs
	// where its own OLLAMA_MODELS says they are.
	models := t.TempDir()
	blobs := filepath.Join(models, "blobs")
	require.NoError(t, os.MkdirAll(blobs, 0o700))
	ollama := startDownloadingOllama(t, blobs)
	configB := t.TempDir()
	writeUserManifest(t, configB, map[string]any{"engine": "ollama", "runtime": map[string]any{"port": ollama.port}})

	portB := freePort(t)
	_, bCleanup := startEngineManagerServer(t, dirB, portB,
		"HOME="+configB, "XDG_CONFIG_HOME="+configB, "APPDATA="+configB, "LOCALAPPDATA="+configB, "USERPROFILE="+configB,
		"OLLAMA_MODELS="+models)
	t.Cleanup(bCleanup)
	waitForPort(t, "127.0.0.1", portB, 10*time.Second)

	aStdin, aMsgs, aCleanup := startEngineManagerStdio(t, dirA)
	t.Cleanup(aCleanup)
	waitForMethod(t, aMsgs, "engine:ready", 10*time.Second)
	snapshot := fmt.Sprintf(`{"jsonrpc":"2.0","method":"discovery:nodes","params":{"nodes":[`+
		`{"hostUuid":"nodeB","name":"nodeB","ip":"127.0.0.1","clusterUuid":%q,"trusted":true,`+
		`"services":{"ec":{"port":%d}},"lastSeen":0}]}}`, uuidB, portB)
	writeRawFrame(t, aStdin, snapshot)

	// Reading B's engines is what has B probe the stand-in and adopt it as a
	// running Ollama; until then a pull there is refused as not running.
	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":1,"method":"engine:remote-get-installed","params":{"node":"nodeB"}}`)
	installed := waitForResponseID(t, aMsgs, 1, 15*time.Second)
	require.Nil(t, installed.Error, "remote-get-installed errored")
	type engineRunning struct {
		Engine  string `json:"engine"`
		Running bool   `json:"running"`
	}
	var engines struct {
		Engines []engineRunning `json:"engines"`
	}
	require.NoError(t, json.Unmarshal(installed.Result, &engines), "decode %s", installed.Result)
	require.Contains(t, engines.Engines, engineRunning{Engine: "ollama", Running: true}, "B did not adopt the stand-in as a running Ollama")

	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":2,"method":"engine:remote-pull-model",`+
		`"params":{"node":"nodeB","engine":"ollama","model":"demo"}}`)
	select {
	case <-ollama.downloading:
	case <-time.After(15 * time.Second):
		require.FailNow(t, "B never started the download")
	}
	// The progress A relays names the model, which is what keys it to the row
	// the user cancels from.
	var progress struct {
		Node  string `json:"node"`
		Model string `json:"model"`
	}
	relayed := waitForMethod(t, aMsgs, "engine:remote-progress", 15*time.Second)
	require.NoError(t, json.Unmarshal(relayed.Params, &progress), "decode %s", relayed.Params)
	require.Equal(t, "nodeB", progress.Node, "progress was not attributed to the peer downloading")
	require.Equal(t, "demo", progress.Model, "progress was not attributed to the model downloading")

	writeRawFrame(t, aStdin, `{"jsonrpc":"2.0","id":3,"method":"engine:remote-cancel-pull",`+
		`"params":{"node":"nodeB","engine":"ollama","model":"demo"}}`)
	// The two replies race, since each settles when the download does.
	replies := make(map[int]jsonrpc.Message)
	var stages []string
	var partialAtAck error
	deadline := time.After(60 * time.Second)
	for len(replies) < 2 {
		select {
		case msg, ok := <-aMsgs:
			require.True(t, ok, "A exited before both requests settled")
			switch {
			case msg.Method == "engine:remote-progress":
				var frame struct {
					Model string `json:"model"`
					Stage string `json:"stage"`
				}
				require.NoError(t, json.Unmarshal(msg.Params, &frame), "decode %s", msg.Params)
				if frame.Model == "demo" {
					stages = append(stages, frame.Stage)
				}
			case msg.Method == "" && idEquals(t, msg.ID, 3):
				// Looked at on arrival: by the pull's reply, a cleanup still
				// running behind an early acknowledgement could have finished.
				_, partialAtAck = os.Stat(ollama.partial)
				replies[3] = msg
			case msg.Method == "" && idEquals(t, msg.ID, 2):
				replies[2] = msg
			}
		case <-deadline:
			require.FailNow(t, "the cancel and the pull did not both settle", "settled: %v", replies)
		}
	}

	assert.ErrorIs(t, partialAtAck, os.ErrNotExist, "the cancel was acknowledged with the download's partial still on disk")
	cancel := replies[3]
	require.Nil(t, cancel.Error, "remote-cancel-pull errored")
	assertCancelAcknowledged(t, cancel)
	pull := replies[2]
	require.Nil(t, pull.Error, "the pull failed instead of settling as cancelled")
	var settled struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(pull.Result, &settled), "decode %s", pull.Result)
	assert.Equal(t, "cancelled", settled.Result.Status, "the pull did not settle as cancelled")
	assert.Contains(t, stages, "cancelled", "A never relayed how the download ended")
	select {
	case <-ollama.stopped:
	case <-time.After(10 * time.Second):
		assert.Fail(t, "the transfer was still open after the cancel was acknowledged")
	}
}

// assertCancelAcknowledged checks a peer's answer to a cancel is its
// acknowledgement.
func assertCancelAcknowledged(t *testing.T, resp jsonrpc.Message) {
	t.Helper()
	var res struct {
		OK bool `json:"ok"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &res), "decode %s", resp.Result)
	assert.True(t, res.OK, "B did not acknowledge the cancel: %s", resp.Result)
}

// downloadingOllama stands in for an Ollama daemon. It answers the readiness
// probe and the other polls with an empty success, and a pull writes a partial
// blob, reports progress against its digest, then holds the transfer open
// until the client drops it.
type downloadingOllama struct {
	port    int
	partial string
	// downloading closes once the partial is on disk and its progress is
	// sent; stopped, once the client has dropped the transfer.
	downloading, stopped chan struct{}
}

func startDownloadingOllama(t *testing.T, blobs string) *downloadingOllama {
	t.Helper()
	digest := "sha256:" + strings.Repeat("ab", 32)
	o := &downloadingOllama{
		partial:     filepath.Join(blobs, strings.ReplaceAll(digest, ":", "-")+"-partial"),
		downloading: make(chan struct{}),
		stopped:     make(chan struct{}),
	}
	var downloading, stopped sync.Once
	quit := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pull", func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, os.WriteFile(o.partial, []byte("partial"), 0o600), "write the partial blob") {
			return
		}
		_, err := fmt.Fprintf(w, `{"status":"pulling","digest":%q,"total":1000,"completed":10}`+"\n", digest)
		if !assert.NoError(t, err, "send pull progress") {
			return
		}
		w.(http.Flusher).Flush()
		downloading.Do(func() { close(o.downloading) })
		select {
		case <-r.Context().Done():
			stopped.Do(func() { close(o.stopped) })
		case <-quit:
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	// Runs before Close, which would otherwise wait on a transfer nobody
	// dropped.
	t.Cleanup(func() { close(quit) })
	o.port = server.Listener.Addr().(*net.TCPAddr).Port
	return o
}

// startEngineManagerServer launches an engine-manager serving the ec surface on
// controlPort with the given cluster dir, adding env to the environment it
// inherits. It keeps stdin open (so the process stays alive) and drains stdout.
// Returns stdin and a cleanup.
func startEngineManagerServer(t *testing.T, clusterDir string, controlPort int, env ...string) (io.WriteCloser, func()) {
	t.Helper()
	cmd := exec.Command(engineMgrBin,
		"--control-port", fmt.Sprintf("%d", controlPort),
		"--cluster-dir", clusterDir,
		"--log-level", "warn",
	)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "engine-manager stdin pipe")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "engine-manager stdout pipe")
	require.NoError(t, cmd.Start(), "start engine-manager (server)")
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
	require.NoError(t, err, "engine-manager stdin pipe")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "engine-manager stdout pipe")
	require.NoError(t, cmd.Start(), "start engine-manager (client)")
	msgs := startMsgReader(t, stdout)
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
