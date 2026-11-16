// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"nvpair-shared/noderec"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discoveryPush builds a discovery:nodes-changed notification.
func discoveryPush(t *testing.T, nodes ...availableNode) NotificationMsg {
	t.Helper()
	params, err := json.Marshal(nodes)
	require.NoError(t, err)
	return NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: params}}
}

func hardwarePush(t *testing.T, value noderec.NodeInfoEvent) NotificationMsg {
	t.Helper()
	params, err := json.Marshal(value)
	require.NoError(t, err)
	return NotificationMsg{Msg: &rpc.Message{Method: noderec.NotifyNodeInfo, Params: params}}
}

func TestNodesView_SeedsDetailFromDiscoverySnapshot(t *testing.T) {
	test := func(name string, verification noderec.IdentityVerification) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			v := newNodesView(nil)
			v.SetSize(120, 40)
			node := availableNode{
				HostUUID: "host", DiscoveryRecordID: "record:host",
				IdentityVerification: verification, Name: "gpu-host",
				IPAddress: "192.0.2.10", Port: 14318,
				Info: &noderec.NodeInfo{
					GPUs:           []noderec.GPUInfo{{Name: "Test GPU", VramBytes: 8 << 30, UtilizationPercent: 42}},
					CPU:            &noderec.CPUInfo{Name: "Test CPU", Cores: 8},
					Memory:         &noderec.MemoryInfo{TotalBytes: 32 << 30, UsedBytes: 12 << 30},
					TelemetryValid: true,
				},
			}
			if verification == noderec.IdentityLocal {
				v.feeds.selfUUID = "host"
			}

			v.Update(discoveryPush(t, node))
			v.openDetail()

			require.NotNil(t, v.detail)
			assert.Contains(t, v.View(), "Test GPU")
			assert.Contains(t, v.View(), "42%")
			assert.Contains(t, v.View(), "Test CPU")
			assert.Contains(t, v.View(), "12.0 GiB / 32.0 GiB")
		})
	}
	test("local owned hardware", noderec.IdentityLocal)
	test("paired peer hardware", noderec.IdentityPinned)
	test("unpaired display preview", noderec.IdentityUnverified)
}

func TestNodesView_UpdatesOpenDetailFromCurrentRecord(t *testing.T) {
	test := func(name string, verification noderec.IdentityVerification) {
		t.Run(name, func(t *testing.T) {
			v := newNodesView(nil)
			v.SetSize(120, 40)
			node := availableNode{
				HostUUID: "host", DiscoveryRecordID: "record:host",
				IdentityVerification: verification, Name: "gpu-host",
				IPAddress: "192.0.2.10", Port: 14318,
			}
			v.Update(discoveryPush(t, node))
			v.openDetail()
			require.NotNil(t, v.detail)
			require.Contains(t, v.detail.hardwareBlock(), "unavailable")

			v.Update(hardwarePush(t, noderec.NodeInfoEvent{
				HostUUID: "host", DiscoveryRecordID: "record:host",
				IdentityVerification: verification,
				Info: noderec.NodeInfo{
					HostUUID: "host", TelemetryValid: true,
					GPUs: []noderec.GPUInfo{{Name: "Live GPU", UtilizationPercent: 61}},
				},
			}))
			assert.Contains(t, v.View(), "Live GPU")
			assert.Contains(t, v.View(), "61%")
		})
	}
	test("local hardware", noderec.IdentityLocal)
	test("paired peer hardware", noderec.IdentityPinned)
	test("unpaired display preview", noderec.IdentityUnverified)
}

// An open detail screen may receive hardware for an old certificate record or
// another discovery. Matching only the host UUID is insufficient; mismatched
// record, verification, or body identity must leave the hardware unavailable.
func TestNodesView_RejectsUpdatesForReplacedOrDifferentRecords(t *testing.T) {
	test := func(name, record, host, bodyHost string, verification noderec.IdentityVerification) {
		t.Run(name, func(t *testing.T) {
			v := newNodesView(nil)
			v.SetSize(100, 30)
			v.Update(discoveryPush(t, availableNode{
				HostUUID: "host", DiscoveryRecordID: "record:current",
				IdentityVerification: noderec.IdentityPinned, Name: "gpu-host",
				IPAddress: "192.0.2.10", Port: 14318, Trusted: true,
			}))
			v.openDetail()
			require.NotNil(t, v.detail)

			v.Update(hardwarePush(t, noderec.NodeInfoEvent{
				HostUUID: host, DiscoveryRecordID: record, IdentityVerification: verification,
				Info: noderec.NodeInfo{HostUUID: bodyHost, GPUs: []noderec.GPUInfo{{Name: "Wrong GPU"}}},
			}))

			assert.Contains(t, v.detail.hardwareBlock(), "unavailable")
			assert.NotContains(t, v.View(), "Wrong GPU")
		})
	}
	test("replaced certificate record", "record:old", "host", "host", noderec.IdentityPinned)
	test("different host", "record:current", "other", "other", noderec.IdentityPinned)
	test("mismatched body identity", "record:current", "host", "other", noderec.IdentityPinned)
	test("unverified update for a paired record", "record:current", "host", "host", noderec.IdentityUnverified)
}

// A paired row and an unverified discovery deliberately claim the same host.
// A preview update must reach only the independent discovery row, preserving
// the paired row's hardware and the preview row's unverified membership state.
func TestNodesView_PreviewCannotOverwriteVerifiedHost(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.Update(discoveryPush(t,
		availableNode{
			HostUUID: "host", DiscoveryRecordID: "peer:host", IdentityVerification: noderec.IdentityPinned,
			Name: "paired", IPAddress: "192.0.2.10", Port: 14318, Trusted: true,
			Info: &noderec.NodeInfo{HostUUID: "host", GPUs: []noderec.GPUInfo{{Name: "Verified GPU"}}},
		},
		availableNode{
			HostUUID: "host", DiscoveryRecordID: "candidate:copy", IdentityVerification: noderec.IdentityUnverified,
			Name: "candidate", IPAddress: "192.0.2.20", Port: 14318,
		},
	))

	v.Update(hardwarePush(t, noderec.NodeInfoEvent{
		HostUUID: "host", DiscoveryRecordID: "candidate:copy", IdentityVerification: noderec.IdentityUnverified,
		Info: noderec.NodeInfo{HostUUID: "host", GPUs: []noderec.GPUInfo{{Name: "Preview GPU"}}},
	}))

	paired := findRow(t, v.all, "paired")
	preview := findRow(t, v.all, "candidate")
	require.NotNil(t, paired.info)
	require.NotNil(t, preview.info)
	assert.Equal(t, "Verified GPU", paired.info.GPUs[0].Name)
	assert.Equal(t, "Preview GPU", preview.info.GPUs[0].Name)
	assert.Equal(t, noderec.IdentityUnverified, preview.identityVerification)
	assert.Equal(t, membershipNone, preview.membership)
	assert.Equal(t, "candidate:copy", preview.key)
}

// Keep the detail screen open while the broker removes the node or withdraws
// only its hardware. Both updates must clear the displayed sample immediately,
// without requiring the user to close and reopen the screen.
func TestNodesView_WithdrawsOpenHardwareWithRecord(t *testing.T) {
	test := func(name string, removeRecord bool) {
		t.Run(name, func(t *testing.T) {
			v := newNodesView(nil)
			v.SetSize(100, 30)
			node := availableNode{
				HostUUID: "host", DiscoveryRecordID: "peer:host", IdentityVerification: noderec.IdentityPinned,
				Name: "paired", IPAddress: "192.0.2.10", Port: 14318, Trusted: true,
				Info: &noderec.NodeInfo{HostUUID: "host", GPUs: []noderec.GPUInfo{{Name: "Previous GPU"}}},
			}
			v.Update(discoveryPush(t, node))
			v.openDetail()
			require.NotNil(t, v.detail)
			require.Contains(t, v.detail.hardwareBlock(), "Previous GPU")

			if removeRecord {
				v.Update(discoveryPush(t))
			} else {
				node.Info = nil
				v.Update(discoveryPush(t, node))
			}

			assert.Contains(t, v.detail.hardwareBlock(), "unavailable")
			assert.NotContains(t, v.View(), "Previous GPU")
		})
	}
	test("node removed", true)
	test("hardware invalidated", false)
}

// Open details before nodes/list identifies the row's manual entry, then deliver
// that association and authenticate the peer. The open screen must follow the
// same entry to its verified UUID, replacing preview hardware and model state.
func TestNodesView_FollowsManualPromotionAfterLateEntryAssociation(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	v.Update(discoveryPush(t, availableNode{
		HostUUID: "manual:entry", DiscoveryRecordID: "manual:entry",
		IdentityVerification: noderec.IdentityUnverified, Name: "lab",
		IPAddress: "192.0.2.10", Port: 14318,
		Info: &noderec.NodeInfo{GPUs: []noderec.GPUInfo{{Name: "Preview GPU"}}},
	}))
	v.openDetail()
	require.NotNil(t, v.detail)
	require.Empty(t, v.detail.node.manualIDs, "nodes/list has not associated the entry yet")

	v.Update(manualNodesMsg{nodes: []manualNode{{ID: "entry", Address: "192.0.2.10", OllamaUp: true}}})
	v.Update(manualNodesMsg{nodes: []manualNode{{
		ID: "entry", Address: "192.0.2.10", HostUUID: "host",
		IdentityVerification: noderec.IdentityPinned, OllamaUp: true, NodeInfoUp: true,
	}}})
	v.Update(discoveryPush(t, availableNode{
		HostUUID: "host", DiscoveryRecordID: "peer:host",
		IdentityVerification: noderec.IdentityPinned, Name: "lab",
		IPAddress: "192.0.2.10", Port: 14318, Trusted: true,
		Models: []string{"peer-model"}, ModelsByEngine: map[string][]string{"ollama": {"peer-model"}},
		Info: &noderec.NodeInfo{HostUUID: "host", GPUs: []noderec.GPUInfo{{Name: "Verified GPU"}}},
	}))

	assert.Equal(t, []string{"entry"}, v.detail.node.manualIDs)
	assert.Equal(t, "host", v.detail.node.key)
	assert.Equal(t, membershipMember, v.detail.node.membership)
	assert.Contains(t, v.View(), "Verified GPU")
	assert.NotContains(t, v.View(), "Preview GPU")
	assert.Equal(t, []string{"peer-model"}, v.detail.models.Models)
}

// UI ticks preserve receipt time. A new hardware event replaces it, so the
// displayed age follows the new sample instead of the previous one.
func TestNodesView_PreservesHardwareAgeAcrossTicksAndRefreshesItOnUpdates(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	node := availableNode{
		HostUUID: "host", DiscoveryRecordID: "peer:host",
		IdentityVerification: noderec.IdentityPinned, Name: "host",
		Info: &noderec.NodeInfo{
			HostUUID: "host", GPUs: []noderec.GPUInfo{{Name: "GPU", UtilizationPercent: 73}},
			TelemetryValid: true, MSSince: 0,
		},
	}
	v.Update(discoveryPush(t, node))
	require.False(t, v.feeds.discovered[0].infoReceivedAt.IsZero())
	v.openDetail()
	require.NotNil(t, v.detail)

	// Advance elapsed time without a sleep or another sample.
	receivedAt := time.Now().Add(-2 * time.Minute)
	v.feeds.discovered[0].infoReceivedAt = receivedAt
	v.Update(TickMsg{})
	v.Update(TickMsg{})

	assert.Contains(t, v.detail.hardwareBlock(), "73% (2m ago)")
	assert.Equal(t, receivedAt, v.detail.node.infoReceivedAt)

	v.Update(hardwarePush(t, noderec.NodeInfoEvent{
		HostUUID: "host", DiscoveryRecordID: "peer:host",
		IdentityVerification: noderec.IdentityPinned, Info: *node.Info,
	}))

	assert.Contains(t, v.detail.hardwareBlock(), "73% (0s ago)")
	assert.True(t, v.detail.node.infoReceivedAt.After(receivedAt))
}

// Withdrawing a manual node's identity proof removes its previous hardware
// while the independently configured engine remains reachable.
func TestNodesView_HidesManualHardwareAfterProofWithdrawal(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	manual := manualNode{
		ID: "tls-node", Name: "tls-box", Address: "10.0.0.9",
		HostUUID: "peer", IdentityVerification: noderec.IdentityPinned,
		OllamaUp: true, NodeInfoUp: true, NodeInfoPort: 14319,
	}
	v.Update(manualNodesMsg{nodes: []manualNode{manual}})
	v.Update(discoveryPush(t, availableNode{
		HostUUID: "peer", DiscoveryRecordID: "peer:peer",
		IdentityVerification: noderec.IdentityPinned, Name: "tls-box",
		IPAddress: "10.0.0.9", Port: 14319, Trusted: true,
		Info: &noderec.NodeInfo{
			HostUUID: "peer", TelemetryValid: true,
			GPUs: []noderec.GPUInfo{{Name: "GPU 0", VramBytes: 1 << 30}},
		},
	}))
	v.openDetail()
	require.NotNil(t, v.detail)
	require.Contains(t, v.View(), "GPU 0", "the authenticated hardware must be visible before proof withdrawal")

	// The engine still answers, but node-info can no longer prove its identity.
	manual.IdentityVerification = noderec.IdentityUnverified
	manual.HostUUID = ""
	manual.NodeInfoUp = false
	v.Update(manualNodesMsg{nodes: []manualNode{manual}})
	v.Update(discoveryPush(t, availableNode{
		HostUUID: "manual:tls-node", DiscoveryRecordID: "manual:tls-node",
		IdentityVerification: noderec.IdentityUnverified, Name: "tls-box",
		IPAddress: "10.0.0.9", Port: 14319,
	}))

	assert.Equal(t, presenceOnline, v.detail.node.presence, "the engine still answers after node-info proof is withdrawn")
	assert.NotContains(t, v.View(), "GPU 0", "the old authenticated hardware must leave the open screen")
	assert.Contains(t, v.detail.hardwareBlock(), "unavailable")
}

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
	ctx, cancel := context.WithCancel(t.Context())
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
		HostUUID: "peer", DiscoveryRecordID: "peer:peer", IdentityVerification: noderec.IdentityPinned,
		Name: "peer", IPAddress: "10.0.0.2", Trusted: true,
	}}
	v.feeds.manual = []manualNode{{
		ID: "manual:10.0.0.2", Address: "10.0.0.2", HostUUID: "peer", IdentityVerification: noderec.IdentityPinned,
	}}
	v.rebuild()
	v.selectedKey = "peer"
	return v
}

// A host may remain in discovery after its manual configuration is deleted.
// The open detail must forget the removed handle when nodes/list updates,
// rather than retaining it until the detail screen is closed.
func TestNodesView_ForgetsRemovedManualHandleInOpenDetail(t *testing.T) {
	v := peerAddedByAddress(nil)
	v.SetSize(100, 30)
	v.openDetail()
	require.NotNil(t, v.detail)
	require.Equal(t, []string{"manual:10.0.0.2"}, v.detail.node.manualIDs)

	v.Update(manualNodesMsg{nodes: []manualNode{}})

	assert.Equal(t, "peer", v.detail.node.key)
	assert.Empty(t, v.detail.node.manualIDs)
	assert.Equal(t, membershipMember, v.detail.node.membership)
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
		manualNode{ID: "manual:peer.lan", Address: "peer.lan", HostUUID: "peer", IdentityVerification: noderec.IdentityPinned})
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
