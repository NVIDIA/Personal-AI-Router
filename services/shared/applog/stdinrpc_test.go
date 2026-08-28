// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package applog

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestNotifyEmitsOneNewlineTerminatedNotification pins the wire form. A parent
// reads this stream as newline-delimited JSON-RPC, so a missing newline or a
// second frame's worth of bytes on one line stops it decoding anything further.
func TestNotifyEmitsOneNewlineTerminatedNotification(t *testing.T) {
	var buf bytes.Buffer
	params := map[string][]string{"addresses": {"10.172.54.70", "10.0.0.5"}}
	if err := NewNotifier(&buf).Notify("nodeinfo:observed-addresses", params); err != nil {
		t.Fatalf("notify: %v", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("frame is not newline-terminated: %q", out)
	}
	if got := strings.Count(out, "\n"); got != 1 {
		t.Errorf("emitted %d newlines, want exactly one frame", got)
	}

	var frame struct {
		JSONRPC string              `json:"jsonrpc"`
		Method  string              `json:"method"`
		ID      json.RawMessage     `json:"id"`
		Params  map[string][]string `json:"params"`
	}
	if err := json.Unmarshal([]byte(out), &frame); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if frame.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", frame.JSONRPC)
	}
	if frame.Method != "nodeinfo:observed-addresses" {
		t.Errorf("method = %q, want nodeinfo:observed-addresses", frame.Method)
	}
	// A notification carries no id: an id would make the parent's reader wait for
	// a reply to a report nobody asked for.
	if len(frame.ID) != 0 {
		t.Errorf("frame carries an id (%s), want a notification", frame.ID)
	}
	if got := frame.Params["addresses"]; len(got) != 2 || got[0] != "10.172.54.70" || got[1] != "10.0.0.5" {
		t.Errorf("params addresses = %v, want the two reported addresses", got)
	}
}

// A subprocess with no stdout channel gets a nil Notifier, and reporting must stay
// a plain call at every site rather than a nil check per caller.
func TestNilNotifierDropsFramesAndReportsSuccess(t *testing.T) {
	var n *Notifier

	if err := n.Notify("nodeinfo:observed-addresses", map[string][]string{"addresses": {"10.0.0.5"}}); err != nil {
		t.Errorf("nil Notifier.Notify returned %v, want the frame dropped silently", err)
	}
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"level":"debug"}}` + "\n")
	got, err := n.Write(frame)
	if err != nil {
		t.Errorf("nil Notifier.Write returned %v, want the frame dropped silently", err)
	}
	// The full length: a short write is an error to an io.Writer's caller, and
	// StdinRPC's response path would report a failure that did not happen.
	if got != len(frame) {
		t.Errorf("nil Notifier.Write wrote %d, want %d", got, len(frame))
	}
}

// splitWriter forwards each write to buf in two halves with a scheduling point
// between them. Without the Notifier's lock another writer lands in that gap and
// the two frames interleave; against a plain buffer, missing serialization would
// usually pass unnoticed with the race detector unavailable.
type splitWriter struct{ buf *bytes.Buffer }

func (w splitWriter) Write(p []byte) (int, error) {
	half := len(p) / 2
	if _, err := w.buf.Write(p[:half]); err != nil {
		return 0, err
	}
	runtime.Gosched()
	if _, err := w.buf.Write(p[half:]); err != nil {
		return half, err
	}
	return len(p), nil
}

// TestNotifierSerializesConcurrentNotifyAndWrite covers why the Notifier exists
// at all: a subprocess reports observations from whichever goroutine noticed
// them, while StdinRPC writes control responses to the same stream. Two frames
// sharing a line are unrecoverable for the parent's line-delimited reader.
func TestNotifierSerializesConcurrentNotifyAndWrite(t *testing.T) {
	const writers = 8
	const framesPerWriter = 100

	var buf bytes.Buffer
	n := NewNotifier(splitWriter{buf: &buf})
	// The shape StdinRPC's response path hands to Write, already marshaled.
	response := []byte(`{"jsonrpc":"2.0","id":7,"result":{"level":"debug"}}` + "\n")

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for range framesPerWriter {
				if w%2 == 0 {
					params := map[string][]string{"addresses": {"10.172.54.70", "10.0.0.5", "192.168.240.1"}}
					if err := n.Notify("nodeinfo:observed-addresses", params); err != nil {
						t.Errorf("notify from writer %d: %v", w, err)
						return
					}
					continue
				}
				if _, err := n.Write(response); err != nil {
					t.Errorf("write from writer %d: %v", w, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("stream does not end on a frame boundary: last 80 bytes = %q", tail(out, 80))
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != writers*framesPerWriter {
		t.Fatalf("read back %d frames, want %d", len(lines), writers*framesPerWriter)
	}
	for i, line := range lines {
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("frame %d is not one complete JSON object (%v): %q", i, err, line)
		}
		if frame.JSONRPC != "2.0" {
			t.Fatalf("frame %d jsonrpc = %q, want 2.0", i, frame.JSONRPC)
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
