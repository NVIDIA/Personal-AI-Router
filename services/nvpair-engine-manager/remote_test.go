// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestRunRemoteRequiresNode(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"engine":"ollama"}`)}, remoteOrder{})
	mustContain(t, out.String(), "node is required")
}

func TestRunRemoteUnknownPeer(t *testing.T) {
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage("1")
	m.runRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id,
		Method: "engine:remote-install", Params: json.RawMessage(`{"node":"ghost","engine":"ollama"}`)}, remoteOrder{})
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
		Method: "engine:remote-install", Params: json.RawMessage(`{"node":"uuid-b","engine":"ollama"}`)}, remoteOrder{})
	mustContain(t, out.String(), "not clustered")
}

var demoOnNodeB = remotePullKey{node: "uuid-b", engine: "ollama", model: "demo"}

// placed puts a request naming demoOnNodeB's download in the gate, as the read
// loop does.
func placed(t *testing.T, gate *remotePullGate, method string) remoteOrder {
	t.Helper()
	params, err := json.Marshal(remoteParam{Node: demoOnNodeB.node, Engine: demoOnNodeB.engine, Model: demoOnNodeB.model})
	require.NoError(t, err, "encode params")
	return gate.place(method, params)
}

func placePull(t *testing.T, gate *remotePullGate) remoteOrder {
	t.Helper()
	return placed(t, gate, "engine:remote-pull-model")
}

func placeCancel(t *testing.T, gate *remotePullGate) remoteOrder {
	t.Helper()
	return placed(t, gate, "engine:remote-cancel-pull")
}

// awaitInBackground starts a cancel waiting on the pulls ahead of it, and
// returns a channel that closes once the gate lets it through.
func awaitInBackground(cancel remoteOrder) <-chan struct{} {
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		cancel.awaitAhead(context.Background())
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

// A pull and the cancel chasing it have to name the same download — including
// when the pull's model arrived inside params rather than as a top-level field.
func TestRemotePullKeyFrom_KeysAPullAndItsCancelByNodeEngineAndModel(t *testing.T) {
	accept := func(name, method, params string) {
		t.Run(name, func(t *testing.T) {
			key, ok := remotePullKeyFrom(method, json.RawMessage(params))

			assert.True(t, ok)
			assert.Equal(t, demoOnNodeB, key)
		})
	}
	reject := func(name, method, params string) {
		t.Run(name, func(t *testing.T) {
			key, ok := remotePullKeyFrom(method, json.RawMessage(params))

			assert.False(t, ok)
			assert.Equal(t, remotePullKey{}, key)
		})
	}
	accept("pull with a top-level model", "engine:remote-pull-model", `{"node":"uuid-b","engine":"ollama","model":"demo"}`)
	accept("pull naming its model in params", "engine:remote-pull-model", `{"node":"uuid-b","engine":"ollama","params":{"name":"demo"}}`)
	accept("cancel", "engine:remote-cancel-pull", `{"node":"uuid-b","engine":"ollama","model":"demo"}`)
	reject("install", "engine:remote-install", `{"node":"uuid-b","engine":"ollama"}`)
	reject("pull without a model", "engine:remote-pull-model", `{"node":"uuid-b","engine":"ollama"}`)
	// runRemote refuses a cancel without a top-level model, so it chases nothing.
	reject("cancel naming its model only in params", "engine:remote-cancel-pull", `{"node":"uuid-b","engine":"ollama","params":{"name":"demo"}}`)
	reject("malformed params", "engine:remote-pull-model", `{`)
}

// A cancel that reached the peer before its pull would find no download
// registered and nothing claimed, be answered as a cancel for nothing, and
// leave the transfer running under a row stuck on "Canceling".
func TestRemotePullGate_HoldsACancelUntilThePeerHasThePull(t *testing.T) {
	var gate remotePullGate
	pull := placePull(t, &gate)
	defer pull.end()

	waited := awaitInBackground(placeCancel(t, &gate))
	assertCancelHeld(t, waited, "the peer does not have the pull yet")

	pull.accepted()()
	assertCancelReleased(t, waited, "the peer has accepted the pull")
}

// The pull can also never reach the peer — an unreachable node, a rejected
// request. Its end has to let the cancel go rather than hold it for the whole
// window chasing a download that no longer exists.
func TestRemotePullGate_ReleasesACancelWhenThePullNeverLands(t *testing.T) {
	var gate remotePullGate
	pull := placePull(t, &gate)

	waited := awaitInBackground(placeCancel(t, &gate))
	assertCancelHeld(t, waited, "the pull is still in flight")

	pull.end()
	assertCancelReleased(t, waited, "the pull has ended")
	// The pull is gone, so a later cancel for the same model has nothing to
	// wait for and must not be held by the entry the first one used.
	assertCancelReleased(t, awaitInBackground(placeCancel(t, &gate)), "a later cancel has no pull to chase")
}

// The peer joins a duplicate onto the download in flight only once the
// duplicate reaches it. Let go when the first request landed, a cancel could
// overtake a second still on its way, stop the download, and leave the second
// to start it over, so the cancel waits on each request it was sent after.
func TestRemotePullGate_HoldsACancelUntilEveryDuplicateHasLanded(t *testing.T) {
	test := func(name string, settleSecond func(second remoteOrder)) {
		t.Run(name, func(t *testing.T) {
			var gate remotePullGate
			first := placePull(t, &gate)
			second := placePull(t, &gate)
			defer first.end()
			defer second.end()

			waited := awaitInBackground(placeCancel(t, &gate))
			first.accepted()()
			assertCancelHeld(t, waited, "the second request has not reached the peer")

			settleSecond(second)
			assertCancelReleased(t, waited, "no request the cancel was sent after is still on its way")
		})
	}
	test("the second reaches the peer", func(second remoteOrder) { second.accepted()() })
	test("the second ends without reaching it", func(second remoteOrder) { second.end() })
}

// A pull sent after the cancel, such as a retry, is not one it was sent to
// stop. Held for it, the cancel would reach the peer behind the retry and stop
// that too.
func TestRemotePullGate_DoesNotHoldACancelForAPullSentAfterIt(t *testing.T) {
	var gate remotePullGate
	cancel := placeCancel(t, &gate)
	retry := placePull(t, &gate)
	defer retry.end()

	assertCancelReleased(t, awaitInBackground(cancel), "the only pull was sent after the cancel")
}

// A cancel for a download nobody requested has nothing to chase, so it goes
// straight through rather than spending the whole window.
func TestRemotePullGate_PassesACancelWithNoPullRegistered(t *testing.T) {
	var gate remotePullGate

	assertCancelReleased(t, awaitInBackground(placeCancel(t, &gate)), "no pull was ever registered")
}

// Whether a pull was the last of its download outstanding decides which one
// reports how the download ended, so exactly one may say so.
func TestRemotePullGate_EndsEachPullOnceAndNamesTheLast(t *testing.T) {
	var gate remotePullGate
	first := placePull(t, &gate)
	second := placePull(t, &gate)

	assert.False(t, first.end(), "another pull of the download is still outstanding")
	assert.True(t, second.end(), "no other pull of the download is outstanding")
	assert.False(t, second.end(), "a pull ended twice")
	assert.False(t, placed(t, &gate, "engine:remote-install").end(), "a request that is not a pull ended one")

	gate.mu.Lock()
	defer gate.mu.Unlock()
	assert.Empty(t, gate.pulls, "the gate should forget a download once every pull of it has ended")
}

// sentUp is what a remote pull sent this node's UI: its engine:remote-progress
// frames, in order, and the reply that settled it.
type sentUp struct {
	progress []remoteProgress
	reply    *Message
}

func (s sentUp) stages() []string {
	stages := make([]string, 0, len(s.progress))
	for _, p := range s.progress {
		stages = append(stages, p.Stage)
	}
	return stages
}

// pullRemoteOver runs order's pull of demoOnNodeB against a peer whose stream
// is frames, and decodes what it sent up.
func pullRemoteOver(t *testing.T, order remoteOrder, frames ...streamFrame) sentUp {
	t.Helper()
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), &Executor{progress: newProgressHub()}, nil)
	id := json.RawMessage("1")
	params := remoteParam{Node: demoOnNodeB.node, Engine: demoOnNodeB.engine, Model: demoOnNodeB.model}
	m.pullRemote(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:remote-pull-model"},
		newRemoteStreamTestClient(t, frames...), demoOnNodeB.node, params, order)

	var sent sentUp
	codec := NewCodec(&out)
	for {
		msg, err := codec.Read()
		if errors.Is(err, io.EOF) {
			return sent
		}
		require.NoError(t, err, "decode what the pull sent up")
		require.Nil(t, sent.reply, "the pull sent %s after its reply", msg.Method)
		if msg.Method != "engine:remote-progress" {
			sent.reply = msg
			continue
		}
		var p remoteProgress
		require.NoError(t, json.Unmarshal(msg.Params, &p), "decode engine:remote-progress")
		sent.progress = append(sent.progress, p)
	}
}

func pullFrame(stage string) streamFrame {
	return streamFrame{Type: "progress", Engine: demoOnNodeB.engine, Model: demoOnNodeB.model, Op: "pull", Stage: stage}
}

func pullResult(result string) streamFrame {
	return streamFrame{Type: "result", Engine: demoOnNodeB.engine, Op: "pull", Result: json.RawMessage(result)}
}

// The frame a download ends on is all a UI that stopped waiting for the reply
// has to go on, and the peer's progress hub drops it along with any other frame
// a slow stream has no room for. The result still arrives, so a stream without
// that frame gets one built from the result, ahead of the reply like the rest.
func TestRemotePull_SendsTheOutcomeItsStreamDidNotCarry(t *testing.T) {
	test := func(name string, end streamFrame, want remoteProgress) {
		t.Run(name, func(t *testing.T) {
			var gate remotePullGate

			sent := pullRemoteOver(t, placePull(t, &gate), pullFrame("downloading"), end)

			require.Len(t, sent.progress, 2, "stages %v", sent.stages())
			got := sent.progress[1]
			assert.Equal(t, sent.progress[0].OpID, got.OpID, "the outcome is stamped with another operation's id")
			got.OpID = ""
			assert.Equal(t, want, got)
			require.NotNil(t, sent.reply, "the pull was never answered")
		})
	}
	test("completed", pullResult(`{"status":"success"}`),
		remoteProgress{Model: "demo", Node: "uuid-b", Engine: "ollama", Op: "pull", Stage: "success", Percent: 100, Message: "demo"})
	test("cancelled", pullResult(`{"status":"cancelled"}`),
		remoteProgress{Model: "demo", Node: "uuid-b", Engine: "ollama", Op: "pull", Stage: "cancelled", Message: "demo"})
	test("failed", streamFrame{Type: "error", Message: "disk full"},
		remoteProgress{Model: "demo", Node: "uuid-b", Engine: "ollama", Op: "pull", Stage: "error", Percent: -1, Message: "disk full"})
}

// A stream that carried its outcome needs no other: a second would tell the UI
// the download ended twice.
func TestRemotePull_AddsNoOutcomeToAStreamThatCarriedOne(t *testing.T) {
	var gate remotePullGate

	sent := pullRemoteOver(t, placePull(t, &gate), pullFrame("downloading"), pullFrame("success"), pullResult(`{"status":"success"}`))

	assert.Equal(t, []string{"downloading", "success"}, sent.stages())
}

// The UIs know a download's frames by node, engine and model alone, so while
// another pull of the same download is outstanding, its frames speak for this
// one too. Sending an outcome for the first of two to end would tell the UI a
// download had ended that the second is still reporting on.
func TestRemotePull_LeavesTheOutcomeToTheLastPullOfTheDownload(t *testing.T) {
	var gate remotePullGate
	first := placePull(t, &gate)
	second := placePull(t, &gate)

	sent := pullRemoteOver(t, first, pullFrame("downloading"), pullResult(`{"status":"cancelled"}`))
	assert.Equal(t, []string{"downloading"}, sent.stages(), "the first pull to end")

	sent = pullRemoteOver(t, second, pullFrame("downloading"), pullResult(`{"status":"success"}`))
	assert.Equal(t, []string{"downloading", "success"}, sent.stages(), "the last pull to end")
}
