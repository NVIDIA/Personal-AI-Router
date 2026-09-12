// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// errFakeTerminal is a non-EOF, non-DecodeError read failure — the shape a
// terminal scanner/transport error takes at the codec boundary.
var errFakeTerminal = errors.New("fake terminal read error")

// errorReader fails every Read with errFakeTerminal; wrapped in a real Codec it
// stands in for a dead terminal transport.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errFakeTerminal }

func newTerminalErrorCodec() *Codec {
	return NewCodec(struct {
		io.Reader
		io.Writer
	}{errorReader{}, io.Discard})
}

// TestReadLoopTerminalReadErrorStopsPump guards the read-loop contract: a
// non-EOF transport error must end Serve-style pumping with errTerminalRead
// (the producer goroutine has already stopped; spinning would burn CPU forever),
// while EOF and plain decode errors keep the loop alive.
func TestReadLoopTerminalReadErrorStopsPump(t *testing.T) {
	t.Run("non-EOF transport error is terminal", func(t *testing.T) {
		b := &Broker{codec: newTerminalErrorCodec()}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- b.readLoop(ctx) }()

		select {
		case err := <-done:
			if !errors.Is(err, errTerminalRead) {
				t.Fatalf("readLoop err = %v, want errTerminalRead", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("readLoop did not stop on transport error")
		}
	})

	t.Run("EOF is clean exit", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		b := &Broker{codec: NewCodec(client)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- b.readLoop(ctx) }()
		server.Close() // producer sees EOF

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("readLoop err on EOF = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("readLoop did not stop on EOF")
		}
	})

	t.Run("recoverable decode error keeps the loop alive", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		b := &Broker{codec: NewCodec(client)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- b.readLoop(ctx) }()
		if _, err := io.WriteString(server, "not-json\n"); err != nil {
			t.Fatal(err)
		}
		// Keep the pipe open: a closed pipe is a terminal read error, while
		// the bad frame must only surface as a recoverable DecodeError.
		time.Sleep(100 * time.Millisecond)
		select {
		case err := <-done:
			t.Fatalf("readLoop exited early on decode error: %v", err)
		default:
		}
		cancel()
	})
}
