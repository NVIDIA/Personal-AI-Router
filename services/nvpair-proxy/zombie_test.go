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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file runs on one arbitrary engine (anyCase), not both. Its subject is
// statusCapture's write and flush deadlines and the terminalOnce guard, which
// never consult the profile; the inference request is only the vehicle that
// reaches the streaming path. Two of these drive real sockets against
// multi-second deadlines, so a second pass would cost real time for no branch.

// count reports how many newline-delimited frames the codec wrote that mention
// s. Method names appear once per emitted notification, so counting the
// substring counts emissions.
func (r *recRW) count(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Count(string(r.b), s)
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

// TestHandleHTTP_ClientWriteError_MarksCancelled is the zombie-job regression: a
// streaming inference response that has committed (200 headers sent) but whose
// body write to the client fails — the signature of a killed / half-open client
// whose write deadline tripped — must terminate the workload as `cancelled`,
// not be silently reported completed. A default arm that sees no ctx cancel, no
// upstream error, and a 2xx status would call this a success and leave a
// truncated stream looking clean.
func TestHandleHTTP_ClientWriteError_MarksCancelled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"model":"llama","response":"partial tokens before the client vanished","done":false}`)
	}))
	defer upstream.Close()

	tc := anyCase(t)
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

	cw := &clientGoneWriter{err: errors.New("write tcp: connection reset by peer")}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.soleFacade().handleHTTP(cw, tc.inferenceRequest())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test expectation failed", "handleHTTP did not return after the client write failed (zombie: handler blocked)")
	}

	require.True(t, cw.wrote, "reverse proxy never attempted a body write to the client; test did not exercise the streaming path")
	{
		got := rec.count("workload:errored")
		require.Equal(t, 1, got, "workload:errored emitted")
	}
	require.False(t, rec.has("workload:completed"), "workload:completed emitted for a request whose client write failed")
	require.True(t, rec.has(`"state":"cancelled"`), "a client that vanished mid-stream must terminate as cancelled, not failed")
}

// TestHandleHTTP_ClientDisconnect_TerminalOnce covers the disconnect watcher: a
// request whose context is cancelled mid-flight (client disconnect / shutdown)
// must emit exactly one terminal (errored) — the watcher and the post-handler
// path are guarded by terminalOnce so they can't double-emit — and handleHTTP
// must return promptly rather than hang.
func TestHandleHTTP_ClientDisconnect_TerminalOnce(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			io.WriteString(w, `{"response":"first chunk","done":false}`+"\n")
			f.Flush()
		}
		select {
		case received <- struct{}{}:
		default:
		}
		<-release // block mid-stream until the test releases us
	}))
	// LIFO: unblock the upstream handler before tearing the server down.
	defer upstream.Close()
	defer doRelease()

	tc := anyCase(t)
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

	ctx, cancel := context.WithCancel(context.Background())
	req := tc.inferenceRequest().WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.soleFacade().handleHTTP(httptest.NewRecorder(), req)
	}()

	// Wait until the upstream is actively streaming, then simulate the client
	// going away.
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test expectation failed", "upstream never started streaming")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test expectation failed", "handleHTTP did not return after client disconnect (zombie: handler blocked)")
	}

	{
		got := rec.count("workload:errored")
		require.Equal(t, 1, got, "workload:errored emitted")
	}
	require.False(t, rec.has("workload:completed"), "workload:completed emitted for a cancelled request")
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
// around each streamed write and clears it after a successful one (so idle gaps
// between tokens never penalize a live-but-slow generation), and that the first
// write error is retained for the caller to classify the workload failed.
func TestStatusCapture_WriteDeadline(t *testing.T) {
	t.Run("armed then cleared on success", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder()}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		{
			_, err := sc.Write([]byte("tokens"))
			require.NoError(t, err, "Write returned error")
		}
		require.Len(t, d.deadlines, 2, "SetWriteDeadline called")
		require.False(t, d.deadlines[0].IsZero(), "first SetWriteDeadline should arm a future deadline, got zero")
		require.True(t, d.deadlines[1].IsZero(), "second SetWriteDeadline should clear the deadline (zero time)")
		require.NoError(t, sc.wroteErr, "wroteErr set after a successful write")
	})

	t.Run("write error retained, deadline not cleared", func(t *testing.T) {
		boom := errors.New("i/o timeout")
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder(), writeErr: boom}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		{
			_, err := sc.Write([]byte("tokens"))
			require.ErrorIs(t, err, boom, "Write err")
		}
		require.ErrorIs(t, sc.wroteErr, boom, "wroteErr")
		require.Len(t, d.deadlines, 1, "SetWriteDeadline called")
	})

	t.Run("no deadline when idle is zero", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder()}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK}
		{
			_, err := sc.Write([]byte("tokens"))
			require.NoError(t, err, "Write returned error")
		}
		require.Len(t, d.deadlines, 0, "SetWriteDeadline called")
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
		require.NoError(t, sc.FlushError(), "FlushError returned error")
		require.Equal(t, 1, d.flushed, "underlying flushed")
		require.Len(t, d.deadlines, 2, "SetWriteDeadline called")
		require.False(t, d.deadlines[0].IsZero(), "flush should arm a future deadline, got zero")
		require.True(t, d.deadlines[1].IsZero(), "flush should clear the deadline on success (zero time)")
		require.NoError(t, sc.wroteErr, "wroteErr set after a successful flush")
	})

	t.Run("flush error retained, deadline not cleared", func(t *testing.T) {
		boom := errors.New("i/o timeout")
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder(), flushErr: boom}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		{
			err := sc.FlushError()
			require.ErrorIs(t, err, boom, "FlushError")
		}
		require.ErrorIs(t, sc.wroteErr, boom, "wroteErr")
		require.Len(t, d.deadlines, 1, "SetWriteDeadline called")
	})

	t.Run("unsupported flush is not a client failure", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder(), flushErr: http.ErrNotSupported}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, idle: 50 * time.Millisecond}
		_ = sc.FlushError()
		require.NoError(t, sc.wroteErr, "ErrNotSupported must not be retained as wroteErr")
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

	tc := anyCase(t)
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	srv := &http.Server{Handler: http.HandlerFunc(p.soleFacade().handleHTTP)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err, "dial proxy")
	defer conn.Close()

	body := tc.inferenceBody()
	reqText := fmt.Sprintf("POST %s HTTP/1.1\r\n", tc.inferencePath) +
		"Host: localhost\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"\r\n" + body
	{
		_, err := conn.Write([]byte(reqText))
		require.NoError(t, err, "write request")
	}

	// Read the status line only — enough to confirm the response committed and
	// started streaming — then STOP reading so the proxy's send buffer backs
	// up. A read deadline guards against a hang if the proxy never responds.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err, "read status line")
	require.Contains(t, statusLine, "200", "unexpected status line")

	// The stuck write must trip the deadline and terminate the workload as
	// failed within a few multiples of the deadline — never completed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !rec.has("workload:errored") {
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, rec.has("workload:errored"), "workload never terminated after the client stopped reading (zombie: write deadline did not trip / no terminal emitted)")
	require.False(t, rec.has("workload:completed"), "workload:completed emitted for a client that stopped reading")
	require.True(t, rec.has(`"state":"cancelled"`), "a client that stopped reading must terminate as cancelled, not failed")
}

// TestHandleHTTP_RealSocketFlushDeadline is the flush-path counterpart to the
// write-deadline test. A streaming response is flushed after every chunk
// (ReverseProxy uses immediate flushing for chunked/streaming upstreams), so a
// small chunk is buffered by a successful Write (no network I/O) and the actual
// network write happens in a *separate* Flush. If only Write is deadline-aware,
// a stalled client makes that Flush block unbounded and the handler never
// returns — a zombie the 64 KiB Write-blocking test does not catch. This drives
// real, paced small flushed chunks over a real socket and asserts the flush
// deadline terminates the workload as failed.
func TestHandleHTTP_RealSocketFlushDeadline(t *testing.T) {
	orig := idleClientWriteTimeout
	idleClientWriteTimeout = 300 * time.Millisecond
	defer func() { idleClientWriteTimeout = orig }()

	// Small (<2 KiB) chunks each followed by Flush: every chunk buffers on
	// Write without touching the socket, then the network write happens in
	// Flush. ReverseProxy flushes after each write for a chunked response, so
	// the block lands in Flush, never in Write.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			assert.Fail(t, "test expectation failed", "upstream ResponseWriter is not a Flusher")
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

	tc := anyCase(t)
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	srv := &http.Server{Handler: http.HandlerFunc(p.soleFacade().handleHTTP)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err, "dial proxy")
	defer conn.Close()

	body := tc.inferenceBody()
	reqText := fmt.Sprintf("POST %s HTTP/1.1\r\n", tc.inferencePath) +
		"Host: localhost\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"\r\n" + body
	{
		_, err := conn.Write([]byte(reqText))
		require.NoError(t, err, "write request")
	}

	// Read the status line only, then stop reading so the proxy's send buffer
	// backs up and a Flush blocks.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err, "read status line")
	require.Contains(t, statusLine, "200", "unexpected status line")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !rec.has("workload:errored") {
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, rec.has("workload:errored"), "workload never terminated after the client stopped reading (flush path not deadline-aware)")
	require.False(t, rec.has("workload:completed"), "workload:completed emitted for a stalled client")
	require.True(t, rec.has(`"state":"cancelled"`), "a stalled client must terminate as cancelled, not failed")
}

// TestHandleHTTP_UpstreamDiesMidStream_EmitsTerminal is the other half of the
// zombie-job story, and the half no in-process test can express. Here the
// CLIENT stays healthy — it reads everything the proxy sends — and the
// UPSTREAM dies mid-body instead.
//
// That combination defeats both existing reporters. Nothing cancels the
// request context, because no write to the client ever fails, so the
// disconnect watcher never fires. And the copy error is never returned to us:
// ReverseProxy converts a mid-copy failure into panic(http.ErrAbortHandler)
// whenever it detects a real server, and that panic unwinds straight past the
// end of handleHTTP. A terminal emitted inline is therefore lost outright and
// the workload stays "running" for the life of the broker. The cost is not
// just a stuck card: the scheduler counts queued and running by scheduledOn,
// so the ghost permanently inflates that node's pending count and biases
// routing away from a node whose only sin was a crashed engine.
//
// A real http.Server is essential. shouldPanicOnCopyError keys off
// ServerContextKey, so calling handleHTTP directly — as the in-process tests
// above do — suppresses the panic and cannot reach this path at all.
//
// The response committed a 2xx before truncating, so the terminal is a
// COMPLETION: a cut-off stream is a success the caller resumes from whatever
// it received, not a cluster failure.
func TestHandleHTTP_UpstreamDiesMidStream_EmitsTerminal(t *testing.T) {
	// A raw listener rather than httptest, so the abrupt close is exact: a
	// chunked response that stops without its terminating zero-length chunk,
	// which is what the proxy sees when an engine is killed mid-generation.
	// Accepting in a loop keeps the address probe in targetURL from consuming
	// the one connection the request needs.
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen upstream")
	defer upLn.Close()
	go func() {
		for {
			conn, err := upLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						c.Close()
						return
					}
					if line == "\r\n" {
						break
					}
				}
				chunk := `{"response":"partial tokens","done":false}`
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
				fmt.Fprintf(c, "%x\r\n%s\r\n", len(chunk), chunk)
				c.Close()
			}(conn)
		}
	}()

	tc := anyCase(t)
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", "http://"+upLn.Addr().String(), tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen proxy")
	srv := &http.Server{Handler: http.HandlerFunc(p.soleFacade().handleHTTP)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err, "dial proxy")
	defer conn.Close()

	body := tc.inferenceBody()
	reqText := fmt.Sprintf("POST %s HTTP/1.1\r\n", tc.inferencePath) +
		"Host: localhost\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"\r\n" + body
	{
		_, err := conn.Write([]byte(reqText))
		require.NoError(t, err, "write request")
	}

	// Keep the client healthy by draining until the proxy hangs up. A client
	// that stopped reading would trip the write deadline and cancel the
	// context, which is the case the tests above cover and would mask this one.
	go func() { _, _ = io.Copy(io.Discard, conn) }()

	deadline := time.Now().Add(5 * time.Second)
	terminals := func() int { return rec.count("workload:completed") + rec.count("workload:errored") }
	for time.Now().Before(deadline) && terminals() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	{
		got := terminals()
		require.Equal(t, 1, got, "terminal workload events")
	}
	require.True(t, rec.has("workload:completed"), "a truncated stream that had already committed 2xx must terminate as completed, not failed")
}
