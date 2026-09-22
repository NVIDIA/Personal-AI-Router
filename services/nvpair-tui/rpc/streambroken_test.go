// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// discardWriter drops writes; these tests only exercise the read side.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestRunReturnsOnBrokenStream is the regression guard for a read loop that
// span instead of reporting a disconnect.
//
// bufio.Scanner is finished after a read error and cannot resync past an
// over-long line, so Scan returns false forever. The loop treated that like a
// frame it could not parse and continued, which burned a core, never closed the
// notifications channel, and left the UI reporting a service it could no longer
// reach.
func TestRunReturnsOnBrokenStream(t *testing.T) {
	// One frame longer than the cap, which is exactly the failure the frame
	// cap's own comment describes.
	oversized := `{"jsonrpc":"2.0","method":"x","params":"` +
		strings.Repeat("A", maxFrame+1024) + `"}` + "\n"

	c := NewClient(strings.NewReader(oversized), discardWriter{})

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamBroken) {
			t.Errorf("Run returned %v, want ErrStreamBroken", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return on a broken stream; it is spinning")
	}

	// Consumers must observe the disconnect.
	select {
	case _, open := <-c.Notifications():
		if open {
			t.Error("notifications channel delivered after the stream broke")
		}
	case <-time.After(time.Second):
		t.Error("notifications channel was never closed, so the UI never learns it is disconnected")
	}
}

// TestRunSkipsUnparseableFrame checks the other direction is unchanged: a frame
// this client does not model must not kill a working session.
func TestRunSkipsUnparseableFrame(t *testing.T) {
	stream := "{not json at all}\n" +
		`{"jsonrpc":"1.0","method":"wrong-version"}` + "\n" +
		`{"jsonrpc":"2.0","method":"app:ready"}` + "\n"

	c := NewClient(strings.NewReader(stream), discardWriter{})
	go func() { _ = c.Run(context.Background()) }()

	select {
	case msg, ok := <-c.Notifications():
		if !ok {
			t.Fatal("session ended on a malformed frame instead of skipping it")
		}
		if msg.Method != "app:ready" {
			t.Errorf("first delivered notification = %q, want app:ready", msg.Method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out; a malformed frame stalled the loop")
	}
}
