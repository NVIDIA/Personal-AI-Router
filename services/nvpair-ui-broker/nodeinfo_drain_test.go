// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// observedFrame is one observed-addresses notification in the wire form node-info
// writes to its stdout.
func observedFrame(addr string) string {
	return `{"jsonrpc":"2.0","method":"` + noderec.NotifyObservedAddresses +
		`","params":{"addresses":["` + addr + `"]}}`
}

type drainedFrame struct {
	method string
	params json.RawMessage
}

// drainFrames runs the production drain over stdout and returns what it
// delivered. The timeout is the real assertion for a malformed stream: a drain
// that neither advances nor returns is the failure mode being guarded against.
func drainFrames(t *testing.T, stdout io.Reader) []drainedFrame {
	t.Helper()
	var got []drainedFrame
	done := make(chan struct{})
	go func() {
		defer close(done)
		drainNodeInfoStdout(stdout, func(method string, params json.RawMessage) {
			got = append(got, drainedFrame{method: method, params: params})
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain neither consumed the stream nor returned")
	}
	return got
}

func addressesOf(t *testing.T, params json.RawMessage) []string {
	t.Helper()
	var p noderec.ObservedAddressesParams
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatalf("decode params %s: %v", params, err)
	}
	return p.Addresses
}

// An oversized frame must cost only itself. The drain is the sole reader of
// node-info's stdout, so ending it leaves the child blocked on a full pipe and
// permanently stops observed-address delivery — the feed address selection
// depends on.
func TestNodeInfoDrainSkipsAnOversizedFrameAndKeepsReading(t *testing.T) {
	oversized := `{"jsonrpc":"2.0","method":"` + noderec.NotifyObservedAddresses +
		`","params":{"addresses":["` + strings.Repeat("a", maxNodeInfoLine) + `"]}}`
	stream := oversized + "\n" + observedFrame("10.172.54.70") + "\n"

	got := drainFrames(t, strings.NewReader(stream))
	if len(got) != 1 {
		t.Fatalf("delivered %d frames, want only the frame that followed the oversized one", len(got))
	}
	if got[0].method != noderec.NotifyObservedAddresses {
		t.Fatalf("method = %q, want %q", got[0].method, noderec.NotifyObservedAddresses)
	}
	if addrs := addressesOf(t, got[0].params); len(addrs) != 1 || addrs[0] != "10.172.54.70" {
		t.Fatalf("addresses = %v, want [10.172.54.70]", addrs)
	}
}

// An oversized frame that is also the last thing on the stream must end the drain
// rather than spin on a buffer it can never empty.
func TestNodeInfoDrainStopsOnAnUnterminatedOversizedFrame(t *testing.T) {
	oversized := `{"method":"x","params":"` + strings.Repeat("a", 2*maxNodeInfoLine) + `"}`

	if got := drainFrames(t, strings.NewReader(oversized)); len(got) != 0 {
		t.Fatalf("delivered %d frames, want none", len(got))
	}
}

// node-info can exit having written a frame but not its newline; that frame is
// still real output.
func TestNodeInfoDrainDeliversAFinalFrameWithoutANewline(t *testing.T) {
	got := drainFrames(t, strings.NewReader(observedFrame("10.172.54.71")))
	if len(got) != 1 {
		t.Fatalf("delivered %d frames, want the unterminated final frame", len(got))
	}
	if addrs := addressesOf(t, got[0].params); len(addrs) != 1 || addrs[0] != "10.172.54.71" {
		t.Fatalf("addresses = %v, want [10.172.54.71]", addrs)
	}
}

// Responses to the broker's own control requests, blank lines, and anything that
// isn't JSON share the stream with notifications and carry no method.
func TestNodeInfoDrainSkipsFramesThatAreNotNotifications(t *testing.T) {
	stream := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"result":{"level":"debug"}}`,
		`not json at all`,
		``,
		observedFrame("10.172.54.72"),
	}, "\n") + "\n"

	got := drainFrames(t, strings.NewReader(stream))
	if len(got) != 1 {
		t.Fatalf("delivered %d frames, want only the notification", len(got))
	}
	if got[0].method != noderec.NotifyObservedAddresses {
		t.Fatalf("method = %q, want %q", got[0].method, noderec.NotifyObservedAddresses)
	}
}
