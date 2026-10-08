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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNotifyEmitsOneNewlineTerminatedNotification pins the wire form. A parent
// reads this stream as newline-delimited JSON-RPC, so a missing newline or a
// second frame's worth of bytes on one line stops it decoding anything further.
func TestNotifyEmitsOneNewlineTerminatedNotification(t *testing.T) {
	var buf bytes.Buffer
	params := map[string][]string{"addresses": {"10.172.54.70", "10.0.0.5"}}
	require.NoError(t, NewNotifier(&buf).Notify("nodeinfo:observed-addresses", params), "notify")

	out := buf.String()
	assert.True(t, strings.HasSuffix(out, "\n"), "frame is not newline-terminated")
	assert.Equal(t, 1, strings.Count(out, "\n"))

	var frame struct {
		JSONRPC string              `json:"jsonrpc"`
		Method  string              `json:"method"`
		ID      json.RawMessage     `json:"id"`
		Params  map[string][]string `json:"params"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &frame), "decode")
	assert.Equal(t, "2.0", frame.JSONRPC)
	assert.Equal(t, "nodeinfo:observed-addresses", frame.Method)
	// A notification carries no id: an id would make the parent's reader wait for
	// a reply to a report nobody asked for.
	assert.Empty(t, frame.ID, "frame carries an id")
	assert.Equal(t, []string{"10.172.54.70", "10.0.0.5"}, frame.Params["addresses"], "params addresses")
}

// A subprocess with no stdout channel gets a nil Notifier, and reporting must stay
// a plain call at every site rather than a nil check per caller.
func TestNilNotifierDropsFramesAndReportsSuccess(t *testing.T) {
	var n *Notifier

	assert.NoError(t, n.Notify("nodeinfo:observed-addresses", map[string][]string{"addresses": {"10.0.0.5"}}), "nil Notifier.Notify returned")
	frame := []byte(`{"jsonrpc":"2.0","id":1,"result":{"level":"debug"}}` + "\n")
	got, err := n.Write(frame)
	assert.NoError(t, err, "nil Notifier.Write returned")
	// The full length: a short write is an error to an io.Writer's caller, and
	// StdinRPC's response path would report a failure that did not happen.
	assert.Equal(t, len(frame), got, "nil Notifier.Write wrote")
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
						assert.Fail(t, "test expectation failed", "notify from writer")
						return
					}
					continue
				}
				if _, err := n.Write(response); err != nil {
					assert.Fail(t, "test expectation failed", "write from writer")
					return
				}
			}
		}(w)
	}
	wg.Wait()

	out := buf.String()
	assert.True(t, strings.HasSuffix(out, "\n"), "stream does not end on a frame boundary: last 80 bytes")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	require.Len(t, lines, writers*framesPerWriter, "read back")
	for i, line := range lines {
		var frame struct {
			JSONRPC string `json:"jsonrpc"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &frame), "frame %d", i)
		assert.Equal(t, "2.0", frame.JSONRPC)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
