// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package splitlisten

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// newSplitter binds a loopback listener, wraps it in a Splitter, and runs a
// plain HTTP server on Plain() and a TLS HTTP server on TLS(). Each server
// returns a distinct body so a test can prove which personality handled a
// request. Returns the shared base address and a cleanup func.
func newSplitter(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := New(ln)

	plainSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "plain")
	})}
	tlsSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "tls")
	})}

	go func() { _ = plainSrv.Serve(s.Plain()) }()
	go func() { _ = tlsSrv.Serve(tls.NewListener(s.TLS(), testServerTLSConfig(t))) }()

	return s.Addr().String(), func() {
		_ = plainSrv.Close()
		_ = tlsSrv.Close()
		_ = s.Close()
	}
}

func TestPlainConnectionRoutesToPlainServer(t *testing.T) {
	addr, cleanup := newSplitter(t)
	defer cleanup()

	body := httpGet(t, "http://"+addr+"/", nil)
	if body != "plain" {
		t.Fatalf("plain request served by %q, want %q", body, "plain")
	}
}

func TestTLSConnectionRoutesToTLSServer(t *testing.T) {
	addr, cleanup := newSplitter(t)
	defer cleanup()

	body := httpGet(t, "https://"+addr+"/", &tls.Config{InsecureSkipVerify: true})
	if body != "tls" {
		t.Fatalf("TLS request served by %q, want %q", body, "tls")
	}
}

// TestMalformedConnectionDoesNotWedgeDispatch opens connections that never
// complete (a silent client that sends no byte, and one that connects then
// immediately closes) and confirms the dispatcher drops them without blocking:
// a normal plain request afterwards must still succeed.
func TestMalformedConnectionDoesNotWedgeDispatch(t *testing.T) {
	old := peekTimeout
	peekTimeout = 200 * time.Millisecond
	defer func() { peekTimeout = old }()

	addr, cleanup := newSplitter(t)
	defer cleanup()

	// Silent client: connect, send nothing, let the peek deadline fire.
	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial silent: %v", err)
	}
	// Immediate-close client: connect and close before sending a byte.
	closer, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial closer: %v", err)
	}
	_ = closer.Close()

	// A good request must still be served while the silent conn is pending and
	// after the closed one was dropped.
	if body := httpGet(t, "http://"+addr+"/", nil); body != "plain" {
		t.Fatalf("plain request after malformed conns served %q, want %q", body, "plain")
	}
	_ = silent.Close()
}

func TestCloseStopsAccepting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := New(ln)
	addr := s.Addr().String()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Sub-listener Accept must report closed rather than block forever.
	done := make(chan error, 1)
	go func() {
		_, aerr := s.Plain().Accept()
		done <- aerr
	}()
	select {
	case aerr := <-done:
		if aerr == nil {
			t.Fatal("Accept returned nil error after Close, want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return after Close")
	}

	// The base port is released, so a fresh dial fails.
	if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
		_ = c.Close()
		t.Fatal("dial succeeded after Close, want connection refused")
	}
}

// TestChanListenerCloseUnblocksInFlightPush confirms Close wakes a push blocked
// on the unbuffered handoff and closes that connection, and that a push after
// Close is closed rather than handed off.
func TestChanListenerCloseUnblocksInFlightPush(t *testing.T) {
	l := newChanListener(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})

	peer, queued := net.Pipe()
	defer peer.Close()

	pushed := make(chan struct{})
	go func() {
		l.push(queued)
		close(pushed)
	}()

	// Give push time to block on the unbuffered send (no Accept running).
	time.Sleep(50 * time.Millisecond)

	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("push did not return after Close")
	}

	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("in-flight connection read error = %v, want EOF", err)
	}

	// A push that starts after Close must be closed rather than handed off.
	peer2, queued2 := net.Pipe()
	defer peer2.Close()
	l.push(queued2)
	_ = peer2.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer2.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("post-Close connection read error = %v, want EOF", err)
	}
}

// httpGet issues a GET and returns the response body. A nil tlsCfg uses plain
// HTTP; a non-nil one dials HTTPS with that client config.
func httpGet(t *testing.T, url string, tlsCfg *tls.Config) string {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	if tlsCfg != nil {
		client.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	var lastErr error
	// The servers start in goroutines; retry briefly to avoid a startup race.
	for i := 0; i < 20; i++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	t.Fatalf("GET %s: %v", url, lastErr)
	return ""
}

// testServerTLSConfig returns a server tls.Config backed by a freshly-minted
// self-signed loopback certificate.
func testServerTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "splitlisten-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

// scriptedListener is a net.Listener whose Accept behavior is driven by acceptFn.
type scriptedListener struct {
	addr     net.Addr
	acceptFn func(call int) (net.Conn, error)
	mu       sync.Mutex
	calls    int
	closed   chan struct{}
	closeOnce sync.Once
}

func newScriptedListener(acceptFn func(call int) (net.Conn, error)) *scriptedListener {
	return &scriptedListener{
		addr:     &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
		acceptFn: acceptFn,
		closed:   make(chan struct{}),
	}
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
	}
	l.mu.Lock()
	l.calls++
	call := l.calls
	l.mu.Unlock()
	return l.acceptFn(call)
}

func (l *scriptedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *scriptedListener) Addr() net.Addr { return l.addr }

func (l *scriptedListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

type tempNetError struct{}

func (tempNetError) Error() string   { return "temporary accept error" }
func (tempNetError) Timeout() bool   { return false }
func (tempNetError) Temporary() bool { return true }

type fatalNetError struct{}

func (fatalNetError) Error() string   { return "fatal accept error" }
func (fatalNetError) Timeout() bool   { return false }
func (fatalNetError) Temporary() bool { return false }

// TestFatalAcceptClosesBase confirms a non-temporary Accept error closes the
// base listener (and sub-listeners) so the port cannot remain in zombie LISTEN.
func TestFatalAcceptClosesBase(t *testing.T) {
	base := newScriptedListener(func(call int) (net.Conn, error) {
		return nil, fatalNetError{}
	})
	s := New(base)

	deadline := time.After(2 * time.Second)
	for !base.isClosed() {
		select {
		case <-deadline:
			t.Fatal("base listener was not closed after fatal Accept")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Plain().Accept()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Plain Accept returned nil after fatal Accept, want error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Plain Accept did not return after fatal Accept")
	}
}

// TestTemporaryAcceptRetries confirms a temporary Accept error does not tear
// down the splitter: a later successful Accept is still dispatched.
func TestTemporaryAcceptRetries(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	var base *scriptedListener
	base = newScriptedListener(func(call int) (net.Conn, error) {
		switch call {
		case 1:
			return nil, tempNetError{}
		case 2:
			return server, nil
		default:
			<-base.closed
			return nil, net.ErrClosed
		}
	})
	s := New(base)
	defer s.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := s.Plain().Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	// First byte 'G' (GET) routes to plain.
	go func() {
		_, _ = client.Write([]byte("G"))
	}()

	select {
	case c := <-accepted:
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("plain Accept did not receive connection after temporary Accept error")
	}
	if base.isClosed() {
		t.Fatal("base listener closed after temporary Accept error")
	}
}

// TestPushTimeoutClosesConnWhenAcceptStops confirms that if a sub-listener is
// not Accepting, connections are closed after pushTimeout instead of pinning
// FDs forever, and the base Accept loop keeps draining.
func TestPushTimeoutClosesConnWhenAcceptStops(t *testing.T) {
	old := pushTimeout
	pushTimeout = 50 * time.Millisecond
	defer func() { pushTimeout = old }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := New(ln)
	defer s.Close()
	// Deliberately do not Accept on Plain() or TLS().

	addr := s.Addr().String()

	closed := make(chan struct{})
	go func() {
		c, derr := net.Dial("tcp", addr)
		if derr != nil {
			t.Errorf("dial: %v", derr)
			return
		}
		_, _ = c.Write([]byte("G"))
		buf := make([]byte, 1)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, rerr := c.Read(buf)
		_ = c.Close()
		if rerr == nil {
			t.Error("expected peer close after push timeout, got successful read")
		}
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not release connection after push timeout")
	}

	// Base Accept is still running: another dial should still complete TCP
	// handshake (not refuse), then be closed by the same timeout path.
	c2, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial after push timeout: %v", err)
	}
	_, _ = c2.Write([]byte("G"))
	_ = c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, rerr := c2.Read(buf)
	_ = c2.Close()
	if rerr == nil {
		t.Fatal("expected connection closed after push timeout")
	}
}

// TestChanListenerPushTimeoutWithoutAccept is the direct unit form of the
// timeout-until-Accept guarantee: with no Accept, push closes the conn within
// pushTimeout rather than parking it in a backlog forever.
func TestChanListenerPushTimeoutWithoutAccept(t *testing.T) {
	old := pushTimeout
	pushTimeout = 50 * time.Millisecond
	defer func() { pushTimeout = old }()

	l := newChanListener(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer l.Close()

	peer, queued := net.Pipe()
	defer peer.Close()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		l.push(queued)
		close(done)
	}()

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("read error = %v, want EOF after push timeout", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push did not return after timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("push took %v, want roughly pushTimeout (%v)", elapsed, pushTimeout)
	}
}
