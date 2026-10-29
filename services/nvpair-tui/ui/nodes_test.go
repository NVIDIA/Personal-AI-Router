// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// removalCall is one request removalBroker received.
type removalCall struct {
	method string
	params json.RawMessage
}

// removalBroker is a broker that records every call and answers it, failing
// each call to the method named by fail.
func removalBroker(t *testing.T, fail string) (*rpc.Client, <-chan removalCall) {
	t.Helper()
	c1, c2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		c1.Close()
		c2.Close()
	})
	client := rpc.NewClient(c1, c1)
	go client.Run(ctx)

	calls := make(chan removalCall, 8)
	broker := rpc.NewCodec(c2, c2)
	go func() {
		for {
			req, err := broker.Read()
			if err != nil {
				return
			}
			calls <- removalCall{method: req.Method, params: req.Params}
			reply := &rpc.Message{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)}
			if req.Method == fail {
				reply = &rpc.Message{JSONRPC: "2.0", ID: req.ID,
					Error: &rpc.RPCError{Code: -32000, Message: "refused"}}
			}
			_ = broker.Write(reply)
		}
	}()
	return client, calls
}

// confirmRemoval arms and confirms removing the selected node, then feeds each
// reply back into the view until it reports an outcome.
func confirmRemoval(t *testing.T, v *nodesView) {
	t.Helper()
	require.Nil(t, v.removeSelected(), "removal was dispatched without confirmation")
	cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	for cmd != nil {
		msg := cmd()
		if _, done := msg.(nodeActionMsg); done {
			v.Update(msg)
			return
		}
		cmd = v.Update(msg)
	}
	require.FailNow(t, "the removal ended without reporting an outcome")
}

// wantCall checks the next request the broker received carries field = value.
func wantCall(t *testing.T, calls <-chan removalCall, method, field, value string) {
	t.Helper()
	select {
	case c := <-calls:
		require.Equal(t, method, c.method)
		var params map[string]string
		require.NoError(t, json.Unmarshal(c.params, &params), "decode %s params", method)
		assert.Equal(t, value, params[field], "%s %s", method, field)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "broker never received "+method)
	}
}

// peerAddedByAddress is a nodes view with one member that was first added by
// address, selected.
func peerAddedByAddress(client *rpc.Client) *nodesView {
	v := newNodesView(client)
	v.feeds.discovered = []availableNode{{
		HostUUID: "peer", Name: "peer", IPAddress: "10.0.0.2", Trusted: true,
	}}
	v.feeds.manual = []manualNode{{ID: "manual:10.0.0.2", Address: "10.0.0.2", HostUUID: "peer"}}
	v.rebuild()
	v.selectedKey = "peer"
	return v
}

// TestRemoveMember_ForgetsItsManualEntry checks un-pairing a peer that was added
// by address forgets that address too. The broker keeps manual entries on disk
// and re-adds them when it starts, so one left behind brought the peer back.
func TestRemoveMember_ForgetsItsManualEntry(t *testing.T) {
	client, calls := removalBroker(t, "")
	v := peerAddedByAddress(client)

	confirmRemoval(t, v)

	wantCall(t, calls, "nodes:remove", "nodeUuid", "peer")
	wantCall(t, calls, "node/remove", "id", "manual:10.0.0.2")
	assert.Contains(t, v.status.render(), "remove peer ok", "the removal is reported")
}

// TestRemoveMember_ForgetsEveryManualEntry checks a peer added twice, by
// hostname and by IP, has both entries forgotten when it is un-paired.
func TestRemoveMember_ForgetsEveryManualEntry(t *testing.T) {
	client, calls := removalBroker(t, "")
	v := peerAddedByAddress(client)
	v.feeds.manual = append(v.feeds.manual,
		manualNode{ID: "manual:peer.lan", Address: "peer.lan", HostUUID: "peer"})
	v.rebuild()

	confirmRemoval(t, v)

	wantCall(t, calls, "nodes:remove", "nodeUuid", "peer")
	wantCall(t, calls, "node/remove", "id", "manual:10.0.0.2")
	wantCall(t, calls, "node/remove", "id", "manual:peer.lan")
	assert.Contains(t, v.status.render(), "remove peer ok", "the removal is reported")
}

// TestRemoveMember_KeepsTheManualEntryWhenUnpairingFails checks a peer the cluster manager
// would not un-pair keeps the address it was added by: it is still a member,
// and that address is how it is reached.
func TestRemoveMember_KeepsTheManualEntryWhenUnpairingFails(t *testing.T) {
	client, calls := removalBroker(t, "nodes:remove")
	v := peerAddedByAddress(client)

	confirmRemoval(t, v)

	wantCall(t, calls, "nodes:remove", "nodeUuid", "peer")
	assert.Empty(t, calls, "a failed un-pairing must not go on to forget the manual entry")
	assert.Contains(t, v.status.render(), "remove peer failed", "the failure is reported")
}
