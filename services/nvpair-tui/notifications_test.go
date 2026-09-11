// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-tui/pairing"
	"nvpair-tui/rpc"
)

// TestTheFanOutFeedsPairingBeforeTheUI checks the property the whole design
// rests on: the pairing service observes an invitation whether or not the UI is
// reading. The UI channel here is left unread and undersized on purpose.
func TestTheFanOutFeedsPairingBeforeTheUI(t *testing.T) {
	pushes := make(chan *rpc.Message, 1)
	toUI := make(chan *rpc.Message) // nobody reads this
	pairs := pairing.NewService(nil)
	var ready atomic.Bool

	done := make(chan struct{})
	go func() {
		defer close(done)
		fanOutNotifications(pushes, pairs, &ready, toUI)
	}()

	pushes <- &rpc.Message{
		JSONRPC: "2.0",
		Method:  "cluster:invite-received",
		Params:  json.RawMessage(`{"inviteId":"inv-1","fromNodeName":"Lab desk A","state":"pending"}`),
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(pairs.Pending()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the pairing service never saw the invitation, because the UI was not reading")
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(pushes)
	<-done
}

// TestReadinessIsWithdrawnWhenTheBrokerGoesAway guards the answer `ping` gives
// a script after the broker has died. Reporting a ready broker there would send
// the script on to an invite that can only time out.
func TestReadinessIsWithdrawnWhenTheBrokerGoesAway(t *testing.T) {
	pushes := make(chan *rpc.Message, 2)
	toUI := make(chan *rpc.Message, 8)
	var ready atomic.Bool

	done := make(chan struct{})
	go func() {
		defer close(done)
		fanOutNotifications(pushes, pairing.NewService(nil), &ready, toUI)
	}()

	pushes <- &rpc.Message{JSONRPC: "2.0", Method: "app:ready", Params: json.RawMessage(`{"version":"1.2.3"}`)}
	deadline := time.Now().Add(5 * time.Second)
	for !ready.Load() {
		if time.Now().After(deadline) {
			t.Fatal("app:ready did not mark the broker ready")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The broker's notification stream closing is how this process learns the
	// broker is gone.
	close(pushes)
	<-done

	if ready.Load() {
		t.Error("the broker is gone but ping would still report it ready")
	}
	// app:ready was forwarded to the UI as well, so drain what is buffered and
	// then confirm the channel is closed rather than merely empty — that close
	// is what makes the UI render "broker disconnected".
	forwarded := 0
	for range toUI {
		forwarded++
	}
	if forwarded != 1 {
		t.Errorf("the UI received %d notifications, want the one app:ready", forwarded)
	}
}
