// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// count reports how many times s appears across the recorded codec frames.
// Method names appear once per emitted notification, so this counts emissions.
func (r *prRec) count(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	hay := string(r.b)
	n := 0
	for i := 0; i+len(s) <= len(hay); i++ {
		if hay[i:i+len(s)] == s {
			n++
		}
	}
	return n
}

// clientGoneWriter is an http.ResponseWriter whose body Write fails after the
// status line is sent, standing in for a client that vanished mid-stream: the
// idle write deadline trips (statusCapture.Write) and the underlying
// connection write returns an error. It records the status and supports Flush
// so the reverse proxy streams through it. This is the shape of the zombie-job
// bug — the response has committed (200), so without the wroteErr check the
// terminal would be misreported as completed.
type clientGoneWriter struct {
	header http.Header
	status int
	err    error
	wrote  bool
}

func (c *clientGoneWriter) Header() http.Header {
	if c.header == nil {
		c.header = make(http.Header)
	}
	return c.header
}

func (c *clientGoneWriter) WriteHeader(code int) { c.status = code }

func (c *clientGoneWriter) Write(b []byte) (int, error) {
	c.wrote = true
	return 0, c.err
}

func (c *clientGoneWriter) Flush() {}

// TestHandleHTTP_ClientWriteError_MarksFailed is the zombie-job regression: a
// streaming inference response that has committed (200 headers sent) but whose
// body write to the client fails — the signature of a killed / half-open client
// whose write deadline tripped — must terminate the workload as FAILED, not be
// silently reported completed.
func TestHandleHTTP_ClientWriteError_MarksFailed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"choices":[{"delta":{"content":"partial tokens"}}]}`)
	}))
	defer upstream.Close()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	cw := &clientGoneWriter{err: errors.New("write tcp: connection reset by peer")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleHTTP(cw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)))
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleHTTP did not return after the client write failed (zombie: handler blocked)")
	}

	if !cw.wrote {
		t.Fatal("reverse proxy never attempted a body write to the client; test did not exercise the streaming path")
	}
	if got := rec.count("workload:errored"); got != 1 {
		t.Fatalf("workload:errored emitted %d times, want exactly 1", got)
	}
	if rec.has("workload:completed") {
		t.Fatal("workload:completed emitted for a request whose client write failed (should be failed)")
	}
}

// TestHandleHTTP_ClientDisconnect_TerminalOnce covers the disconnect watcher: a
// request whose context is cancelled mid-flight must emit exactly one terminal
// (errored) — the watcher and post-handler path are guarded by terminalOnce —
// and handleHTTP must return promptly rather than hang.
func TestHandleHTTP_ClientDisconnect_TerminalOnce(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			io.WriteString(w, `{"choices":[{"delta":{"content":"first chunk"}}]}`+"\n")
			f.Flush()
		}
		select {
		case received <- struct{}{}:
		default:
		}
		<-release
	}))
	defer upstream.Close()
	defer doRelease()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama"}`)).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never started streaming")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleHTTP did not return after client disconnect (zombie: handler blocked)")
	}

	if got := rec.count("workload:errored"); got != 1 {
		t.Fatalf("workload:errored emitted %d times, want exactly 1 (terminalOnce guard)", got)
	}
	if rec.has("workload:completed") {
		t.Fatal("workload:completed emitted for a cancelled request")
	}
}

// deadlineRW records SetWriteDeadline calls and can force a Write error, so the
// statusCapture write-deadline mechanics can be tested without a real socket.
type deadlineRW struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	writeErr  error
	flushErr  error
	flushed   int
}

func (d *deadlineRW) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineRW) Write(b []byte) (int, error) {
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	return d.ResponseRecorder.Write(b)
}

// FlushError lets a test drive statusCapture.FlushError against a controllable
// flush outcome (recorded so the deadline arm/clear can be asserted).
func (d *deadlineRW) FlushError() error {
	d.flushed++
	return d.flushErr
}

// TestStatusCapture_WriteDeadline verifies statusCapture arms a write deadline
// around each streamed write and clears it after a successful one, and that the
// first write error is retained for the caller to classify the workload failed.
func TestStatusCapture_WriteDeadline(t *testing.T) {
	t.Run("armed then cleared on success", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder()}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		if _, err := sc.Write([]byte("tokens")); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
		if len(d.deadlines) != 2 {
			t.Fatalf("SetWriteDeadline called %d times, want 2 (arm + clear)", len(d.deadlines))
		}
		if d.deadlines[0].IsZero() {
			t.Fatal("first SetWriteDeadline should arm a future deadline, got zero")
		}
		if !d.deadlines[1].IsZero() {
			t.Fatal("second SetWriteDeadline should clear the deadline (zero time)")
		}
		if sc.wroteErr != nil {
			t.Fatalf("wroteErr set after a successful write: %v", sc.wroteErr)
		}
	})

	t.Run("write error retained, deadline not cleared", func(t *testing.T) {
		boom := errors.New("i/o timeout")
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder(), writeErr: boom}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		if _, err := sc.Write([]byte("tokens")); !errors.Is(err, boom) {
			t.Fatalf("Write err = %v, want %v", err, boom)
		}
		if !errors.Is(sc.wroteErr, boom) {
			t.Fatalf("wroteErr = %v, want %v", sc.wroteErr, boom)
		}
		if len(d.deadlines) != 1 {
			t.Fatalf("SetWriteDeadline called %d times, want 1 (arm only; not cleared on error)", len(d.deadlines))
		}
	})

	t.Run("no deadline when idle is zero", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder()}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK}
		if _, err := sc.Write([]byte("tokens")); err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
		if len(d.deadlines) != 0 {
			t.Fatalf("SetWriteDeadline called %d times with idle=0, want 0", len(d.deadlines))
		}
	})
}

// TestStatusCapture_FlushDeadline verifies the flush path is deadline-aware:
// statusCapture.FlushError arms the idle deadline around the underlying flush,
// clears it after a successful flush, and retains a real flush error (but not an
// unsupported-flush) so a stalled client's blocked flush is classified failed
// rather than hanging forever.
func TestStatusCapture_FlushDeadline(t *testing.T) {
	t.Run("armed then cleared on success", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder()}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		if err := sc.FlushError(); err != nil {
			t.Fatalf("FlushError returned error: %v", err)
		}
		if d.flushed != 1 {
			t.Fatalf("underlying flushed %d times, want 1", d.flushed)
		}
		if len(d.deadlines) != 2 {
			t.Fatalf("SetWriteDeadline called %d times, want 2 (arm + clear)", len(d.deadlines))
		}
		if d.deadlines[0].IsZero() {
			t.Fatal("flush should arm a future deadline, got zero")
		}
		if !d.deadlines[1].IsZero() {
			t.Fatal("flush should clear the deadline on success (zero time)")
		}
		if sc.wroteErr != nil {
			t.Fatalf("wroteErr set after a successful flush: %v", sc.wroteErr)
		}
	})

	t.Run("flush error retained, deadline not cleared", func(t *testing.T) {
		boom := errors.New("i/o timeout")
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder(), flushErr: boom}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		if err := sc.FlushError(); !errors.Is(err, boom) {
			t.Fatalf("FlushError = %v, want %v", err, boom)
		}
		if !errors.Is(sc.wroteErr, boom) {
			t.Fatalf("wroteErr = %v, want %v", sc.wroteErr, boom)
		}
		if len(d.deadlines) != 1 {
			t.Fatalf("SetWriteDeadline called %d times, want 1 (arm only; not cleared on error)", len(d.deadlines))
		}
	})

	t.Run("unsupported flush is not a client failure", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder(), flushErr: http.ErrNotSupported}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		_ = sc.FlushError()
		if sc.wroteErr != nil {
			t.Fatalf("ErrNotSupported must not be retained as wroteErr, got %v", sc.wroteErr)
		}
	})
}

// TestHandleHTTP_RealSocketWriteDeadline is the end-to-end, OS-level proof of
// the zombie-job fix. It drives handleHTTP over a REAL TCP socket with a client
// that reads the response headers and then stops reading — the shape of a
// killed / half-open client whose receive window closes without a FIN/RST. The
// upstream streams without end, so the proxy's kernel send buffer to the client
// fills and its next write blocks. Before the fix that write blocks ~forever
// (no terminal event; the zombie job), so r.Context() never fires and the
// handler never returns. With the fix, statusCapture arms a real
// SetWriteDeadline that the Go runtime's netpoller enforces on every platform
// (IOCP on Windows, epoll/kqueue elsewhere) regardless of the peer's TCP state,
// so the stuck write fails and the workload terminates as failed. This is the
// piece the in-process tests stub out — here the deadline is genuinely enforced
// by the OS/runtime.
func TestHandleHTTP_RealSocketWriteDeadline(t *testing.T) {
	// Shorten the idle write deadline so a stuck write trips quickly; restore
	// the production default for any test that runs after this one.
	orig := idleClientWriteTimeout
	idleClientWriteTimeout = 300 * time.Millisecond
	defer func() { idleClientWriteTimeout = orig }()

	// Upstream streams 64 KiB chunks endlessly. Once the proxy stops reading
	// from it (because the proxy is itself blocked writing to the stalled
	// client), the upstream's own writes block too — no busy loop — and it
	// unwinds when the proxy tears the connection down (write error or context
	// cancel).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunk := bytes.Repeat([]byte("x"), 64*1024)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer upstream.Close()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(p.handleHTTP)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	body := `{"model":"llama"}`
	reqText := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"\r\n" + body
	if _, err := conn.Write([]byte(reqText)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read the status line only — enough to confirm the response committed and
	// started streaming — then STOP reading so the proxy's send buffer backs
	// up. A read deadline guards against a hang if the proxy never responds.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("unexpected status line: %q", statusLine)
	}

	// The stuck write must trip the deadline and terminate the workload as
	// failed within a few multiples of the deadline — never completed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !rec.has("workload:errored") {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.has("workload:errored") {
		t.Fatal("workload never terminated after the client stopped reading (zombie: write deadline did not trip / no terminal emitted)")
	}
	if rec.has("workload:completed") {
		t.Fatal("workload:completed emitted for a client that stopped reading (should be failed)")
	}
}

// TestHandleHTTP_RealSocketFlushDeadline is the flush-path counterpart to the
// write-deadline test. A streaming response is flushed after every chunk, so a
// small chunk buffers on a successful Write (no network I/O) and the actual
// network write happens in a separate Flush. If only Write is deadline-aware, a
// stalled client makes that Flush block unbounded and the handler never returns
// — a zombie the 64 KiB Write-blocking test does not catch. This drives real,
// paced small flushed chunks over a real socket and asserts the flush deadline
// terminates the workload as failed.
func TestHandleHTTP_RealSocketFlushDeadline(t *testing.T) {
	orig := idleClientWriteTimeout
	idleClientWriteTimeout = 300 * time.Millisecond
	defer func() { idleClientWriteTimeout = orig }()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter is not a Flusher")
			return
		}
		chunk := bytes.Repeat([]byte("x"), 1500)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			flusher.Flush()
			// Pace so the reverse proxy's 32 KiB copy read returns one small
			// chunk per iteration rather than coalescing many into a >2 KiB
			// write (which would block inside Write, not Flush).
			time.Sleep(2 * time.Millisecond)
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer upstream.Close()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(p.handleHTTP)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	body := `{"model":"llama"}`
	reqText := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"\r\n" + body
	if _, err := conn.Write([]byte(reqText)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("unexpected status line: %q", statusLine)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !rec.has("workload:errored") {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.has("workload:errored") {
		t.Fatal("workload never terminated after the client stopped reading (flush path not deadline-aware)")
	}
	if rec.has("workload:completed") {
		t.Fatal("workload:completed emitted for a stalled client (should be failed)")
	}
}
