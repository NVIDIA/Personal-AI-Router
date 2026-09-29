// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// TestOwnLogReachesTheLogsTab checks this program's log goes where it can be
// read. While the full-screen program runs, stderr is behind it; each line has
// to arrive on the Logs channel instead, and pass through to stderr otherwise.
func TestOwnLogReachesTheLogsTab(t *testing.T) {
	var stderr bytes.Buffer
	out := &logOutput{fallback: &stderr}

	_, _ = out.Write([]byte("before\n"))
	if stderr.String() != "before\n" {
		t.Errorf("a line before the program started did not reach stderr: %q", stderr.String())
	}

	lines := make(chan string, 4)
	out.attach(lines)
	_, _ = out.Write([]byte("one\ntwo\n"))
	for _, want := range []string{"one", "two"} {
		select {
		case got := <-lines:
			if got != want {
				t.Errorf("Logs tab got %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%q never reached the Logs tab", want)
		}
	}

	out.detach()
	_, _ = out.Write([]byte("after\n"))
	if stderr.String() != "before\nafter\n" {
		t.Errorf("a line after the program ended did not reach stderr: %q", stderr.String())
	}
}

// TestOwnLogNeverBlocksOnAFullTab checks a full buffer drops a line rather
// than waiting. The update loop that drains the channel also logs, so a
// blocking write from there would wait on itself.
func TestOwnLogNeverBlocksOnAFullTab(t *testing.T) {
	out := &logOutput{fallback: &bytes.Buffer{}}
	lines := make(chan string, 1)
	out.attach(lines)

	done := make(chan struct{})
	go func() {
		_, _ = out.Write([]byte("fits\nfull\nstill full\n"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("writing to a full Logs tab blocked")
	}
}

// TestDecodeOrLogReportsAMismatch checks a shape the broker and this client
// disagree about is reported rather than read as zero values.
func TestDecodeOrLogReportsAMismatch(t *testing.T) {
	var into struct {
		Port int `json:"port"`
	}
	if decodeOrLog("test:method", json.RawMessage(`{"port":"not a number"}`), &into) {
		t.Error("a payload that did not fit its type was reported as decoded")
	}
	if !decodeOrLog("test:method", json.RawMessage(`{"port":1234}`), &into) || into.Port != 1234 {
		t.Errorf("a good payload did not decode: %+v", into)
	}
	if !decodeOrLog("test:method", nil, &into) {
		t.Error("an empty payload, which carries nothing to decode, was reported as a failure")
	}
}
