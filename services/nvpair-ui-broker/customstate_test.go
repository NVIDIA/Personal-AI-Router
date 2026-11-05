// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Probe-state plane for Custom endpoints (cards plan, Task 2).
//
// custom/list used to answer with a constant placeholder for the probe
// fields, which made the desktop render every endpoint as "Unavailable" even
// while it was routing. These tests pin the replacement contract: the broker
// remembers the prober's verdicts (fed by custom/updated events and evicted
// by config replace/remove) and projects them onto the list reply, with
// "never checked" spelled as null — not false, which would read as a
// confirmed down. Two invariants carry the design:
//
//   - A verdict belongs to the endpoint that produced it. Replacing or
//     removing a key evicts its record, so an edit's follow-up read can never
//     show the previous endpoint's health or model list.
//   - The cache is display truth only; routing keeps consuming the same
//     events through the bridge, unchanged.

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stateTestURL = "http://10.0.0.9:8123"

func stateTestKey() string { return "custom:" + stateTestURL }

// customListView decodes the list reply permissively so tests can assert the
// decoded shape, not just the raw string: Healthy *bool distinguishes a
// verdict from its absence.
type customListView struct {
	ID          string   `json:"id"`
	BaseURL     string   `json:"baseUrl"`
	KeySet      bool     `json:"keySet"`
	Healthy     *bool    `json:"healthy"`
	LastChecked string   `json:"lastChecked"`
	Models      []string `json:"models"`
}

func customListReply(t *testing.T, b *Broker, client *Codec) (string, []customListView) {
	t.Helper()
	resp := customCall(t, b, client, "custom/list", `{}`)
	require.Nil(t, resp.Error, "list response")
	var entries []customListView
	require.NoError(t, json.Unmarshal(resp.Result, &entries))
	return string(resp.Result), entries
}

func customAddCall(t *testing.T, b *Broker, client *Codec, params string) {
	t.Helper()
	resp := customCall(t, b, client, "custom/add", params)
	require.Nil(t, resp.Error, "add "+params)
}

func TestCustomList_NeverCheckedReportsNullHealthy(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)

	body, entries := customListReply(t, b, client)
	require.Len(t, entries, 1)
	assert.Nil(t, entries[0].Healthy, "never-checked decodes as no verdict")
	assert.Contains(t, body, `"healthy":null`,
		"the wire spells never-checked as null; false would render a false 'Not working'")
	assert.NotContains(t, body, `"models"`, "no models claim without a probe")
}

func TestCustomList_HealthyProbeEnrichesReply(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)

	b.forwardManualNodesNotification("custom/updated",
		updatedParams(stateTestKey(), stateTestURL, true, "model-x"))

	body, entries := customListReply(t, b, client)
	require.Len(t, entries, 1)
	require.NotNil(t, entries[0].Healthy, "a probe verdict reaches the list reply (%s)", body)
	assert.True(t, *entries[0].Healthy)
	assert.Equal(t, []string{"model-x"}, entries[0].Models,
		"served models come from the probe, not the config")
	require.NotEmpty(t, entries[0].LastChecked, "a checked entry carries when it was checked")
	checkedAt, err := time.Parse(time.RFC3339, entries[0].LastChecked)
	require.NoError(t, err, "lastChecked wire format is RFC3339")
	assert.False(t, checkedAt.IsZero())
	assert.NotContains(t, body, `"models":null`)
}

func TestCustomList_FlapReportsNotWorking(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)

	b.forwardManualNodesNotification("custom/updated",
		updatedParams(stateTestKey(), stateTestURL, true, "model-x"))
	_, entries := customListReply(t, b, client)
	require.Len(t, entries, 1)
	require.NotNil(t, entries[0].Healthy, "setup: working before the flap")
	require.True(t, *entries[0].Healthy)
	require.Equal(t, []string{"model-x"}, entries[0].Models)

	b.forwardManualNodesNotification("custom/updated",
		updatedParams(stateTestKey(), stateTestURL, false))
	_, entries = customListReply(t, b, client)
	require.Len(t, entries, 1)
	require.NotNil(t, entries[0].Healthy)
	assert.False(t, *entries[0].Healthy, "a down verdict replaces the old one")
	assert.Empty(t, entries[0].Models, "a down endpoint's stale models must not keep claiming service")
}

func TestCustomList_ReplaceEvictsStaleVerdict(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"sk-old-key"}`)
	b.forwardManualNodesNotification("custom/updated",
		updatedParams(stateTestKey(), stateTestURL, true, "model-x"))

	// The edit flow re-adds the same key (here: rotating the credential).
	// The stored verdict belongs to the previous endpoint; an editor must
	// never show it for the new config.
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123,"apiKey":"sk-new-key"}`)

	body, entries := customListReply(t, b, client)
	require.Len(t, entries, 1, "a same-key edit replaces, never forks")
	assert.Nil(t, entries[0].Healthy, "the replaced entry starts unprobed")
	assert.NotContains(t, body, "model-x", "the previous endpoint's model list must not ride the edit")
	assert.True(t, entries[0].KeySet, "key presence still reported after the rotation")
	assert.NotContains(t, body, "sk-new-key", "list replies still never carry the key")
}

func TestCustomList_ReaddedEndpointStartsUnprobed(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)
	b.forwardManualNodesNotification("custom/updated",
		updatedParams(stateTestKey(), stateTestURL, true, "model-x"))

	resp := customCall(t, b, client, "custom/remove", `{"id":"custom:http://10.0.0.9:8123"}`)
	require.Nil(t, resp.Error, "remove")
	assert.Contains(t, string(resp.Result), `"removed":true`)

	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)
	body, entries := customListReply(t, b, client)
	require.Len(t, entries, 1)
	assert.Nil(t, entries[0].Healthy, "a re-added endpoint does not inherit its old probe verdict")
	assert.NotContains(t, body, "model-x", "a remove evicts the verdict; re-add is new configuration")
}

// --- custom/refresh (cards plan, Task 3) ------------------------------------
//
// Refresh must answer with truth from a fresh probe cycle, fall back to the
// cache when there is no prober to ask (a down worker reads as stale-but-
// honest, never as a hang), fork onto the relay when a peer is addressed,
// and run the same pipeline on the owner half of that relay.

// newCustomProbeAnswerFaker is an rpcWorker over one end of a net.Pipe that
// answers every request with the given JSON and records method+params — a
// prober whose next answer is known, so refresh's merge is assertable.
func newCustomProbeAnswerFaker(t *testing.T, seen chan proberCall, answer string) *rpcWorker {
	t.Helper()
	brokerSide, workerSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = workerSide.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	go worker.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(workerSide)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.ID == nil || !msg.IsRequest() {
				continue
			}
			select {
			case seen <- proberCall{method: msg.Method, params: string(msg.Params)}:
			case <-time.After(2 * time.Second):
			}
			_ = codec.Respond(msg.ID, json.RawMessage(answer))
		}
	}()
	return worker
}

// drainProberCall reads one recorded worker call, failing loudly rather than
// letting a test misattribute which forward was which.
func drainProberCall(t *testing.T, seen chan proberCall) proberCall {
	t.Helper()
	select {
	case call := <-seen:
		return call
	case <-time.After(3 * time.Second):
		require.FailNow(t, "no worker forward arrived")
		return proberCall{}
	}
}

func TestCustomRefresh_WithoutProberAnswersFromCache(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)
	b.forwardManualNodesNotification("custom/updated",
		updatedParams(stateTestKey(), stateTestURL, true, "model-x"))

	// No prober is attached: refresh must answer now, from the cached
	// verdict, instead of waiting for a probe that will never run.
	resp := customCall(t, b, client, "custom/refresh", `{}`)
	require.Nil(t, resp.Error, "refresh without a prober")
	body := string(resp.Result)
	assert.Contains(t, body, `"healthy":true`, "the cached verdict still answers")
	assert.Contains(t, body, "model-x")
}

func TestCustomRefresh_MergesProberSnapshotIntoReply(t *testing.T) {
	seen := make(chan proberCall, 4)
	worker := newCustomProbeAnswerFaker(t, seen,
		`{"endpoints":[`+
			`{"id":"custom:http://10.0.0.9:8123","baseUrl":"http://10.0.0.9:8123","healthy":true,"models":["m1"]},`+
			`{"id":"custom:http://ghost.test:7000","baseUrl":"http://ghost.test:7000","healthy":true,"models":["ghost-m"]}]}`)
	b, client, _ := customTestBroker(t, worker)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)
	setup := drainProberCall(t, seen)
	require.Equal(t, "custom/add", setup.method, "the setup add forwarded first")

	resp := customCall(t, b, client, "custom/refresh", `{}`)
	require.Nil(t, resp.Error, "refresh")

	probe := drainProberCall(t, seen)
	require.Equal(t, "custom/probe", probe.method, "an unaddressed refresh asks for a full snapshot")
	assert.Equal(t, `{}`, probe.params, "no scope means no id field on the probe request")

	body := string(resp.Result)
	assert.Contains(t, body, `"m1"`, "the refresh reply carries the fresh snapshot, not pre-refresh truth")
	assert.NotContains(t, body, "ghost-m",
		"an id the config no longer has must not resurrect as an entry or a claim")

	body, entries := customListReply(t, b, client)
	require.Len(t, entries, 1, "the ghost id must not appear in a later list either")
	require.NotNil(t, entries[0].Healthy)
	assert.True(t, *entries[0].Healthy, "the merged verdict persists past the refresh reply")
	assert.Contains(t, body, `"m1"`, "list reads the same cache refresh just merged")
}

func TestCustomRefresh_ScopedRefreshCarriesTheEndpointId(t *testing.T) {
	seen := make(chan proberCall, 4)
	worker := newCustomProbeAnswerFaker(t, seen,
		`{"endpoints":[{"id":"custom:http://10.0.0.9:8123","baseUrl":"http://10.0.0.9:8123","healthy":true,"models":["fresh-a"]}]}`)
	b, client, _ := customTestBroker(t, worker)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8124","port":8124}`)
	for i := 0; i < 2; i++ {
		drainProberCall(t, seen) // the two setup add forwards
	}

	resp := customCall(t, b, client, "custom/refresh", `{"id":"custom:http://10.0.0.9:8123"}`)
	require.Nil(t, resp.Error, "scoped refresh")

	probe := drainProberCall(t, seen)
	require.Equal(t, "custom/probe", probe.method, "scoped probe forward")
	assert.Contains(t, probe.params, `"id":"custom:http://10.0.0.9:8123"`,
		"the addressed scope travels to the prober; it decides what to re-probe")

	_, entries := customListReply(t, b, client)
	require.Len(t, entries, 2)
	byID := map[string]customListView{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	a, okA := byID["custom:http://10.0.0.9:8123"]
	require.True(t, okA)
	require.NotNil(t, a.Healthy, "the probed endpoint answers enriched")
	assert.True(t, *a.Healthy)
	assert.Equal(t, []string{"fresh-a"}, a.Models)
	assert.Nil(t, byID["custom:http://10.0.0.9:8124"].Healthy,
		"the unaddressed endpoint keeps its never-checked state; a scoped cycle makes no claim about it")
}

func TestCustomRefresh_PeerForkTravelsAsRemoteRefresh(t *testing.T) {
	seen := make(chan proberCall, 4)
	b, client, _ := customTestBroker(t, nil)
	b.setEngineMgr(newCustomProberFaker(t, seen))

	resp := customCall(t, b, client, "custom/refresh", `{"nodeId":"peer-uuid","id":"custom:http://10.0.0.9:8123"}`)
	require.Nil(t, resp.Error, "peer-addressed refresh")
	assert.Equal(t, `{"ok":true}`, string(resp.Result), "the peer's answer passes through")

	call := drainProberCall(t, seen)
	assert.Equal(t, "custom:remote-refresh", call.method,
		"a foreign nodeId forks onto the relay under the remote-refresh method")
	assert.Contains(t, call.params, `"nodeId":"peer-uuid"`, "params ride verbatim, nodeId included")
	assert.Contains(t, call.params, "custom:http://10.0.0.9:8123", "the scope travels with the fork")
}

func TestCustomRelayRefresh_OwnerAnswersFromProbeCache(t *testing.T) {
	b, client, _ := customTestBroker(t, nil)
	customAddCall(t, b, client, `{"url":"http://10.0.0.9:8123","port":8123}`)
	b.forwardManualNodesNotification("custom/updated",
		updatedParams(stateTestKey(), stateTestURL, true, "model-x"))

	fake := newCustomOwnerEmFaker(t, b)
	fake.attach(t, b)

	// The owner has no prober attached here, so the expected answer is the
	// cached verdict — the owner half must reuse the same fallback, not fail.
	fake.inject(t, `{"id":"op1","op":"refresh","request":{},"caller":"urn:nvpair:node:peer-a"}`)

	reply, raw := fake.reply(t)
	require.Empty(t, reply.Error, "relay refresh reply")
	assert.Contains(t, raw, `"healthy":true`, "the peer-addressed refresh answers with this node's probe truth")
	assert.Contains(t, raw, "model-x")
}
