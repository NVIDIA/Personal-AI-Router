// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

func TestRunRemoteRequiresNode(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"engine":"ollama"}`)})
	mustContain(t, out.String(), "node is required")
}

func TestRunRemoteUnknownPeer(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"node":"ghost","engine":"ollama"}`)})
	mustContain(t, out.String(), "not a discovered ec peer")
}

func TestRunRemoteNotClustered(t *testing.T) {
	var out bytes.Buffer
	// Peer is discovered, but this node has no mesh (not clustered), so the
	// pinned dial can't be built.
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	m.peers.set([]noderec.DirectoryNode{{
		HostUUID: "uuid-b", Name: "nodeB", IP: "192.168.1.42", ClusterUUID: "cuuid-b",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceEngineControl: {Port: 14323},
		},
	}})
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"node":"uuid-b","engine":"ollama"}`)})
	mustContain(t, out.String(), "not clustered")
}

// Only a remote pull is registered, and its key has to match the one a cancel
// builds from its own params — including when the model arrived inside params
// rather than as a top-level field.
func TestRemotePullClaimFrom(t *testing.T) {
	test := func(name, method, params, wantKey string) {
		t.Run(name, func(t *testing.T) {
			key, isPull := remotePullClaimFrom(method, json.RawMessage(params))
			if isPull != (wantKey != "") {
				t.Fatalf("isPull = %v, want %v", isPull, wantKey != "")
			}
			if key != wantKey {
				t.Errorf("key = %q, want %q", key, wantKey)
			}
		})
	}
	test("pull with a top-level model", "engine:remote-pull-model",
		`{"node":"uuid-b","engine":"ollama","model":"demo"}`, remotePullKey("uuid-b", "ollama", "demo"))
	test("pull naming its model in params", "engine:remote-pull-model",
		`{"node":"uuid-b","engine":"ollama","params":{"name":"demo"}}`, remotePullKey("uuid-b", "ollama", "demo"))
	test("cancel is not registered", "engine:remote-cancel-pull",
		`{"node":"uuid-b","engine":"ollama","model":"demo"}`, "")
	test("install is not registered", "engine:remote-install",
		`{"node":"uuid-b","engine":"ollama"}`, "")
	test("pull without a model", "engine:remote-pull-model",
		`{"node":"uuid-b","engine":"ollama"}`, "")
	test("malformed params", "engine:remote-pull-model", `{`, "")
}

// A cancel that reached the peer before its pull would find no download
// registered and nothing claimed, be answered as a cancel for nothing, and
// leave the transfer running under a row stuck on "Canceling".
func TestRemotePullGateHoldsACancelUntilThePeerHasThePull(t *testing.T) {
	var gate remotePullGate
	key := remotePullKey("uuid-b", "ollama", "demo")
	release := gate.register(key, true)
	defer release()

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		gate.awaitAccepted(context.Background(), key)
	}()
	select {
	case <-waited:
		t.Fatal("cancel was sent before the peer had the pull")
	case <-time.After(50 * time.Millisecond):
	}

	gate.accepted(key)()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel kept waiting after the peer accepted the pull")
	}
}

// The pull can also never reach the peer — an unreachable node, a rejected
// request. Its release has to let the cancel go rather than hold it for the
// whole window chasing a download that no longer exists.
func TestRemotePullGateReleasesACancelWhenThePullNeverLands(t *testing.T) {
	var gate remotePullGate
	key := remotePullKey("uuid-b", "ollama", "demo")
	release := gate.register(key, true)

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		gate.awaitAccepted(context.Background(), key)
	}()
	select {
	case <-waited:
		t.Fatal("cancel was sent while the pull attempt was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel outlived the pull attempt it was chasing")
	}
	// The attempt is gone, so a later cancel for the same model has nothing to
	// wait for and must not be held by the entry the first one used.
	gate.awaitAccepted(context.Background(), key)
}

// Two requests for the same remote download share one attempt, because the peer
// joins the duplicate onto the transfer already in flight. What releases the
// cancel is the peer accepting, which happens however many requests are
// outstanding — not the first of them ending. While one is still on its way to
// the peer there is still a pull to chase, and letting the cancel go early
// sends it to a peer with nothing registered, which is answered as a cancel for
// nothing and leaves the row stuck on "Canceling".
func TestRemotePullGateSharesOneAttemptAcrossDuplicateRequests(t *testing.T) {
	var gate remotePullGate
	key := remotePullKey("uuid-b", "ollama", "demo")
	first := gate.register(key, true)
	second := gate.register(key, true)

	first()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		gate.awaitAccepted(context.Background(), key)
	}()
	select {
	case <-waited:
		t.Fatal("cancel was sent while the second request was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	// Acceptance frees it without that request having ended.
	gate.accepted(key)()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel kept waiting after the peer accepted the pull")
	}
	second()

	gate.mu.Lock()
	remaining := len(gate.pulls)
	gate.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("gate retained %d attempt(s) after both requests ended", remaining)
	}
}

// When no request reaches the peer, the last one ending is what frees the
// cancel — the shared attempt has to be exhausted, not merely reduced.
func TestRemotePullGateHoldsACancelUntilEveryDuplicateHasEnded(t *testing.T) {
	var gate remotePullGate
	key := remotePullKey("uuid-b", "ollama", "demo")
	first := gate.register(key, true)
	second := gate.register(key, true)

	first()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		gate.awaitAccepted(context.Background(), key)
	}()
	select {
	case <-waited:
		t.Fatal("cancel was sent while the second request was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	second()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel outlived every request it was chasing")
	}
}

// A cancel for a download nobody requested has nothing to chase, so it goes
// straight through rather than spending the whole window.
func TestRemotePullGatePassesACancelWithNoPullRegistered(t *testing.T) {
	var gate remotePullGate
	done := make(chan struct{})
	go func() {
		defer close(done)
		gate.awaitAccepted(context.Background(), remotePullKey("uuid-b", "ollama", "demo"))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel waited on a pull that was never registered")
	}
}
