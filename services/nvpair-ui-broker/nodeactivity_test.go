// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// The broker is the only thing joining the proxy that SEES a peer streaming to
// the scanner that decides whether to evict it. If it drops the notification,
// both components' own tests still pass and the signal is silently dead — which
// is the whole reason this test exists at the seam.

// newTestScannerRelay builds a scannerProcess with the same activity plumbing
// startScanner gives it. Constructing the struct literally would leave the queue
// nil, and a send on a nil channel takes the select's default branch — so every
// report would be silently dropped and each test would fail on a timeout with no
// hint as to why.
func newTestScannerRelay(t *testing.T, brokerSide io.ReadWriter) *scannerProcess {
	t.Helper()
	sp := &scannerProcess{
		peer:     NewPeer(NewCodec(brokerSide)),
		activity: make(chan nodeActivityReport, activityQueueDepth),
		done:     make(chan struct{}),
	}
	go sp.peer.Serve(nil, nil)
	go sp.drainNodeActivity()
	t.Cleanup(func() { close(sp.done) })
	return sp
}

// relayHarness wires a broker to a fake scanner over a pipe and returns the
// channel the relayed frame arrives on.
func relayHarness(t *testing.T) (*Broker, <-chan *Message) {
	t.Helper()
	brokerSide, scannerSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = scannerSide.Close()
	})

	sp := newTestScannerRelay(t, brokerSide)
	b := &Broker{}
	b.setScanner(sp)

	relayed := make(chan *Message, 1)
	go func() {
		codec := NewCodec(scannerSide)
		msg, err := codec.Read()
		if !assertRPCRead(t, err) {
			return
		}
		// Deliberately does NOT respond: a liveness report is a notification, so
		// a scanner that never answers must not stall the relay. See
		// TestActivityIsRelayedAsANotificationNotARequest.
		relayed <- msg
	}()
	return b, relayed
}

// The report must be a notification. The daemon has nothing to say back, so an
// id-bearing call would buy a pending entry and a goroutine blocked on an ack
// nobody reads — once per node every couple of seconds for as long as inference
// streams. This is the shape the relay was first written in, and nothing else
// would catch a regression to it.
func TestActivityIsRelayedAsANotificationNotARequest(t *testing.T) {
	b, relayed := relayHarness(t)

	b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(),
		noderec.NotifyNodeActivity, json.RawMessage(`{"hostUuid":"busy-peer"}`))

	msg := awaitRelay(t, relayed)
	require.False(t, msg.IsRequest(), "activity was relayed as an id-bearing request; it must be a notification")
}

// A scanner that never acks must not wedge the relay: the queue keeps draining
// and later reports still arrive. With an id-bearing call each report blocked a
// goroutine until its timeout instead.
func TestUnacknowledgedReportsDoNotStallTheRelay(t *testing.T) {
	b, relayed := relayHarness(t)

	for i := 0; i < 3; i++ {
		b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(),
			noderec.NotifyNodeActivity, json.RawMessage(`{"hostUuid":"busy-peer"}`))
	}
	// The harness reads exactly one frame; the point is that the two later
	// reports neither blocked the caller nor panicked on a full queue.
	awaitRelay(t, relayed)
}

func TestOllamaProxyActivityReachesTheScanner(t *testing.T) {
	b, relayed := relayHarness(t)

	b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(),
		noderec.NotifyNodeActivity, json.RawMessage(`{"hostUuid":"busy-peer"}`))

	msg := awaitRelay(t, relayed)
	require.Equal(t, noderec.MethodNodeActivity, msg.Method, "relayed method")
	var got noderec.NodeActivityParams
	require.NoError(t, json.Unmarshal(msg.Params, &got), "decode relayed params")
	require.Equal(t, "busy-peer", got.HostUUID, "relayed hostUuid")
}

// Both proxies produce this signal, so both forwarding paths must carry it. The
// LM Studio path is a separate function and has been missed by feature work
// before.
func TestLMStudioProxyActivityReachesTheScanner(t *testing.T) {
	b, relayed := relayHarness(t)

	b.forwardLMStudioProxyNotification(noderec.NotifyNodeActivity,
		json.RawMessage(`{"hostUuid":"busy-peer"}`))

	msg := awaitRelay(t, relayed)
	require.Equal(t, noderec.MethodNodeActivity, msg.Method, "relayed method")
}

// The report crosses two pipes and a goroutine hop, and the scanner measures
// freshness against the age it is given, so the delay in transit has to be added
// to the age the proxy reported rather than replacing it.
func TestRelayedActivityAgeAccumulatesTransitDelay(t *testing.T) {
	b, relayed := relayHarness(t)

	b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(),
		noderec.NotifyNodeActivity, json.RawMessage(`{"hostUuid":"busy-peer","msSince":4000}`))

	var got noderec.NodeActivityParams
	require.NoError(t, json.Unmarshal(awaitRelay(t, relayed).Params, &got), "decode relayed params")
	require.GreaterOrEqual(t, got.MSSince, int64(4000), "relayed msSince")
}

// The reported age crosses a process boundary and is multiplied into a duration,
// which is the operation that overflows. An overflowed value goes NEGATIVE, which
// places the observation in the future — and a future observation never expires,
// so the node could never be probed again.
func TestReportedAgeIsClampedAtBothEnds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		msSince int64
		want    time.Duration
	}{
		{"a normal age passes through", 4_000, 4 * time.Second},
		{"negative floors at zero", -60_000, 0},
		{"absurd caps at the ceiling", 1 << 62, activityAgeCeiling},
		{"the overflow boundary caps too", int64(1) << 53, activityAgeCeiling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := clampActivityAge(tc.msSince)
			require.Equal(t, tc.want, got, "clampActivityAge")
			require.GreaterOrEqual(t, got, time.Duration(0), "clampActivityAge")
		})
	}
}

// An unidentified peer cannot be vouched for: relaying it would credit whichever
// node ends up keyed by the empty string.
func TestActivityWithoutAHostUUIDIsNotRelayed(t *testing.T) {
	b, relayed := relayHarness(t)

	b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(),
		noderec.NotifyNodeActivity, json.RawMessage(`{"hostUuid":""}`))

	select {
	case msg := <-relayed:
		require.FailNowf(t, "relayed an activity report with no hostUuid", "%s", msg.Params)
	case <-time.After(200 * time.Millisecond):
	}
}

// Activity is discovery input, not a proxy control-plane event. Forwarding it on
// to proxy:subscribe clients would put a per-request frame on the client stream
// that no client has any use for.
func TestActivityIsNotForwardedToClients(t *testing.T) {
	brokerSide, scannerSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = scannerSide.Close()
	})
	clientSide, uiSide := net.Pipe()
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = uiSide.Close()
	})

	sp := newTestScannerRelay(t, brokerSide)
	go func() {
		codec := NewCodec(scannerSide)
		for {
			if _, err := codec.Read(); err != nil {
				return
			}
		}
	}()

	b := &Broker{codec: NewCodec(clientSide)}
	b.setScanner(sp)
	b.proxyMu.Lock()
	b.setEngineProxySubscribed(ollamaProxyProfile, true)
	b.proxyMu.Unlock()

	toClient := make(chan *Message, 1)
	go func() {
		codec := NewCodec(uiSide)
		msg, err := codec.Read()
		if !assertRPCRead(t, err) {
			return
		}
		toClient <- msg
	}()

	b.forwardProxyNotificationForGeneration(b.currentOllamaProxyGeneration(),
		noderec.NotifyNodeActivity, json.RawMessage(`{"hostUuid":"busy-peer"}`))

	select {
	case msg := <-toClient:
		require.FailNowf(t, "activity leaked onto the client stream", "method %q", msg.Method)
	case <-time.After(200 * time.Millisecond):
	}
}

func awaitRelay(t *testing.T, relayed <-chan *Message) *Message {
	t.Helper()
	select {
	case msg := <-relayed:
		return msg
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the activity report never reached the scanner")
		return nil
	}
}
