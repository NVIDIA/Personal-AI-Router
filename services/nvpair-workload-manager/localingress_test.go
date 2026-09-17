// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureRW is the manager's local interface in these tests: whatever the
// codec writes (the upward workloads:upsert / workloads:remove notifications)
// is captured; Read blocks forever, as an idle broker would.
type captureRW struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureRW) Read(p []byte) (int, error) { select {} }
func (c *captureRW) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}
func (c *captureRW) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func newTestManager(t *testing.T) (*Manager, *captureRW) {
	t.Helper()
	rw := &captureRW{}
	m, err := NewManager(NewCodec(rw), 0, "self-uuid", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.ctx, m.cancel = ctx, cancel
	return m, rw
}

func TestStampOriginFillsEmptyLifecycleOrigin(t *testing.T) {
	in := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"m","engine":"llamacpp","state":"running","createdAt":1}}`)
	out, err := stampOrigin(MethodStarted, in, "self-uuid")
	if err != nil {
		t.Fatal(err)
	}
	wl, err := parseLifecycle(out)
	if err != nil {
		t.Fatal(err)
	}
	if wl.OriginatedFrom != "self-uuid" {
		t.Fatalf("originatedFrom = %q, want self-uuid", wl.OriginatedFrom)
	}
	if wl.ID != "j1" || wl.Engine != "llamacpp" {
		t.Fatalf("other fields disturbed: %+v", wl)
	}
}

func TestStampOriginKeepsExplicitOrigin(t *testing.T) {
	in := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"m","engine":"llamacpp","state":"running","originatedFrom":"other","createdAt":1}}`)
	out, err := stampOrigin(MethodStarted, in, "self-uuid")
	if err != nil {
		t.Fatal(err)
	}
	wl, _ := parseLifecycle(out)
	if wl.OriginatedFrom != "other" {
		t.Fatalf("originatedFrom = %q, want other", wl.OriginatedFrom)
	}
}

func TestStampOriginRemove(t *testing.T) {
	out, err := stampOrigin(MethodRemove, json.RawMessage(`{"workloadId":"j1"}`), "self-uuid")
	if err != nil {
		t.Fatal(err)
	}
	id, node, err := parseRemove(out)
	if err != nil {
		t.Fatal(err)
	}
	if id != "j1" || node != "self-uuid" {
		t.Fatalf("remove = (%q, %q), want (j1, self-uuid)", id, node)
	}
}

func TestStampOriginRejectsMissingInfo(t *testing.T) {
	if _, err := stampOrigin(MethodStarted, json.RawMessage(`{}`), "self-uuid"); err == nil {
		t.Fatal("expected an error for a lifecycle frame without workloadInfo")
	}
}

func TestIngestLocalTracksAndEmitsUpsert(t *testing.T) {
	m, rw := newTestManager(t)
	params := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"qwen","engine":"llamacpp","runId":"j1","state":"running","originatedFrom":"self-uuid","createdAt":5,"startedAt":5,"completedAt":null,"error":null,"requesterId":null}}`)
	if err := m.ingestLocal(MethodStarted, params); err != nil {
		t.Fatal(err)
	}
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1", got)
	}
	out := rw.String()
	if !strings.Contains(out, `"method":"workloads:upsert"`) || !strings.Contains(out, `"id":"j1"`) {
		t.Fatalf("broker did not receive the upsert: %s", out)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"j1","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
	if out := rw.String(); !strings.Contains(out, `"method":"workloads:remove"`) {
		t.Fatalf("broker did not receive the remove: %s", out)
	}
}

func TestIngestLocalRejectsMalformed(t *testing.T) {
	m, rw := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, json.RawMessage(`{"workloadInfo":{"id":""}}`)); err == nil {
		t.Fatal("expected validation error")
	}
	if err := m.ingestLocal("bogus:method", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected unknown-method error")
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("malformed frames must not be tracked, got %d", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("malformed frames must not reach the broker: %s", out)
	}
}

func TestNewLocalIngressRefusesNonLoopback(t *testing.T) {
	noop := func(string, json.RawMessage) error { return nil }
	for _, addr := range []string{"0.0.0.0:14324", ":14324", "192.0.2.5:14324", "[::]:14324", "example.com:14324", "nonsense"} {
		if _, err := newLocalIngress(addr, "self", noop); err == nil {
			t.Errorf("%q accepted, want refusal", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0", "127.5.5.5:14324"} {
		if _, err := newLocalIngress(addr, "self", noop); err != nil {
			t.Errorf("%q refused: %v", addr, err)
		}
	}
}

func TestLocalIngressEndToEnd(t *testing.T) {
	m, rw := newTestManager(t)
	li, err := newLocalIngress("127.0.0.1:0", "self-uuid", m.ingestLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := li.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = li.Serve(ctx) }()
	url := "http://" + li.Addr() + eventsPath

	post := func(body string) int {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Origin left empty: stamped, tracked, emitted upward.
	if code := post(`{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"j9","model":"qwen","engine":"llamacpp","runId":"j9","state":"running","createdAt":1,"startedAt":1,"completedAt":null,"error":null,"requesterId":null}}}`); code != http.StatusOK {
		t.Fatalf("post = %d, want 200", code)
	}
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1", got)
	}
	if out := rw.String(); !strings.Contains(out, `"originatedFrom":"self-uuid"`) || !strings.Contains(out, `"method":"workloads:upsert"`) {
		t.Fatalf("upsert missing or origin not stamped: %s", out)
	}
	// Producer mistakes are 400s and never reach the broker or the wire.
	if code := post(`{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":""}}}`); code != http.StatusBadRequest {
		t.Fatalf("malformed = %d, want 400", code)
	}
	if code := post(`{"jsonrpc":"1.0","method":"workload:started","params":{}}`); code != http.StatusBadRequest {
		t.Fatalf("wrong version = %d, want 400", code)
	}
	if code := post(`{"jsonrpc":"2.0","method":"discovery:nodes","params":{}}`); code != http.StatusBadRequest {
		t.Fatalf("foreign method = %d, want 400", code)
	}
	if code := post(`not json`); code != http.StatusBadRequest {
		t.Fatalf("not json = %d, want 400", code)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", resp.StatusCode)
	}
	// Removal without an origin is stamped too and clears the re-sync set.
	if code := post(`{"jsonrpc":"2.0","method":"workloads:remove","params":{"workloadId":"j9"}}`); code != http.StatusOK {
		t.Fatalf("remove = %d, want 200", code)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
}

func TestResolveLocalIngressAddr(t *testing.T) {
	dir := t.TempDir()
	cluster := filepath.Join(dir, "cluster")
	if got := resolveLocalIngressAddr("127.0.0.1:1", cluster); got != "127.0.0.1:1" {
		t.Fatalf("flag not honoured: %q", got)
	}
	if got := resolveLocalIngressAddr("", cluster); got != "" {
		t.Fatalf("no file should mean off, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, localIngressConfigFile), []byte(`{"listen":" 127.0.0.1:14324 "}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveLocalIngressAddr("", cluster); got != "127.0.0.1:14324" {
		t.Fatalf("file not honoured: %q", got)
	}
	if got := resolveLocalIngressAddr("127.0.0.1:2", cluster); got != "127.0.0.1:2" {
		t.Fatalf("flag must win over the file: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, localIngressConfigFile), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveLocalIngressAddr("", cluster); got != "" {
		t.Fatalf("malformed file should mean off, got %q", got)
	}
}

func TestNewManagerRefusesNonLoopbackIngress(t *testing.T) {
	if _, err := NewManager(NewCodec(&captureRW{}), 0, "self", "", "0.0.0.0:14324"); err == nil {
		t.Fatal("a non-loopback ingress address must fail construction")
	}
}
