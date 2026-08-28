// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generateTestPKI creates a fresh CA and a server cert + key signed
// by it, plus a client cert + key signed by the same CA. Everything
// stays in-memory until the caller writes the PEM bytes; nothing
// touches the user's keychain or the system trust store. Returns
// PEM-encoded byte slices for: CA cert, server cert, server key,
// client cert, client key.
func generateTestPKI(t *testing.T, host string) (caPEM, serverCertPEM, serverKeyPEM, clientCertPEM, clientKeyPEM []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nvpair-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caTmpl, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}
	serverCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	serverKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("client key: %v", err)
	}
	clientTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "nvpair-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTmpl, caTmpl, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	clientCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	clientKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER})

	return
}

// writePEM writes the given PEM bytes to a freshly created file
// inside dir under the given name. Returns the absolute path the
// node-info subprocess can be pointed at.
func writePEM(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// startNodeInfo launches the nvpair-node-info binary built by
// TestMain, with the given extra flags. Returns a cleanup func
// that closes stdin (graceful shutdown) and waits for the process
// to exit. The test doesn't read stdout because all responses
// arrive on the wire — stderr is forwarded to the test log so
// fatal startup errors are visible if the test fails.
func startNodeInfo(t *testing.T, instanceName string, extraArgs ...string) (cleanup func()) {
	t.Helper()
	// node-info no longer advertises over mDNS (discovery consolidation), so
	// these tests connect directly to the ports they pass.
	cmd := exec.Command(nodeInfoBin, extraArgs...)
	cmd.Stderr = os.Stderr
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("node-info stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node-info: %v", err)
	}
	t.Logf("node-info %q started: pid=%d args=%v", instanceName, cmd.Process.Pid, extraArgs)

	return func() {
		stdinPipe.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
}

// freePort grabs an OS-allocated TCP port and immediately closes
// the listener, returning the port number. There's a tiny race
// here — between Close() and the subprocess binding — but on a
// quiet test host it's fine, and using a randomly-allocated port
// avoids stomping on whatever the developer has running locally.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// httpsClientWithCert builds an *http.Client that trusts the test
// CA and presents the test client cert. Used by mTLS-positive
// test paths.
func httpsClientWithCert(t *testing.T, caPEM, clientCertPEM, clientKeyPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA pool: no certs parsed")
	}
	cert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("client keypair: %v", err)
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			},
		},
	}
}

// httpsClientNoCert builds an *http.Client that trusts the test
// CA but presents no client cert. Used to verify that mTLS
// rejects unauthenticated clients.
func httpsClientNoCert(t *testing.T, caPEM []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA pool: no certs parsed")
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}
}

// portReachable does a quick TCP connect to the given port and
// returns true if the dial succeeds. Used to check that the HTTP
// listener really did stay down when --accept-http wasn't set.
func portReachable(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForPort polls TCP-connect on the given port and returns once
// it's listening. Used after launching the nvpair-node-info subprocess
// because GPU/CPU detection runs synchronously before the
// listeners bind, and on a busy host that can take 1-2 seconds.
// A fixed sleep would either be flaky (too short) or wasteful
// (too long); this returns as soon as the listener is up.
func waitForPort(t *testing.T, host string, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portReachable(host, port) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("port %s:%d did not start listening within %s", host, port, timeout)
}

func TestNodeInfoHTTPSWithMTLS(t *testing.T) {
	dir := t.TempDir()
	caPEM, scPEM, skPEM, ccPEM, ckPEM := generateTestPKI(t, "localhost")
	caPath := writePEM(t, dir, "ca.pem", caPEM)
	scPath := writePEM(t, dir, "server.crt", scPEM)
	skPath := writePEM(t, dir, "server.key", skPEM)

	httpPort := freePort(t)
	tlsPort := freePort(t)
	instance := fmt.Sprintf("tls-mtls-%d", tlsPort)

	cleanup := startNodeInfo(t,
		instance,
		"--port", fmt.Sprintf("%d", httpPort),
		"--tls-port", fmt.Sprintf("%d", tlsPort),
		"--cert", scPath,
		"--key", skPath,
		"--client-ca", caPath,
		// HTTP intentionally NOT accepted: verifies that the
		// default of --accept-http=false really does shut the
		// legacy listener.
	)
	t.Cleanup(cleanup)

	waitForPort(t, "127.0.0.1", tlsPort, 10*time.Second)

	// 1. HTTPS with the right client cert succeeds.
	client := httpsClientWithCert(t, caPEM, ccPEM, ckPEM)
	url := fmt.Sprintf("https://localhost:%d/v1/node-info", tlsPort)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("authenticated mTLS GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated mTLS GET status = %d, body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "GPUs") {
		t.Errorf("response body missing GPUs key: %s", body)
	}
	t.Logf("mTLS-authenticated GET succeeded (%d bytes)", len(body))

	// 2. HTTPS without any client cert fails the handshake.
	noCertClient := httpsClientNoCert(t, caPEM)
	if _, err := noCertClient.Get(url); err == nil {
		t.Fatal("expected unauthenticated GET to fail, got nil error")
	} else {
		t.Logf("unauthenticated GET correctly rejected: %v", err)
	}

	// 3. The HTTP port is NOT listening.
	if portReachable("127.0.0.1", httpPort) {
		t.Errorf("HTTP port %d should be closed without --accept-http but TCP dial succeeded", httpPort)
	}
}

func TestNodeInfoHTTPSAcceptHTTP(t *testing.T) {
	dir := t.TempDir()
	caPEM, scPEM, skPEM, _, _ := generateTestPKI(t, "localhost")
	scPath := writePEM(t, dir, "server.crt", scPEM)
	skPath := writePEM(t, dir, "server.key", skPEM)

	httpPort := freePort(t)
	tlsPort := freePort(t)
	instance := fmt.Sprintf("tls-dual-%d", tlsPort)

	cleanup := startNodeInfo(t,
		instance,
		"--port", fmt.Sprintf("%d", httpPort),
		"--tls-port", fmt.Sprintf("%d", tlsPort),
		"--cert", scPath,
		"--key", skPath,
		"--accept-http",
	)
	t.Cleanup(cleanup)

	waitForPort(t, "127.0.0.1", httpPort, 10*time.Second)
	waitForPort(t, "127.0.0.1", tlsPort, 10*time.Second)

	// Plain HTTP still works on the legacy port.
	httpClient := &http.Client{Timeout: 5 * time.Second}
	httpURL := fmt.Sprintf("http://localhost:%d/v1/node-info", httpPort)
	resp, err := httpClient.Get(httpURL)
	if err != nil {
		t.Fatalf("HTTP GET %s: %v", httpURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP GET status = %d, want 200", resp.StatusCode)
	}

	// HTTPS works too — server-only TLS, no client cert needed.
	tlsClient := httpsClientNoCert(t, caPEM)
	tlsURL := fmt.Sprintf("https://localhost:%d/v1/node-info", tlsPort)
	resp, err = tlsClient.Get(tlsURL)
	if err != nil {
		t.Fatalf("HTTPS GET %s: %v", tlsURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTPS GET status = %d, want 200", resp.StatusCode)
	}
}

func TestNodeInfoPlainHTTPDefaultUnchanged(t *testing.T) {
	httpPort := freePort(t)
	instance := fmt.Sprintf("plain-%d", httpPort)

	cleanup := startNodeInfo(t,
		instance,
		"--port", fmt.Sprintf("%d", httpPort),
	)
	t.Cleanup(cleanup)

	waitForPort(t, "127.0.0.1", httpPort, 10*time.Second)

	httpClient := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://localhost:%d/v1/node-info", httpPort)
	resp, err := httpClient.Get(url)
	if err != nil {
		t.Fatalf("HTTP GET %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP GET status = %d, want 200", resp.StatusCode)
	}
}
