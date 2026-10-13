// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/httpcon/testclient"
)

func TestProbeHTTPReusesConnections(t *testing.T) {
	test := func(name string, chunked bool) {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			client, connections := testclient.New(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(engineIdentityProbeHeader) != "1" {
					t.Error("missing engine identity header")
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
			ex := &Executor{client: client}
			probe := &Probe{HTTP: "http://127.0.0.1:{port}/api/version"}
			const rounds = 32
			for i := 0; i < rounds; i++ {
				if got, want := ex.probe(context.Background(), probe, 1), i%2 == 0; got != want {
					t.Fatalf("poll %d: healthy = %v, want %v", i, got, want)
				}
			}
			if got := connections.Count(); got != 1 {
				t.Fatalf("accepted %d HTTP/1 connections for %d polls, want 1", got, rounds)
			}
		})
	}

	test("content-length", false)
	test("chunked", true)
}

func TestProbeHTTPMatchesJSONIdentity(t *testing.T) {
	test := func(name, body string, want bool) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			client, _ := testclient.New(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			ex := &Executor{client: client}
			probe := &Probe{
				HTTP:      "http://127.0.0.1:{port}/props",
				JSONMatch: &ProbeJSONMatch{Field: "service.role", Value: "router"},
			}
			if got := ex.probe(context.Background(), probe, 1); got != want {
				t.Fatalf("probe = %v, want %v", got, want)
			}
		})
	}

	test("matching nested string", `{"service":{"role":"router"}}`, true)
	test("different string", `{"service":{"role":"worker"}}`, false)
	test("missing field", `{"service":{}}`, false)
	test("wrong field type", `{"service":{"role":true}}`, false)
	test("malformed JSON", `{"service":`, false)
}

func TestProbeHTTPBoundsBodyDrain(t *testing.T) {
	body := &healthProbeBody{reader: strings.NewReader(strings.Repeat("x", 4<<20))}
	ex := &Executor{client: &http.Client{Transport: healthProbeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusAccepted, Body: body, Header: make(http.Header)}, nil
	})}}
	probe := &Probe{HTTP: "http://127.0.0.1:{port}/", Status: http.StatusAccepted}
	if !ex.probe(context.Background(), probe, 1) {
		t.Fatal("body drain changed the configured HTTP status health result")
	}
	if body.read == 0 || body.read > 1<<20 || !body.closed {
		t.Fatalf("body read = %d, closed = %v; want bounded drain and close", body.read, body.closed)
	}
}

func TestProbeHTTPRejectsOversizedJSONIdentityBody(t *testing.T) {
	body := &healthProbeBody{reader: strings.NewReader(strings.Repeat("x", 4<<20))}
	ex := &Executor{client: &http.Client{Transport: healthProbeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}}
	probe := &Probe{
		HTTP:      "http://127.0.0.1:{port}/",
		JSONMatch: &ProbeJSONMatch{Field: "role", Value: "router"},
	}
	if ex.probe(context.Background(), probe, 1) {
		t.Fatal("oversized JSON identity body passed the probe")
	}
	if body.read == 0 || body.read > 2*maxProbeJSONBytes+1 || !body.closed {
		t.Fatalf("body read = %d, closed = %v; want bounded read and close", body.read, body.closed)
	}
}

func TestProbeHTTPBodyDrainHonorsDeadline(t *testing.T) {
	var body *healthProbeBody
	ex := &Executor{client: &http.Client{Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
		body = &healthProbeBody{ctx: req.Context()}
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}}
	probe := &Probe{HTTP: "http://127.0.0.1:{port}/"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if !ex.probe(ctx, probe, 1) {
		t.Fatal("body drain changed the HTTP status health result")
	}
	if !body.attempted || !body.closed || time.Since(start) > time.Second {
		t.Fatalf("stalled body did not drain and close within deadline: %+v", body)
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
