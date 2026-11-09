// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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

var demoOnNodeB = remotePullKey{node: "uuid-b", engine: "ollama", model: "demo"}

// awaitInBackground starts a cancel waiting on the gate, and returns a channel
// that closes once the gate lets it through.
func awaitInBackground(gate *remotePullGate, key remotePullKey) <-chan struct{} {
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		gate.awaitAccepted(context.Background(), key)
	}()
	return waited
}

func assertCancelHeld(t *testing.T, waited <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-waited:
		assert.Fail(t, "the cancel was let through early", why)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertCancelReleased(t *testing.T, waited <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		assert.Fail(t, "the cancel was never let through", why)
	}
}

// Only a remote pull is registered, and its key has to match the one a cancel
// builds from its own params — including when the model arrived inside params
// rather than as a top-level field.
func TestRemotePullClaimFrom_KeysOnlyARemotePullByNodeEngineAndModel(t *testing.T) {
	accept := func(name, params string) {
		t.Run(name, func(t *testing.T) {
			key, isPull := remotePullClaimFrom("engine:remote-pull-model", json.RawMessage(params))

			assert.True(t, isPull)
			assert.Equal(t, demoOnNodeB, key)
		})
	}
	reject := func(name, method, params string) {
		t.Run(name, func(t *testing.T) {
			key, isPull := remotePullClaimFrom(method, json.RawMessage(params))

			assert.False(t, isPull)
			assert.Equal(t, remotePullKey{}, key)
		})
	}
	accept("pull with a top-level model", `{"node":"uuid-b","engine":"ollama","model":"demo"}`)
	accept("pull naming its model in params", `{"node":"uuid-b","engine":"ollama","params":{"name":"demo"}}`)
	reject("cancel is not registered", "engine:remote-cancel-pull", `{"node":"uuid-b","engine":"ollama","model":"demo"}`)
	reject("install is not registered", "engine:remote-install", `{"node":"uuid-b","engine":"ollama"}`)
	reject("pull without a model", "engine:remote-pull-model", `{"node":"uuid-b","engine":"ollama"}`)
	reject("malformed params", "engine:remote-pull-model", `{`)
}

// A cancel that reached the peer before its pull would find no download
// registered and nothing claimed, be answered as a cancel for nothing, and
// leave the transfer running under a row stuck on "Canceling".
func TestRemotePullGate_HoldsACancelUntilThePeerHasThePull(t *testing.T) {
	var gate remotePullGate
	release := gate.register(demoOnNodeB, true)
	defer release()

	waited := awaitInBackground(&gate, demoOnNodeB)
	assertCancelHeld(t, waited, "the peer does not have the pull yet")

	gate.accepted(demoOnNodeB)()
	assertCancelReleased(t, waited, "the peer has accepted the pull")
}

// The pull can also never reach the peer — an unreachable node, a rejected
// request. Its release has to let the cancel go rather than hold it for the
// whole window chasing a download that no longer exists.
func TestRemotePullGate_ReleasesACancelWhenThePullNeverLands(t *testing.T) {
	var gate remotePullGate
	release := gate.register(demoOnNodeB, true)

	waited := awaitInBackground(&gate, demoOnNodeB)
	assertCancelHeld(t, waited, "the pull attempt is still in flight")

	release()
	assertCancelReleased(t, waited, "the pull attempt has ended")
	// The attempt is gone, so a later cancel for the same model has nothing to
	// wait for and must not be held by the entry the first one used.
	assertCancelReleased(t, awaitInBackground(&gate, demoOnNodeB), "a later cancel has no attempt to chase")
}

// Two requests for the same remote download share one attempt, because the peer
// joins the duplicate onto the transfer already in flight. What releases the
// cancel is the peer accepting, which happens however many requests are
// outstanding — not the first of them ending. While one is still on its way to
// the peer there is still a pull to chase, and letting the cancel go early
// sends it to a peer with nothing registered, which is answered as a cancel for
// nothing and leaves the row stuck on "Canceling".
func TestRemotePullGate_SharesOneAttemptAcrossDuplicateRequests(t *testing.T) {
	var gate remotePullGate
	first := gate.register(demoOnNodeB, true)
	second := gate.register(demoOnNodeB, true)

	first()
	waited := awaitInBackground(&gate, demoOnNodeB)
	assertCancelHeld(t, waited, "the second request is still in flight")

	gate.accepted(demoOnNodeB)()
	assertCancelReleased(t, waited, "the peer has accepted the pull, although a request is still outstanding")

	second()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	assert.Empty(t, gate.pulls, "the gate should forget an attempt once every request for it has ended")
}

// When no request reaches the peer, the last one ending is what frees the
// cancel — the shared attempt has to be exhausted, not merely reduced.
func TestRemotePullGate_HoldsACancelUntilEveryDuplicateHasEnded(t *testing.T) {
	var gate remotePullGate
	first := gate.register(demoOnNodeB, true)
	second := gate.register(demoOnNodeB, true)

	first()
	waited := awaitInBackground(&gate, demoOnNodeB)
	assertCancelHeld(t, waited, "the second request is still in flight")

	second()
	assertCancelReleased(t, waited, "every request for the pull has ended")
}

// A cancel for a download nobody requested has nothing to chase, so it goes
// straight through rather than spending the whole window.
func TestRemotePullGate_PassesACancelWithNoPullRegistered(t *testing.T) {
	var gate remotePullGate

	assertCancelReleased(t, awaitInBackground(&gate, demoOnNodeB), "no pull was ever registered")
}
