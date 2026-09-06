// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var healthProbeChecks = []struct {
	name    string
	path    string
	profile engineProxyProfile
}{
	{"ollama", "/", ollamaProxyProfile},
	{"lmstudio", "/v1/models", lmstudioProxyProfile},
}

func TestHealthChecksReuseConnections(t *testing.T) {
	for _, check := range healthProbeChecks {
		for _, chunked := range []bool{false, true} {
			framing := "content-length"
			if chunked {
				framing = "chunked"
			}
			t.Run(check.name+"/"+framing, func(t *testing.T) {
				var requests atomic.Int32
				client, connections := healthProbePipeClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != check.path {
						t.Errorf("path = %q, want %q", r.URL.Path, check.path)
					}
					status := http.StatusOK
					if requests.Add(1)%2 == 0 {
						status = http.StatusServiceUnavailable
					}
					w.WriteHeader(status)
					if chunked {
						_ = http.NewResponseController(w).Flush()
					}
					_, _ = io.WriteString(w, `{"models":[],"status":"responding"}`)
				}))
				const rounds = 32
				for i := 0; i < rounds; i++ {
					if got, want := checkEngineHealth(check.profile, client, 1), i%2 == 0; got != want {
						t.Fatalf("poll %d: healthy = %v, want %v", i, got, want)
					}
				}
				if got := connections.Load(); got != 1 {
					t.Fatalf("accepted %d HTTP/1 connections for %d polls, want 1", got, rounds)
				}
			})
		}
	}
}

func TestHealthChecksBoundBodyDrain(t *testing.T) {
	for _, check := range healthProbeChecks {
		t.Run(check.name, func(t *testing.T) {
			body := &healthProbeBody{reader: strings.NewReader(strings.Repeat("x", 4<<20))}
			client := &http.Client{Transport: healthProbeTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
			})}
			if !checkEngineHealth(check.profile, client, 1) {
				t.Fatal("body drain changed the HTTP status health result")
			}
			if body.read == 0 || body.read > 1<<20 || !body.closed {
				t.Fatalf("body read = %d, closed = %v; want bounded drain and close", body.read, body.closed)
			}
		})
	}
}

func TestHealthChecksBodyDrainHonorsTimeout(t *testing.T) {
	for _, check := range healthProbeChecks {
		t.Run(check.name, func(t *testing.T) {
			var body *healthProbeBody
			client := &http.Client{Timeout: 50 * time.Millisecond, Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
				body = &healthProbeBody{ctx: req.Context()}
				return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
			})}
			start := time.Now()
			if !checkEngineHealth(check.profile, client, 1) {
				t.Fatal("body drain changed the HTTP status health result")
			}
			if !body.attempted || !body.closed || time.Since(start) > time.Second {
				t.Fatalf("stalled body did not drain and close within timeout: %+v", body)
			}
		})
	}
}

type healthProbeTransport func(*http.Request) (*http.Response, error)

func (f healthProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type healthProbeBody struct {
	reader    io.Reader
	ctx       context.Context
	read      int
	attempted bool
	closed    bool
}

func (b *healthProbeBody) Read(p []byte) (int, error) {
	b.attempted = true
	if b.ctx != nil {
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *healthProbeBody) Close() error { b.closed = true; return nil }

// Exercise the real HTTP/1 transport over net.Pipe so this regression can run
// even on a machine whose TCP source ports have already been exhausted.
func healthProbePipeClient(t *testing.T, handler http.Handler) (*http.Client, *atomic.Int32) {
	t.Helper()
	listener := &healthProbeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	connections := &atomic.Int32{}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		select {
		case listener.conns <- serverConn:
			connections.Add(1)
			return clientConn, nil
		case <-ctx.Done():
			_ = clientConn.Close()
			_ = serverConn.Close()
			return nil, ctx.Err()
		}
	}}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return client, connections
}

type healthProbeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *healthProbeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *healthProbeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *healthProbeListener) Addr() net.Addr { return &net.TCPAddr{Port: 1} }
