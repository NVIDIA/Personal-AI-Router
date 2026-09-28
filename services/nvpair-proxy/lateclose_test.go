// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Coverage for the classification of a client that closes its connection
// while a committed stream is still unwinding.
//
// Observed live on an eight-node cluster fronting llama.cpp: 120 streaming chat
// completions all returned 200 with the full token stream and a usage block,
// yet 21 were labelled `cancelled` ("client disconnected before completion").
// The client closed its socket the instant it had read `data: [DONE]`; the
// router sends EOF 0-5 ms after that frame, so the request context was
// cancelled while the copy still waited on the upstream — and the cancel also
// cancels the upstream request, so the copy ended in a context error, never
// EOF. A client that read to EOF instead produced 24/24 `completed`.
//
// These drive that window over real sockets, per streamed dialect: a client
// that hangs up on the terminal frame must be `completed`, and one that hangs
// up mid-stream must stay `cancelled`.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lateCloseDialect is one streamed inference dialect: the route the client
// calls, the frames the upstream writes, and the substrings at which the two
// clients hang up.
type lateCloseDialect struct {
	name string
	tc   func(*testing.T) engineCase
	path string
	// frames are written in order, each flushed; the last ends the stream.
	frames []string
	// mid is a substring of the second frame, where the mid-stream client
	// closes.
	mid string
	// end is the terminal marker, where the late-close client closes.
	end string
}

func lateCloseDialects() []lateCloseDialect {
	return []lateCloseDialect{
		{
			name: "openai-sse", tc: llamacppCase, path: "/v1/chat/completions",
			frames: []string{
				"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":3}}\n\n",
				"data: [DONE]\n\n",
			},
			mid: `"content":"lo"`,
			end: "data: [DONE]",
		},
		{
			name: "anthropic-sse", tc: llamacppCase, path: "/v1/messages",
			frames: []string{
				"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hello\"}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
			mid: `"text":"Hello"`,
			end: "event: message_stop",
		},
		{
			name: "ollama-ndjson", tc: ollamaCase, path: "/api/chat",
			frames: []string{
				"{\"message\":{\"role\":\"assistant\",\"content\":\"Hel\"},\"done\":false}\n",
				"{\"message\":{\"role\":\"assistant\",\"content\":\"lo\"},\"done\":false}\n",
				"{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"done_reason\":\"stop\",\"done\":true}\n",
			},
			mid: `"content":"lo"`,
			end: `"done":true`,
		},
	}
}

// lateCloseUpstream streams d.frames one flushed write at a time.
//
// With holdBeforeLast set it writes the first two frames and then waits for the
// proxy to cancel the request, which a mid-stream client close causes; only if
// no cancel arrives does it go on to write the rest, so a misclassification
// shows up as the wrong state rather than a hang. Otherwise it writes every
// frame and then holds the connection open — EOF withheld — until the proxy
// cancels it or the hold elapses, recording whether the cancel came. That hold
// is the window the defect lives in: the client has the terminal frame while
// the upstream has not yet sent EOF.
func lateCloseUpstream(t *testing.T, d lateCloseDialect, holdBeforeLast bool, cancelled *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter is not a Flusher")
			return
		}
		if strings.HasPrefix(d.frames[0], "data:") || strings.HasPrefix(d.frames[0], "event:") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/x-ndjson")
		}
		w.WriteHeader(http.StatusOK)
		write := func(frames []string) {
			for _, f := range frames {
				if _, err := io.WriteString(w, f); err != nil {
					return
				}
				flusher.Flush()
				// Pace so each frame is its own read on the proxy side, as a
				// generating engine's tokens are.
				time.Sleep(2 * time.Millisecond)
			}
		}
		if holdBeforeLast {
			write(d.frames[:2])
			select {
			case <-r.Context().Done():
				cancelled.Store(true)
				return
			case <-time.After(2 * time.Second):
			}
			write(d.frames[2:])
			return
		}
		write(d.frames)
		select {
		case <-r.Context().Done():
			cancelled.Store(true)
		case <-time.After(2 * time.Second):
		}
	}))
}

// lateCloseProxy serves handleHTTP on a real listener. A real http.Server is
// essential twice over: only it cancels r.Context() when the client's socket
// closes, and only under it does ReverseProxy turn the interrupted copy into
// the ErrAbortHandler panic finalize has to classify through.
func lateCloseProxy(t *testing.T, tc engineCase, upstreamURL string) (*recRW, net.Listener) {
	t.Helper()
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstreamURL, tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(p.soleFacade().handleHTTP)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return rec, ln
}

// readUntilThenClose sends a streaming inference request over a raw socket,
// reads the response until the accumulated text contains needle, and closes
// the socket at once — the shape of a client that stops at the frame it was
// waiting for rather than draining to EOF.
func readUntilThenClose(t *testing.T, addr string, tc engineCase, path, needle string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	body := fmt.Sprintf(`{"model":%q,"stream":true}`, tc.requestedModel)
	reqText := fmt.Sprintf("POST %s HTTP/1.1\r\n", path) +
		"Host: localhost\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(body)) +
		"\r\n" + body
	if _, err := conn.Write([]byte(reqText)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	var got strings.Builder
	for !strings.Contains(got.String(), needle) {
		line, err := br.ReadString('\n')
		got.WriteString(line)
		if err != nil {
			t.Fatalf("reading response before %q arrived: %v\nread so far:\n%s", needle, err, got.String())
		}
	}
	if !strings.Contains(got.String(), " 200 ") {
		t.Fatalf("response did not commit a 200:\n%s", got.String())
	}
}

// terminalWorkloads returns the workload carried by every terminal
// notification the codec wrote, in order.
func (r *recRW) terminalWorkloads() []Workload {
	r.mu.Lock()
	raw := append([]byte(nil), r.b...)
	r.mu.Unlock()
	var out []Workload
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var frame struct {
			Method string         `json:"method"`
			Params workloadParams `json:"params"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}
		if frame.Method == workloadCompletedMethod || frame.Method == workloadErroredMethod {
			out = append(out, frame.Params.WorkloadInfo)
		}
	}
	return out
}

// awaitTerminal waits for exactly one terminal workload event and returns it.
func awaitTerminal(t *testing.T, rec *recRW) Workload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(rec.terminalWorkloads()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// A second emission, if one were coming, would follow the first closely;
	// give it the chance so a double-emit is caught rather than raced past.
	time.Sleep(50 * time.Millisecond)
	terminals := rec.terminalWorkloads()
	if len(terminals) != 1 {
		t.Fatalf("terminal workload events = %d, want exactly 1", len(terminals))
	}
	return terminals[0]
}

// TestHandleHTTP_ClientClosesAfterTerminalFrame_Completed is the defect: a
// client that closes the instant it has read the terminal frame, before the
// upstream's EOF, has the whole response and must be reported `completed` with
// no error — not `cancelled`, though r.Context() is cancelled and the copy is
// interrupted.
func TestHandleHTTP_ClientClosesAfterTerminalFrame_Completed(t *testing.T) {
	for _, d := range lateCloseDialects() {
		t.Run(d.name, func(t *testing.T) {
			tc := d.tc(t)
			var upstreamCancelled atomic.Bool
			upstream := lateCloseUpstream(t, d, false, &upstreamCancelled)
			defer upstream.Close()
			rec, ln := lateCloseProxy(t, tc, upstream.URL)

			readUntilThenClose(t, ln.Addr().String(), tc, d.path, d.end)

			wl := awaitTerminal(t, rec)
			if wl.State != "completed" {
				t.Fatalf("state = %q, want completed: a client that hung up after the terminal frame had the whole response (error=%v)", wl.State, deref(wl.Error))
			}
			if wl.Error != nil {
				t.Fatalf("completed workload carries error %q, want none", *wl.Error)
			}
			if rec.has("workload:errored") {
				t.Fatal("workload:errored emitted for a fully delivered stream")
			}
			// The proxy's cancel must have reached the upstream while it was
			// still holding EOF back: that is the window under test, and a
			// run where EOF won the race would pass without exercising it.
			if !upstreamCancelled.Load() {
				t.Fatal("upstream saw no cancel before EOF: the client close did not race the copy, so the defect window was not exercised")
			}
		})
	}
}

// TestHandleHTTP_ClientClosesMidStream_Cancelled pins the other side: a client
// that closes after the second frame, before the terminal one, abandoned the
// response and stays `cancelled`.
func TestHandleHTTP_ClientClosesMidStream_Cancelled(t *testing.T) {
	for _, d := range lateCloseDialects() {
		t.Run(d.name, func(t *testing.T) {
			tc := d.tc(t)
			var upstreamCancelled atomic.Bool
			upstream := lateCloseUpstream(t, d, true, &upstreamCancelled)
			defer upstream.Close()
			rec, ln := lateCloseProxy(t, tc, upstream.URL)

			readUntilThenClose(t, ln.Addr().String(), tc, d.path, d.mid)

			wl := awaitTerminal(t, rec)
			if wl.State != "cancelled" {
				t.Fatalf("state = %q, want cancelled: the client left before the terminal frame (error=%v)", wl.State, deref(wl.Error))
			}
			if wl.Error == nil || *wl.Error == "" {
				t.Fatal("cancelled workload carries no error message")
			}
			if rec.has("workload:completed") {
				t.Fatal("workload:completed emitted for a client that left mid-stream")
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestStatusCapture_BodyComplete pins the completion rules statusCapture
// applies to what it writes, without a socket.
func TestStatusCapture_BodyComplete(t *testing.T) {
	newSC := func(terminal streamTerminal, contentLength int64) *statusCapture {
		return &statusCapture{
			ResponseWriter: httptest.NewRecorder(),
			status:         http.StatusOK,
			terminal:       terminal,
			contentLength:  contentLength,
		}
	}

	t.Run("terminal frame at line start completes", func(t *testing.T) {
		sc := newSC(sseDone, -1)
		sc.Write([]byte("data: {\"choices\":[]}\n\n"))
		if sc.bodyComplete() {
			t.Fatal("complete before the terminal frame")
		}
		sc.Write([]byte("data: [DONE]\n\n"))
		if !sc.bodyComplete() {
			t.Fatal("not complete after data: [DONE]")
		}
	})

	t.Run("marker quoted inside model text does not complete", func(t *testing.T) {
		sc := newSC(sseDone, -1)
		sc.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"data: [DONE]\"}}]}\n\n"))
		if sc.bodyComplete() {
			t.Fatal("a quoted marker mid-line ended the stream")
		}
	})

	t.Run("marker split from its newline across writes completes", func(t *testing.T) {
		sc := newSC(anthropicStop, -1)
		sc.Write([]byte("event: message_delta\ndata: {}\n\n"))
		sc.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		if !sc.bodyComplete() {
			t.Fatal("a terminal frame at the start of a write, after a write ending in newline, was missed")
		}
	})

	t.Run("marker at the start of a write mid-line does not complete", func(t *testing.T) {
		sc := newSC(sseDone, -1)
		sc.Write([]byte("data: {\"content\":\""))
		sc.Write([]byte("data: [DONE]\"}\n\n"))
		if sc.bodyComplete() {
			t.Fatal("a marker continuing a broken line was taken for a frame")
		}
	})

	t.Run("ndjson done frame completes", func(t *testing.T) {
		sc := newSC(ndjsonDone, -1)
		sc.Write([]byte("{\"message\":{\"content\":\"hi\"},\"done\":false}\n"))
		if sc.bodyComplete() {
			t.Fatal("complete on a done:false frame")
		}
		sc.Write([]byte("{\"message\":{\"content\":\"\"},\"done\":true}\n"))
		if !sc.bodyComplete() {
			t.Fatal("not complete after the done:true frame")
		}
	})

	t.Run("content length reached completes", func(t *testing.T) {
		sc := newSC(streamTerminal{}, 10)
		sc.Write([]byte("12345"))
		if sc.bodyComplete() {
			t.Fatal("complete at half the declared length")
		}
		sc.Write([]byte("67890"))
		if !sc.bodyComplete() {
			t.Fatal("not complete at the declared length")
		}
	})

	t.Run("upstream EOF completes once its bytes are written", func(t *testing.T) {
		sc := newSC(streamTerminal{}, -1)
		body := completionBody{ReadCloser: io.NopCloser(strings.NewReader("tail")), sc: sc}
		buf := make([]byte, 32)
		n, err := body.Read(buf)
		if n != 4 || (err != nil && err != io.EOF) {
			t.Fatalf("Read = %d, %v", n, err)
		}
		if err != io.EOF {
			// strings.Reader returns EOF on the following read; drive it.
			if _, err = body.Read(buf); err != io.EOF {
				t.Fatalf("second Read err = %v, want EOF", err)
			}
			if !sc.bodyComplete() {
				t.Fatal("a bare EOF did not complete the body")
			}
			return
		}
		if sc.bodyComplete() {
			t.Fatal("complete before the bytes that came with EOF were written")
		}
		sc.Write(buf[:n])
		if !sc.bodyComplete() {
			t.Fatal("not complete after writing the bytes that came with EOF")
		}
	})

	t.Run("failed write never completes", func(t *testing.T) {
		d := &deadlineRW{ResponseRecorder: httptest.NewRecorder(), writeErr: io.ErrClosedPipe}
		sc := &statusCapture{ResponseWriter: d, status: http.StatusOK, terminal: sseDone}
		sc.Write([]byte("data: [DONE]\n\n"))
		if sc.bodyComplete() {
			t.Fatal("a terminal frame the client never received counted as complete")
		}
	})
}
