// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureRW is the manager's local interface in these tests: whatever the
// codec writes (the upward workloads:upsert / workloads:remove notifications)
// is captured; Read blocks forever, as an idle broker would. While fail is set,
// writes error out as they do once the broker has gone away.
type captureRW struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	fail bool
}

func (c *captureRW) Read(p []byte) (int, error) { select {} }
func (c *captureRW) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return 0, errors.New("broker pipe closed")
	}
	return c.buf.Write(p)
}
func (c *captureRW) setFail(fail bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = fail
}
func (c *captureRW) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func newTestManager(t *testing.T) (*Manager, *captureRW) {
	t.Helper()
	rw := &captureRW{}
	m := NewManager(NewCodec(rw), 0, "self-uuid", "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.ctx, m.cancel = ctx, cancel
	return m, rw
}

func TestNormalizeIngressParamsFillsEmptyLifecycleOrigin(t *testing.T) {
	in := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"m","engine":"llamacpp","state":"running","createdAt":1}}`)
	out, err := normalizeIngressParams(MethodStarted, in, "self-uuid")
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

func TestNormalizeIngressParamsAcceptsOwnOrigin(t *testing.T) {
	in := json.RawMessage(`{"workloadInfo":{"id":"j1","model":"m","engine":"llamacpp","state":"running","originatedFrom":"self-uuid","createdAt":1}}`)
	out, err := normalizeIngressParams(MethodStarted, in, "self-uuid")
	if err != nil {
		t.Fatal(err)
	}
	wl, _ := parseLifecycle(out)
	if wl.OriginatedFrom != "self-uuid" {
		t.Fatalf("originatedFrom = %q, want self-uuid", wl.OriginatedFrom)
	}
}

func TestNormalizeIngressParamsRejectsForeignOrigin(t *testing.T) {
	for name, tc := range map[string]struct{ method, params string }{
		"lifecycle":            {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","originatedFrom":"other-node"}}`},
		"lifecycle non-string": {MethodStarted, `{"workloadInfo":{"id":"j1","model":"m","engine":"e","state":"running","originatedFrom":7}}`},
		"remove":               {MethodRemove, `{"workloadId":"j1","originatedFrom":"other-node"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeIngressParams(tc.method, json.RawMessage(tc.params), "self-uuid"); !errors.Is(err, errIngressOrigin) {
				t.Fatalf("err = %v, want errIngressOrigin", err)
			}
		})
	}
}

func TestNormalizeIngressParamsRemove(t *testing.T) {
	out, err := normalizeIngressParams(MethodRemove, json.RawMessage(`{"workloadId":"j1"}`), "self-uuid")
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

func TestNormalizeIngressParamsRejectsMissingInfo(t *testing.T) {
	if _, err := normalizeIngressParams(MethodStarted, json.RawMessage(`{}`), "self-uuid"); err == nil {
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
		if _, err := newLocalIngress(addr, noop); err == nil {
			t.Errorf("%q accepted, want refusal", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0", "127.5.5.5:14324"} {
		if _, err := newLocalIngress(addr, noop); err != nil {
			t.Errorf("%q refused: %v", addr, err)
		}
	}
}

func TestLocalIngressEndToEnd(t *testing.T) {
	m, rw := newTestManager(t)
	li, err := newLocalIngress("127.0.0.1:0", m.ingestLocal)
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

func TestEnableLocalIngressRefusesNonLoopback(t *testing.T) {
	m := NewManager(NewCodec(&captureRW{}), 0, "self", "")
	if err := m.EnableLocalIngress("0.0.0.0:14324"); err == nil {
		t.Fatal("a non-loopback ingress address must be refused")
	}
	if m.ingress != nil {
		t.Fatal("a refused address must not install the ingress")
	}
}

// ingestFrame is the params of a running-state lifecycle frame for workload id
// at producer event counter seq.
func ingestFrame(id string, seq int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"workloadInfo":{"id":%q,"model":"qwen","engine":"llamacpp","runId":%q,"state":"running","originatedFrom":"self-uuid","createdAt":1,"seq":%d}}`, id, id, seq))
}

func TestLocalIngressBrokerDownIs500(t *testing.T) {
	m, rw := newTestManager(t)
	rw.setFail(true)
	li, err := newLocalIngress("127.0.0.1:0", m.ingestLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := li.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = li.Serve(ctx) }()
	body := `{"jsonrpc":"2.0","method":"workload:started","params":` + string(ingestFrame("j1", 1)) + `}`
	resp, err := http.Post("http://"+li.Addr()+eventsPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("post with the broker down = %d, want 500", resp.StatusCode)
	}
}

// frameSeqs returns workloadInfo.seq of each JSON-RPC frame, in order.
func frameSeqs(t *testing.T, frames []string) []int {
	t.Helper()
	seqs := make([]int, 0, len(frames))
	for _, raw := range frames {
		var frame struct {
			Params struct {
				WorkloadInfo struct {
					Seq int `json:"seq"`
				} `json:"workloadInfo"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("decode frame %q: %v", raw, err)
		}
		seqs = append(seqs, frame.Params.WorkloadInfo.Seq)
	}
	return seqs
}

// queuedFrames drains the peer broadcast queue, in queue order.
func queuedFrames(m *Manager) []string {
	var out []string
	for len(m.broadcastCh) > 0 {
		out = append(out, string(<-m.broadcastCh))
	}
	return out
}

func TestIngestLocalBrokerWriteFailureLeavesNoState(t *testing.T) {
	m, rw := newTestManager(t)
	rw.setFail(true)
	err := m.ingestLocal(MethodStarted, ingestFrame("j1", 1))
	if !errors.Is(err, errBrokerWrite) {
		t.Fatalf("err = %v, want errBrokerWrite", err)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if got := len(m.broadcastCh); got != 0 {
		t.Fatalf("peer queue = %d frames, want 0", got)
	}
}

func TestIngestLocalFailedRemoveKeepsWorkloadTracked(t *testing.T) {
	m, rw := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, ingestFrame("j1", 1)); err != nil {
		t.Fatal(err)
	}
	queuedFrames(m)
	rw.setFail(true)
	err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"j1","originatedFrom":"self-uuid"}`))
	if !errors.Is(err, errBrokerWrite) {
		t.Fatalf("err = %v, want errBrokerWrite", err)
	}
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1: the broker still holds the workload", got)
	}
	if got := len(m.broadcastCh); got != 0 {
		t.Fatalf("peer queue = %d frames, want 0: peers must not see a removal the broker never got", got)
	}
}

func TestIngestLocalConcurrentPostsKeepBrokerAndPeerOrder(t *testing.T) {
	m, rw := newTestManager(t)
	const posts = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for seq := 1; seq <= posts; seq++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			<-start
			if err := m.ingestLocal(MethodStarted, ingestFrame("j1", seq)); err != nil {
				t.Errorf("ingest seq %d: %v", seq, err)
			}
		}(seq)
	}
	close(start)
	wg.Wait()

	brokerSeqs := frameSeqs(t, strings.Split(strings.TrimSpace(rw.String()), "\n"))
	peerSeqs := frameSeqs(t, queuedFrames(m))
	if len(brokerSeqs) != posts || len(peerSeqs) != posts {
		t.Fatalf("broker got %d frames and peers %d, want %d each", len(brokerSeqs), len(peerSeqs), posts)
	}
	for i := range brokerSeqs {
		if brokerSeqs[i] != peerSeqs[i] {
			t.Fatalf("frame %d: broker saw seq %d but peers were queued seq %d", i, brokerSeqs[i], peerSeqs[i])
		}
	}
}

// TestIngestLocalSnapshotCannotResurrectRemovedWorkload is the ingress twin of
// TestManager_SnapshotCannotResurrectRemovedWorkload: a removal ingested while
// a re-sync snapshot is in flight must not be followed by that snapshot's
// upsert on the peer queue.
func TestIngestLocalSnapshotCannotResurrectRemovedWorkload(t *testing.T) {
	m := &Manager{
		codec:       NewCodec(codecNop{}),
		activeLocal: make(map[workloadKey]workloadEvent),
		broadcastCh: make(chan []byte, 4),
		peers:       newPeerSet(0),
		selfUUID:    "self-uuid",
	}
	if err := m.ingestLocal(MethodStarted, ingestFrame("j1", 1)); err != nil {
		t.Fatal(err)
	}
	queuedFrames(m)

	paused := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSnapshot := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSnapshot()
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(&snapshotPauseHandler{paused: paused, release: release}))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	snapshotDone := make(chan struct{})
	go func() {
		m.broadcastSnapshot("snapshot copied before removal")
		close(snapshotDone)
	}()
	select {
	case <-paused:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not pause after copying the workload")
	}

	removeDone := make(chan error, 1)
	go func() {
		removeDone <- m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"j1","originatedFrom":"self-uuid"}`))
	}()
	// The removal is either queued while the snapshot is paused or held back
	// until it resumes; both orders are fine as long as the removal is last.
	time.Sleep(100 * time.Millisecond)
	releaseSnapshot()
	select {
	case err := <-removeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("removal did not finish")
	}
	select {
	case <-snapshotDone:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not finish")
	}

	frames := queuedFrames(m)
	if len(frames) == 0 {
		t.Fatal("no frames queued, want at least the removal")
	}
	for i, raw := range frames {
		var frame Message
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			t.Fatalf("decode queued frame: %v", err)
		}
		if last := i == len(frames)-1; last != (frame.Method == MethodRemove) {
			t.Fatalf("frame %d of %d is %s: the removal must be the last frame queued", i, len(frames), frame.Method)
		}
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
}

// startIngress serves a loopback ingress over m and returns its events URL.
func startIngress(t *testing.T, m *Manager) (*localIngress, string) {
	t.Helper()
	li, err := newLocalIngress("127.0.0.1:0", m.ingestLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := li.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = li.Serve(ctx) }()
	return li, "http://" + li.Addr() + eventsPath
}

// postIngress POSTs body as application/json, after mutate has adjusted the
// request, and returns the status code.
func postIngress(t *testing.T, url, body string, mutate func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func startedBody(id string) string {
	return `{"jsonrpc":"2.0","method":"workload:started","params":` + string(ingestFrame(id, 1)) + `}`
}

func TestLocalIngressRejectsNonJSONContentType(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data", ""} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) {
			if ct == "" {
				r.Header.Del("Content-Type")
			} else {
				r.Header.Set("Content-Type", ct)
			}
		})
		if code != http.StatusUnsupportedMediaType {
			t.Errorf("content type %q = %d, want 415", ct, code)
		}
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a rejected request reached the broker: %s", out)
	}
}

func TestLocalIngressAcceptsJSONContentTypeWithCharset(t *testing.T) {
	m, _ := newTestManager(t)
	_, url := startIngress(t, m)
	code := postIngress(t, url, startedBody("j1"), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	})
	if code != http.StatusOK {
		t.Fatalf("application/json with a charset = %d, want 200", code)
	}
}

func TestLocalIngressRejectsOriginHeader(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:14324", "null"} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) { r.Header.Set("Origin", origin) })
		if code != http.StatusForbidden {
			t.Errorf("Origin %q = %d, want 403", origin, code)
		}
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a rejected request reached the broker: %s", out)
	}
}

func TestLocalIngressRejectsNonLoopbackHost(t *testing.T) {
	m, rw := newTestManager(t)
	li, url := startIngress(t, m)
	_, port, _ := net.SplitHostPort(li.Addr())
	for _, host := range []string{"evil.example:" + port, "evil.example", "127.0.0.1", "127.0.0.1:1", "192.0.2.5:" + port, "localhost.evil.example:" + port} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) { r.Host = host })
		if code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q = %d, want 421", host, code)
		}
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a rejected request reached the broker: %s", out)
	}
}

func TestLocalIngressAcceptsLoopbackHostNames(t *testing.T) {
	m, _ := newTestManager(t)
	li, url := startIngress(t, m)
	_, port, _ := net.SplitHostPort(li.Addr())
	for _, host := range []string{"localhost:" + port, "LOCALHOST:" + port, "127.0.0.1:" + port, "127.5.5.5:" + port, "[::1]:" + port} {
		code := postIngress(t, url, startedBody("j1"), func(r *http.Request) { r.Host = host })
		if code != http.StatusOK {
			t.Errorf("Host %q = %d, want 200", host, code)
		}
	}
}

// failReader fails the test if the handler reads the body.
type failReader struct{ t *testing.T }

func (r failReader) Read([]byte) (int, error) {
	r.t.Error("the request body was read before the header checks")
	return 0, io.EOF
}

func TestLocalIngressHeaderChecksPrecedeBodyRead(t *testing.T) {
	li, err := newLocalIngress("127.0.0.1:14324", func(string, json.RawMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*http.Request){
		"host":         func(r *http.Request) { r.Host = "evil.example" },
		"origin":       func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"content type": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, eventsPath, failReader{t})
			req.Host = "127.0.0.1:14324"
			req.Header.Set("Content-Type", "application/json")
			mutate(req)
			rec := httptest.NewRecorder()
			li.handle(rec, req)
			if rec.Code < 400 {
				t.Fatalf("status = %d, want a rejection", rec.Code)
			}
		})
	}
}

// A header check refuses a request before its body is read, so the body it
// declares may never arrive. The answer must not wait for it: net/http drains an
// unread body on a keep-alive connection before it replies, which would hold the
// 403 back for the whole read timeout if the rejection did not close the
// connection.
func TestLocalIngressRejectionDoesNotWaitForTheBody(t *testing.T) {
	m, _ := newTestManager(t)
	li, _ := startIngress(t, m)
	conn, err := net.Dial("tcp", li.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// The headers declare a body of 64 bytes, and it is never sent.
	request := strings.Join([]string{
		"POST " + eventsPath + " HTTP/1.1",
		"Host: " + li.Addr(),
		"Origin: https://evil.example",
		"Content-Type: application/json",
		"Content-Length: 64",
		"", "",
	}, "\r\n")
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer within a second to a rejected request whose body was never sent: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLocalIngressRejectsBodyOverCap(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	// A valid frame padded with whitespace past the cap: truncating the read
	// would still parse it, so only a real limit rejects it.
	padded := startedBody("j1") + strings.Repeat(" ", localIngressBodyCap)
	if code := postIngress(t, url, padded, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body = %d, want 413", code)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("an oversize body reached the broker: %s", out)
	}
}

func TestLocalIngressServerHasTimeouts(t *testing.T) {
	li, err := newLocalIngress("127.0.0.1:0", func(string, json.RawMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	srv := li.newServer()
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("ingress server timeouts unset: %+v", srv)
	}
}

// net/http's WriteTimeout runs from the end of the request headers and covers
// the whole handler, so it would cut off a request that is only waiting on a
// slow broker write while the frame still lands, and the producer would retry.
func TestLocalIngressServerHasNoWriteTimeout(t *testing.T) {
	li, err := newLocalIngress("127.0.0.1:0", func(string, json.RawMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := li.newServer().WriteTimeout; got != 0 {
		t.Fatalf("WriteTimeout = %v, want none", got)
	}
}

func TestLocalIngressStopsWhenContextCancelled(t *testing.T) {
	m, _ := newTestManager(t)
	li, err := newLocalIngress("127.0.0.1:0", m.ingestLocal)
	if err != nil {
		t.Fatal(err)
	}
	if err := li.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- li.Serve(ctx) }()
	resp, err := http.Get("http://" + li.Addr() + eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
	// A request, not a bare dial: on some hosts a connect to a port nobody
	// listens on can complete, but nothing answers it.
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := client.Post("http://"+li.Addr()+eventsPath, "application/json", strings.NewReader(startedBody("W"))); err == nil {
		resp.Body.Close()
		t.Fatalf("the ingress still answers requests after shutdown: %s", resp.Status)
	}
}

func TestRunReturnsIngressListenError(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	m := NewManager(NewCodec(&captureRW{}), 0, "self-uuid", t.TempDir())
	if err := m.EnableLocalIngress(taken.Addr().String()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "local ingress listen") {
			t.Fatalf("Run = %v, want the ingress listen error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not fail on an ingress port that is already in use")
	}
}

func TestIngestLocalRejectsForeignOrigin(t *testing.T) {
	m, rw := newTestManager(t)
	foreign := json.RawMessage(`{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"other-node","createdAt":1}}`)
	if err := m.ingestLocal(MethodStarted, foreign); !errors.Is(err, errIngressOrigin) {
		t.Fatalf("lifecycle err = %v, want errIngressOrigin", err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"other-node"}`)); !errors.Is(err, errIngressOrigin) {
		t.Fatalf("remove err = %v, want errIngressOrigin", err)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if got := len(m.broadcastCh); got != 0 {
		t.Fatalf("peer queue = %d frames, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a foreign-origin frame reached the broker: %s", out)
	}
}

// A workload reported without an origin is stamped, so the producer's removal
// that also omits it matches and clears it.
func TestIngestLocalOmittedOriginRoundTrips(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, json.RawMessage(`{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1}}`)); err != nil {
		t.Fatal(err)
	}
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1", got)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W"}`)); err != nil {
		t.Fatal(err)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set after remove = %d, want 0", got)
	}
}

func TestLocalIngressForeignOriginIs400(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	upsert := `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"other-node","createdAt":1}}}`
	if code := postIngress(t, url, upsert, nil); code != http.StatusBadRequest {
		t.Fatalf("foreign-origin upsert = %d, want 400", code)
	}
	remove := `{"jsonrpc":"2.0","method":"workloads:remove","params":{"workloadId":"W","originatedFrom":"other-node"}}`
	if code := postIngress(t, url, remove, nil); code != http.StatusBadRequest {
		t.Fatalf("foreign-origin remove = %d, want 400", code)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("a foreign-origin frame reached the broker: %s", out)
	}
}

func TestIngestLocalRejectsUnknownState(t *testing.T) {
	m, rw := newTestManager(t)
	for _, state := range []string{"done", "complete", "Running", "bogus"} {
		params := json.RawMessage(fmt.Sprintf(`{"workloadInfo":{"id":"W","model":"m","engine":"e","state":%q,"originatedFrom":"self-uuid","createdAt":1}}`, state))
		if err := m.ingestLocal(MethodStarted, params); err == nil {
			t.Errorf("state %q accepted, want an error", state)
		}
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if out := rw.String(); out != "" {
		t.Fatalf("an unknown state reached the broker: %s", out)
	}
}

func TestIngestLocalAcceptsEveryKnownState(t *testing.T) {
	m, _ := newTestManager(t)
	for _, state := range []WorkloadState{StateQueued, StateRunning, StateCompleted, StateFailed, StateCancelled} {
		params := json.RawMessage(fmt.Sprintf(`{"workloadInfo":{"id":%q,"model":"m","engine":"e","state":%q,"originatedFrom":"self-uuid","createdAt":1}}`, string(state), string(state)))
		if err := m.ingestLocal(MethodStarted, params); err != nil {
			t.Errorf("state %q refused: %v", state, err)
		}
	}
}

func TestLocalIngressUnknownStateIs400(t *testing.T) {
	m, _ := newTestManager(t)
	_, url := startIngress(t, m)
	body := `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"done","createdAt":1}}}`
	if code := postIngress(t, url, body, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown state = %d, want 400", code)
	}
}

// The stdio path is not the ingress's business: a state the ingress refuses is
// still tracked when the broker sends it.
func TestStdioLifecycleKeepsAcceptingAnyState(t *testing.T) {
	m, _ := newTestManager(t)
	m.handleMessage(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: json.RawMessage(`{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"paused","originatedFrom":"self-uuid","createdAt":1}}`)})
	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1", got)
	}
}

func TestIngestLocalRejectsResyncKey(t *testing.T) {
	info := `"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1}`
	for _, key := range []string{"resync", "Resync", "RESYNC", "reSync", `reſync`} {
		for name, tc := range map[string]struct{ method, params string }{
			"lifecycle": {MethodStarted, `{"` + key + `":true,` + info + `}`},
			"remove":    {MethodRemove, `{"` + key + `":true,"workloadId":"W","originatedFrom":"self-uuid"}`},
		} {
			t.Run(name+"/"+key, func(t *testing.T) {
				m, rw := newTestManager(t)
				if err := m.ingestLocal(tc.method, json.RawMessage(tc.params)); !errors.Is(err, errIngressResync) {
					t.Fatalf("err = %v, want errIngressResync", err)
				}
				assertIngressUntouched(t, m, rw)
			})
		}
	}
}

func TestIngestLocalTrackedFrameCarriesNoResyncFlag(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, ingestFrame("W", 1)); err != nil {
		t.Fatal(err)
	}
	snapshot := m.activeSnapshot()
	if len(snapshot) != 1 || isResyncFrame(snapshot[0].params) {
		t.Fatalf("the re-sync set holds a re-sync tagged frame: %+v", snapshot)
	}
}

// assertIngressUntouched fails when a rejected frame left any trace: a broker
// write, a queued peer frame, a re-sync entry or a removal tombstone.
func assertIngressUntouched(t *testing.T, m *Manager, rw *captureRW) {
	t.Helper()
	if out := rw.String(); out != "" {
		t.Fatalf("a rejected frame reached the broker: %s", out)
	}
	if got := len(m.broadcastCh); got != 0 {
		t.Fatalf("peer queue = %d frames, want 0", got)
	}
	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0", got)
	}
	if got := len(m.removedIngress); got != 0 {
		t.Fatalf("removal memory = %d, want 0", got)
	}
}

// stdioReplay is the broker replaying a stored lifecycle event to the worker.
func stdioReplay(m *Manager, method, id string, seq int) {
	m.handleMessage(&Message{JSONRPC: "2.0", Method: method, Params: ingestFrame(id, seq)})
}

// TestIngressRemovalBlocksStaleStdioReplay: the broker replays its store to a
// restarted worker, and the replay can land after the producer's removal.
func TestIngressRemovalBlocksStaleStdioReplay(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, ingestFrame("W", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	queuedFrames(m)

	stdioReplay(m, MethodStarted, "W", 1)
	m.broadcastSnapshot("heartbeat")

	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0: the removed workload was tracked again", got)
	}
	if frames := queuedFrames(m); len(frames) != 0 {
		t.Fatalf("peer queue = %v, want empty: the stale replay was re-broadcast", frames)
	}
}

func TestIngressLifecycleClearsRemovalTombstone(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, ingestFrame("W", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.ingestLocal(MethodStarted, ingestFrame("W", 2)); err != nil {
		t.Fatal(err)
	}
	queuedFrames(m)

	stdioReplay(m, MethodCompleted, "W", 3)

	if frames := queuedFrames(m); len(frames) != 1 {
		t.Fatalf("peer queue = %d frames, want the broker's next event for the re-reported workload", len(frames))
	}
}

func TestRemovalTombstoneExpires(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, ingestFrame("W", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	for el := m.removedOrder.Front(); el != nil; el = el.Next() {
		entry := el.Value.(removedEntry)
		entry.expiresAt = time.Now().Add(-time.Second)
		el.Value = entry
	}
	queuedFrames(m)

	stdioReplay(m, MethodStarted, "W", 2)

	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1: an expired tombstone must not block", got)
	}
	if len(m.removedIngress) != 0 {
		t.Fatalf("expired tombstone kept: %v", m.removedIngress)
	}
}

// TestIngressRemovalBeforeReplayOnFreshWorker is the restart case itself: the
// worker has just started, so nothing is tracked when the producer's removal
// arrives, and the broker's replay of the workload follows it.
func TestIngressRemovalBeforeReplayOnFreshWorker(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	queuedFrames(m)

	stdioReplay(m, MethodStarted, "W", 1)
	m.broadcastSnapshot("heartbeat")

	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0: the removed workload was tracked again", got)
	}
	if frames := queuedFrames(m); len(frames) != 0 {
		t.Fatalf("peer queue = %v, want empty: the stale replay was re-broadcast", frames)
	}
}

// The tombstone is the (origin, id) pair a removal carries, so a workload with
// another id is unaffected.
func TestRemovalTombstoneCoversOnlyTheRemovedPair(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	queuedFrames(m)

	stdioReplay(m, MethodStarted, "V", 1)

	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1: a different workload was blocked", got)
	}
}

func TestStdioRemovalLeavesNoTombstone(t *testing.T) {
	m, _ := newTestManager(t)
	stdioReplay(m, MethodStarted, "W", 1)
	m.handleMessage(&Message{JSONRPC: "2.0", Method: MethodRemove, Params: json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid"}`)})
	queuedFrames(m)

	stdioReplay(m, MethodStarted, "W", 2)

	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1: only ingress removals are tombstoned", got)
	}
}

func TestRemovalTombstoneDoesNotBlockResyncTaggedFrame(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, ingestFrame("W", 1)); err != nil {
		t.Fatal(err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid"}`)); err != nil {
		t.Fatal(err)
	}
	queuedFrames(m)

	m.handleMessage(&Message{JSONRPC: "2.0", Method: MethodStarted, Params: json.RawMessage(`{"resync":true,"workloadInfo":{"id":"W","model":"qwen","engine":"llamacpp","runId":"W","state":"running","originatedFrom":"self-uuid","createdAt":1}}`)})

	if got := len(m.activeSnapshot()); got != 1 {
		t.Fatalf("re-sync set = %d, want 1: an explicit re-assertion must not be treated as a replay", got)
	}
}

// The frames a producer could use to name another node as the origin, or hide
// the resync flag, by spelling a field name differently from the canonical one.
// encoding/json decodes each of these into the field.
var foldedKeyFrames = map[string]struct {
	method, params string
	want           error
}{
	"workloadInfo.originatedfrom beside an empty canonical key": {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"","originatedfrom":"victim","createdAt":1}}`, errIngressKey},
	"workloadInfo.originatedfrom alone":                         {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedfrom":"victim","createdAt":1}}`, errIngressKey},
	"workloadInfo.originatedfRom":                               {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedfRom":"victim","createdAt":1}}`, errIngressKey},
	"workloadInfo.OriginatedFrom":                               {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","OriginatedFrom":"victim","createdAt":1}}`, errIngressKey},
	"workloadInfo.ORIGINATEDFROM":                               {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","ORIGINATEDFROM":"victim","createdAt":1}}`, errIngressKey},
	"workloadInfo.ID":                                           {MethodStarted, `{"workloadInfo":{"ID":"W","model":"m","engine":"e","state":"running","createdAt":1}}`, errIngressKey},
	"workloadInfo long-s state":                                 {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","ſtate":"running","createdAt":1}}`, errIngressKey},
	"second top-level workloadinfo":                             {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1},"workloadinfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"victim","createdAt":1}}`, errIngressKey},
	"top-level WorkloadInfo alone":                              {MethodStarted, `{"WorkloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1}}`, errIngressKey},
	"two unknown keys that fold together":                       {MethodStarted, `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","createdAt":1,"note":1,"NOTE":2}}`, errIngressKey},
	"remove originatedfrom":                                     {MethodRemove, `{"workloadId":"W","originatedfrom":"victim"}`, errIngressKey},
	"remove originatedfrom beside the canonical key":            {MethodRemove, `{"workloadId":"W","originatedFrom":"self-uuid","originatedfrom":"victim"}`, errIngressKey},
	"remove WorkloadID":                                         {MethodRemove, `{"WorkloadID":"W"}`, errIngressKey},
	"top-level Resync":                                          {MethodStarted, `{"Resync":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1}}`, errIngressResync},
	"top-level long-s resync":                                   {MethodStarted, `{"reſync":true,"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","createdAt":1}}`, errIngressResync},
	"remove RESYNC":                                             {MethodRemove, `{"RESYNC":true,"workloadId":"W","originatedFrom":"self-uuid"}`, errIngressResync},
}

func TestIngestLocalRejectsFoldedFieldNames(t *testing.T) {
	for name, tc := range foldedKeyFrames {
		t.Run(name, func(t *testing.T) {
			m, rw := newTestManager(t)
			if err := m.ingestLocal(tc.method, json.RawMessage(tc.params)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			assertIngressUntouched(t, m, rw)
		})
	}
}

func TestLocalIngressFoldedFieldNamesAre400(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	for name, tc := range foldedKeyFrames {
		body := `{"jsonrpc":"2.0","method":"` + tc.method + `","params":` + tc.params + `}`
		if code := postIngress(t, url, body, nil); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	assertIngressUntouched(t, m, rw)
}

// foldKey has to agree with what encoding/json matches, or a spelling could be
// decoded as a field without being seen as one.
func TestFoldKeyAgreesWithJSONFieldMatching(t *testing.T) {
	names := []string{"originatedFrom", "state", "k", "workloadInfo"}
	for _, key := range []string{"originatedfrom", "ORIGINATEDFROM", "originatedFRom", "ſtate", "K", "workloadinfo", "state", "STATE", "stat", "originated_from"} {
		var got struct {
			OriginatedFrom string `json:"originatedFrom"`
			State          string `json:"state"`
			K              string `json:"k"`
			WorkloadInfo   string `json:"workloadInfo"`
		}
		quoted, err := json.Marshal(key)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(`{`+string(quoted)+`:"x"}`), &got); err != nil {
			t.Fatal(err)
		}
		decoded := got.OriginatedFrom != "" || got.State != "" || got.K != "" || got.WorkloadInfo != ""
		folds := false
		for _, name := range names {
			folds = folds || foldKey(key) == foldKey(name)
		}
		if decoded != folds {
			t.Errorf("key %q: encoding/json decoded a field = %v, foldKey matched a name = %v", key, decoded, folds)
		}
	}
}

// The same key spelt exactly twice is one key to encoding/json (the last wins),
// and the ingress re-marshals a single one, so the origin check sees what is sent.
func TestIngestLocalRepeatedExactKeyUsesTheLastValue(t *testing.T) {
	m, rw := newTestManager(t)
	foreign := `{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","originatedFrom":"victim","createdAt":1}}`
	if err := m.ingestLocal(MethodStarted, json.RawMessage(foreign)); !errors.Is(err, errIngressOrigin) {
		t.Fatalf("err = %v, want errIngressOrigin", err)
	}
	assertIngressUntouched(t, m, rw)
}

// The canonical frame a well-behaved producer sends, with every field the schema
// has and one field of its own, is accepted, and the extra field is passed through.
func TestLocalIngressAcceptsCanonicalFrameAndPassesUnknownFieldsThrough(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	body := `{"jsonrpc":"2.0","method":"workload:started","params":{"workloadInfo":{"id":"job-17","model":"my-model","engine":"my-engine","runId":"9f3c1a7b","seq":1,"state":"running","originatedFrom":"","scheduledOn":"self-uuid","createdAt":1716998400000,"startedAt":1716998401000,"completedAt":null,"error":null,"requesterId":null,"tag":"batch"}}}`
	if code := postIngress(t, url, body, nil); code != http.StatusOK {
		t.Fatalf("canonical frame = %d, want 200", code)
	}
	if !strings.Contains(rw.String(), `"workloads:upsert"`) || !strings.Contains(rw.String(), `"originatedFrom":"self-uuid"`) {
		t.Fatalf("the broker did not get the stamped upsert: %s", rw.String())
	}
	frames := queuedFrames(m)
	if len(frames) != 1 || !strings.Contains(frames[0], `"tag":"batch"`) {
		t.Fatalf("peer frames = %v, want one carrying the unknown field", frames)
	}
}

// The decoded-value check does not look at spelling, so it holds for a frame
// that normalizeIngressParams let through.
func TestValidateIngressFrameChecksDecodedValues(t *testing.T) {
	good := func() localFrame {
		return localFrame{method: MethodStarted, wl: &Workload{ID: "W", State: StateRunning, OriginatedFrom: "self-uuid"}}
	}
	if err := validateIngressFrame(good(), "self-uuid"); err != nil {
		t.Fatalf("good lifecycle frame: %v", err)
	}
	foreign := good()
	foreign.wl.OriginatedFrom = "victim"
	if err := validateIngressFrame(foreign, "self-uuid"); !errors.Is(err, errIngressOrigin) {
		t.Errorf("foreign lifecycle origin: err = %v, want errIngressOrigin", err)
	}
	badState := good()
	badState.wl.State = "done"
	if err := validateIngressFrame(badState, "self-uuid"); err == nil {
		t.Error("unknown lifecycle state accepted")
	}
	if err := validateIngressFrame(localFrame{method: MethodRemove, removeID: "W", removeNode: "self-uuid"}, "self-uuid"); err != nil {
		t.Fatalf("good removal: %v", err)
	}
	if err := validateIngressFrame(localFrame{method: MethodRemove, removeID: "W", removeNode: "victim"}, "self-uuid"); !errors.Is(err, errIngressOrigin) {
		t.Errorf("foreign removal node: err = %v, want errIngressOrigin", err)
	}
}

// A frame whose case-variant origin key was not caught by the spelling check
// still decodes to the foreign origin, and the decoded-value check refuses it.
func TestValidateIngressFrameRefusesAnOriginTheKeyCheckMissed(t *testing.T) {
	lifecycle, err := parseLocalFrame(MethodStarted, json.RawMessage(`{"workloadInfo":{"id":"W","model":"m","engine":"e","state":"running","originatedFrom":"self-uuid","originatedfrom":"victim","createdAt":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateIngressFrame(lifecycle, "self-uuid"); !errors.Is(err, errIngressOrigin) {
		t.Errorf("lifecycle err = %v, want errIngressOrigin (decoded origin %q)", err, lifecycle.wl.OriginatedFrom)
	}
	removal, err := parseLocalFrame(MethodRemove, json.RawMessage(`{"workloadId":"W","originatedFrom":"self-uuid","originatedfrom":"victim"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateIngressFrame(removal, "self-uuid"); !errors.Is(err, errIngressOrigin) {
		t.Errorf("removal err = %v, want errIngressOrigin (decoded node %q)", err, removal.removeNode)
	}
}

func TestIngestLocalAcceptsWorkloadIDAtTheLimit(t *testing.T) {
	atLimit := strings.Repeat("a", maxIngressIDLen)
	m, _ := newTestManager(t)
	if err := m.ingestLocal(MethodStarted, ingestFrame(atLimit, 1)); err != nil {
		t.Fatalf("id of %d bytes refused: %v", maxIngressIDLen, err)
	}
	if err := m.ingestLocal(MethodRemove, json.RawMessage(`{"workloadId":"`+atLimit+`"}`)); err != nil {
		t.Fatalf("removal of an id of %d bytes refused: %v", maxIngressIDLen, err)
	}
}

func TestIngestLocalRejectsOverlongWorkloadID(t *testing.T) {
	overLimit := strings.Repeat("a", maxIngressIDLen+1)
	for name, tc := range map[string]struct{ method, params string }{
		"lifecycle": {MethodStarted, string(ingestFrame(overLimit, 1))},
		"remove":    {MethodRemove, `{"workloadId":"` + overLimit + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			m, rw := newTestManager(t)
			if err := m.ingestLocal(tc.method, json.RawMessage(tc.params)); !errors.Is(err, errIngressID) {
				t.Fatalf("err = %v, want errIngressID", err)
			}
			assertIngressUntouched(t, m, rw)
		})
	}
}

func TestLocalIngressOverlongWorkloadIDIs400(t *testing.T) {
	m, rw := newTestManager(t)
	_, url := startIngress(t, m)
	overLimit := strings.Repeat("a", maxIngressIDLen+1)
	upsert := `{"jsonrpc":"2.0","method":"workload:started","params":` + string(ingestFrame(overLimit, 1)) + `}`
	if code := postIngress(t, url, upsert, nil); code != http.StatusBadRequest {
		t.Fatalf("overlong id upsert = %d, want 400", code)
	}
	remove := `{"jsonrpc":"2.0","method":"workloads:remove","params":{"workloadId":"` + overLimit + `"}}`
	if code := postIngress(t, url, remove, nil); code != http.StatusBadRequest {
		t.Fatalf("overlong id remove = %d, want 400", code)
	}
	assertIngressUntouched(t, m, rw)
}

func TestRemovalMemoryIsBounded(t *testing.T) {
	m, _ := newTestManager(t)
	for i := 0; i < maxRemovedIngress+10; i++ {
		m.markRemoved("self-uuid", fmt.Sprintf("w%d", i))
	}
	if got := len(m.removedIngress); got != maxRemovedIngress {
		t.Fatalf("removal memory = %d, want the cap %d", got, maxRemovedIngress)
	}
	if got := m.removedOrder.Len(); got != maxRemovedIngress {
		t.Fatalf("removal order list = %d, want the cap %d", got, maxRemovedIngress)
	}
}

func TestRemovalMemoryEvictsTheOldestFirst(t *testing.T) {
	m, _ := newTestManager(t)
	const extra = 10
	for i := 0; i < maxRemovedIngress+extra; i++ {
		m.markRemoved("self-uuid", fmt.Sprintf("w%d", i))
	}
	for i := 0; i < extra; i++ {
		if m.wasRemoved("self-uuid", fmt.Sprintf("w%d", i)) {
			t.Errorf("w%d is among the oldest and should have been evicted", i)
		}
	}
	for i := extra; i < maxRemovedIngress+extra; i++ {
		if !m.wasRemoved("self-uuid", fmt.Sprintf("w%d", i)) {
			t.Fatalf("w%d was evicted but is newer than an evicted entry", i)
		}
	}
}

// Removing the same workload again refreshes its entry instead of adding one,
// and makes it the newest, so a repeated removal is not evicted before older ones.
func TestRemovalMemoryRefreshesARepeatedRemoval(t *testing.T) {
	m, _ := newTestManager(t)
	m.markRemoved("self-uuid", "first")
	for i := 0; i < maxRemovedIngress-1; i++ {
		m.markRemoved("self-uuid", fmt.Sprintf("w%d", i))
	}
	m.markRemoved("self-uuid", "first")
	m.markRemoved("self-uuid", "newest")
	if got := len(m.removedIngress); got != maxRemovedIngress {
		t.Fatalf("removal memory = %d, want %d", got, maxRemovedIngress)
	}
	if !m.wasRemoved("self-uuid", "first") {
		t.Error("the refreshed removal was evicted ahead of older ones")
	}
	if m.wasRemoved("self-uuid", "w0") {
		t.Error("the oldest removal outlived the refreshed one")
	}
}

// Expired entries are dropped from the front of the order list when a removal
// is marked, so the memory does not keep them until the cap evicts them.
func TestRemovalMemoryPrunesExpiredEntriesOnMark(t *testing.T) {
	m, _ := newTestManager(t)
	for i := 0; i < 5; i++ {
		m.markRemoved("self-uuid", fmt.Sprintf("old%d", i))
	}
	for el := m.removedOrder.Front(); el != nil; el = el.Next() {
		entry := el.Value.(removedEntry)
		entry.expiresAt = time.Now().Add(-time.Second)
		el.Value = entry
	}
	m.markRemoved("self-uuid", "fresh")
	if got := len(m.removedIngress); got != 1 || !m.wasRemoved("self-uuid", "fresh") {
		t.Fatalf("removal memory = %d entries, want only the fresh one", got)
	}
}

// Removing the same pair again below the cap replaces its entry in the order
// list as well as in the map. A list element left behind keeps the first
// removal's expiry, and dropping it when that expiry passes would delete the map
// entry of the refreshed removal.
func TestRemovalMemoryRepeatedRemovalBelowTheCapHoldsOneEntry(t *testing.T) {
	m, _ := newTestManager(t)
	m.markRemoved("self-uuid", "W")
	m.markRemoved("self-uuid", "W")
	if got := len(m.removedIngress); got != 1 {
		t.Fatalf("removal memory = %d entries, want 1", got)
	}
	if got := m.removedOrder.Len(); got != 1 {
		t.Fatalf("removal order list = %d entries, want 1 like the map", got)
	}
}

// ageRemovals moves every removal's expiry d earlier, as if d had passed since
// it was marked. The removal memory reads the clock directly, so this is how a
// test lets time go by.
func ageRemovals(m *Manager, d time.Duration) {
	for el := m.removedOrder.Front(); el != nil; el = el.Next() {
		entry := el.Value.(removedEntry)
		entry.expiresAt = entry.expiresAt.Add(-d)
		el.Value = entry
	}
}

// A repeated removal renews its tombstone: once the first removal's own expiry
// has passed, and until the renewed one does, a stale replay is still blocked,
// even after another removal has pruned the expired entries in between.
func TestRemovalMemoryRepeatedRemovalBlocksPastTheFirstExpiry(t *testing.T) {
	m, _ := newTestManager(t)
	m.markRemoved("self-uuid", "W")
	// Three quarters of the retention pass, and the producer removes it again.
	ageRemovals(m, terminalRetention*3/4)
	m.markRemoved("self-uuid", "W")
	// Half of the retention passes: the first removal expired a quarter of it ago
	// and the second has half of it left. Marking another removal prunes what has
	// expired.
	ageRemovals(m, terminalRetention/2)
	m.markRemoved("self-uuid", "other")

	stdioReplay(m, MethodStarted, "W", 1)

	if got := len(m.activeSnapshot()); got != 0 {
		t.Fatalf("re-sync set = %d, want 0: the renewed tombstone stopped blocking before it expired", got)
	}
	if frames := queuedFrames(m); len(frames) != 0 {
		t.Fatalf("peer queue = %v, want empty: the stale replay was re-broadcast", frames)
	}
}

// A tombstone lasts terminalRetention from the removal: the window in which the
// broker's replay of its store can still arrive, and as long as a finished
// workload stays in the re-sync set.
func TestRemovalMemoryTombstoneLivesForTheTerminalRetention(t *testing.T) {
	m, _ := newTestManager(t)
	before := time.Now()
	m.markRemoved("self-uuid", "W")
	after := time.Now()
	lifetime := m.removedOrder.Front().Value.(removedEntry).expiresAt.Sub(before)
	if lifetime < terminalRetention || lifetime > terminalRetention+after.Sub(before) {
		t.Fatalf("tombstone expires %v after the removal, want %v", lifetime, terminalRetention)
	}
}
