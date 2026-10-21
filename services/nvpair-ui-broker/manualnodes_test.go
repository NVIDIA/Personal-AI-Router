// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/noderec"
)

// Two entries deliberately overlap: "lab" has broker key "manual:lab", while
// "manual:lab" is a different entry with broker key "manual:manual:lab". Each
// case sends a removal through the broker to a fake worker that acknowledges it.
// The assertions check that the worker receives the intended exact entry ID
// and that persistence removes only that entry, leaving the other one intact.
// This covers both the desktop's nodeId selector and the TUI's id selector.
func TestManualNodeRelay_DistinguishesEntryIDsFromNodeKeys(t *testing.T) {
	const entryID = "lab"
	const prefixedEntryID = "manual:lab"
	const prefixedNodeID = "manual:manual:lab"
	type removalRequest struct {
		ID     string `json:"id,omitempty"`
		NodeID string `json:"nodeId,omitempty"`
	}
	type workerObservation struct {
		request removalRequest
		err     error
	}
	test := func(name string, request removalRequest, wantRemovedID, wantRemainingID string) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manual-nodes.json")
			require.NoError(t, writePersistedManualNodes(path, []persistedManualNode{
				{ID: entryID, Name: entryID, Address: "first.local"},
				{ID: prefixedEntryID, Name: prefixedEntryID, Address: "second.local"},
			}))
			clientBroker, clientSide := net.Pipe()
			workerBroker, workerSide := net.Pipe()
			t.Cleanup(func() {
				assert.NoError(t, clientBroker.Close())
				assert.NoError(t, clientSide.Close())
				assert.NoError(t, workerBroker.Close())
				assert.NoError(t, workerSide.Close())
			})
			require.NoError(t, clientSide.SetReadDeadline(time.Now().Add(2*time.Second)))
			require.NoError(t, workerSide.SetReadDeadline(time.Now().Add(2*time.Second)))
			worker := &rpcWorker{peer: NewPeer(NewCodec(workerBroker))}
			go worker.peer.Serve(nil, nil)
			observed := make(chan workerObservation, 1)
			go func() {
				codec := NewCodec(workerSide)
				message, err := codec.Read()
				if err != nil {
					observed <- workerObservation{err: err}
					return
				}
				var forwarded removalRequest
				if err := json.Unmarshal(message.Params, &forwarded); err != nil {
					observed <- workerObservation{err: err}
					return
				}
				err = codec.Respond(message.ID, map[string]bool{"removed": true})
				observed <- workerObservation{request: forwarded, err: err}
			}()
			broker := &Broker{
				codec:                 NewCodec(clientBroker),
				manualNodesConfigPath: path,
				manualNodeKeys: map[string]string{
					entryID:         prefixedEntryID,
					prefixedEntryID: prefixedNodeID,
				},
			}
			broker.setManualNodes(worker)
			params, err := json.Marshal(request)
			require.NoError(t, err)
			id := json.RawMessage("1")

			go broker.relayToManualNodes(&Message{
				JSONRPC: "2.0",
				ID:      &id,
				Method:  "node/remove",
				Params:  params,
			})
			response, err := NewCodec(clientSide).Read()
			require.NoError(t, err)
			require.Nil(t, response.Error)
			observation := <-observed
			require.NoError(t, observation.err)
			assert.Equal(t, wantRemovedID, observation.request.ID)
			assert.Empty(t, observation.request.NodeID, "the worker receives only its entry ID")
			entries, err := loadPersistedManualNodes(path)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, wantRemainingID, entries[0].ID)
		})
	}
	test("node key overlaps another entry ID", removalRequest{NodeID: prefixedEntryID}, entryID, prefixedEntryID)
	test("prefixed node key", removalRequest{NodeID: prefixedNodeID}, prefixedEntryID, entryID)
	test("entry ID overlaps another node key", removalRequest{ID: prefixedEntryID}, prefixedEntryID, entryID)
	test("plain entry ID", removalRequest{ID: entryID}, entryID, prefixedEntryID)
}

func TestManualNodeRelay_LeavesUnknownNodeKeysUnchanged(t *testing.T) {
	const entryID = "lab"
	path := filepath.Join(t.TempDir(), "manual-nodes.json")
	entries := []persistedManualNode{
		{ID: entryID, Name: entryID, Address: "node.local"},
	}
	require.NoError(t, writePersistedManualNodes(path, entries))
	var clientMessages bytes.Buffer
	var workerRequests bytes.Buffer
	broker := &Broker{
		codec:                 NewCodec(&clientMessages),
		manualNodesConfigPath: path,
		manualNodeKeys:        map[string]string{entryID: "manual:lab"},
	}
	broker.setManualNodes(&rpcWorker{peer: NewPeer(NewCodec(&workerRequests))})
	id := json.RawMessage("1")

	// This is an existing entry ID, but it is not a broker node key.
	broker.relayToManualNodes(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/remove",
		Params:  json.RawMessage(`{"nodeId":"lab"}`),
	})

	response, err := broker.codec.Read()
	require.NoError(t, err)
	require.Nil(t, response.Error)
	type removalResult struct {
		Removed bool `json:"removed"`
	}
	var result removalResult
	require.NoError(t, json.Unmarshal(response.Result, &result))
	assert.False(t, result.Removed)
	assert.Empty(t, workerRequests.String(), "an unknown node key must not reach the worker")
	saved, err := loadPersistedManualNodes(path)
	require.NoError(t, err)
	assert.Equal(t, entries, saved)
}

func TestManualNodeRelay_RejectsAmbiguousOrMissingRemovalSelectors(t *testing.T) {
	test := func(name, params string) {
		t.Run(name, func(t *testing.T) {
			var clientMessages bytes.Buffer
			var workerRequests bytes.Buffer
			broker := &Broker{codec: NewCodec(&clientMessages)}
			broker.setManualNodes(&rpcWorker{peer: NewPeer(NewCodec(&workerRequests))})
			id := json.RawMessage("1")

			broker.relayToManualNodes(&Message{
				JSONRPC: "2.0",
				ID:      &id,
				Method:  "node/remove",
				Params:  json.RawMessage(params),
			})

			response, err := broker.codec.Read()
			require.NoError(t, err)
			require.NotNil(t, response.Error)
			assert.Equal(t, -32602, response.Error.Code)
			assert.Empty(t, workerRequests.String(), "invalid selectors must not reach the worker")
		})
	}
	test("both selectors", `{"id":"lab","nodeId":"manual:lab"}`)
	test("missing selectors", `{}`)
	test("empty entry ID", `{"id":""}`)
	test("empty node key", `{"nodeId":""}`)
	test("null parameters", `null`)
	test("non-string selector", `{"id":123}`)
}

func TestManualAliasForKey_ResolvesOnlyNodeKeys(t *testing.T) {
	const hostUUID = "node-uuid"
	const entryID = "lab"
	const prefixedEntryID = "manual:lab"
	const prefixedNodeID = "manual:manual:lab"
	broker := &Broker{
		manualNodeKeys: map[string]string{
			"second":        hostUUID,
			"first":         hostUUID,
			entryID:         prefixedEntryID,
			prefixedEntryID: prefixedNodeID,
		},
	}
	test := func(name, key, wantID string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, wantID, broker.manualAliasForKey(key))
		})
	}
	test("shared node key resolves deterministically", hostUUID, "first")
	test("node key overlaps another entry ID", prefixedEntryID, entryID)
	test("prefixed node key", prefixedNodeID, prefixedEntryID)
	test("entry ID is not a node key", entryID, "")
	test("unknown node key", "missing", "")
}

func TestManualToEnriched_ExcludesUnverifiedHardwareFromServiceRecords(t *testing.T) {
	const modelName = "explicit-engine-model"
	status := manualNodeStatus{
		ID:                   "configured",
		HostUUID:             "claimed-peer",
		IdentityVerification: noderec.IdentityUnverified,
		GPUs:                 []GPUInfo{{Name: "public GPU"}},
		CPU:                  &CPUInfo{Name: "public CPU"},
		Memory:               &MemoryInfo{TotalBytes: 1024},
		OllamaModels:         []string{modelName},
	}

	node := manualToEnriched(status)

	assert.Empty(t, node.GPUs)
	assert.Nil(t, node.CPU)
	assert.Nil(t, node.Memory)
	assert.Equal(t, "manual:configured", node.HostUUID)
	assert.Equal(t, []string{modelName}, node.Models)
}

type observedProxyWriter struct {
	net.Conn
	writing chan struct{}
	once    sync.Once
}

func (w *observedProxyWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	return w.Conn.Write(data)
}

// A proxy that stops reading or answering must not hold up local hardware
// updates or prevent the broker from accepting the manual target's removal.
func TestManualProxyBridge_DoesNotBlockDiscovery(t *testing.T) {
	test := func(name string, blockWrite bool) {
		t.Run(name, func(t *testing.T) {
			brokerSide, proxySide := net.Pipe()
			t.Cleanup(func() {
				assert.NoError(t, brokerSide.Close())
				assert.NoError(t, proxySide.Close())
			})
			require.NoError(t, proxySide.SetDeadline(time.Now().Add(5*time.Second)))
			writer := &observedProxyWriter{Conn: brokerSide, writing: make(chan struct{})}
			proxy := &proxyProcess{peer: NewPeer(NewCodec(writer))}
			go proxy.peer.Serve(nil, nil)
			b := newManualTestBroker()
			b.nodeID = "self"
			b.setProxy(proxy)
			codec := NewCodec(proxySide)
			added := make(chan struct{})
			go func() {
				b.forwardManualNodesNotification("node/discovered", json.RawMessage(`{"id":"lab","address":"192.0.2.20","ollama_up":true,"ollama_port":11434}`))
				close(added)
			}()
			select {
			case <-writer.writing:
			case <-time.After(2 * time.Second):
				require.FailNow(t, "manual target was not sent to the proxy")
			}
			var first *Message
			if !blockWrite {
				var err error
				first, err = codec.Read()
				require.NoError(t, err)
			}

			owned := make(chan struct{})
			go func() {
				b.nodeInfoNotificationHandler(0)(noderec.NotifyInfoUpdated, json.RawMessage(`{"info":{"hostUuid":"self"}}`))
				close(owned)
			}()
			removed := make(chan struct{})
			go func() {
				b.forwardManualNodesNotification("node/removed", json.RawMessage(`{"id":"lab"}`))
				close(removed)
			}()
			finished := func(done <-chan struct{}, operation string) {
				t.Helper()
				select {
				case <-done:
				case <-time.After(time.Second):
					assert.Fail(t, operation+" waited for the stalled proxy")
				}
			}
			finished(owned, "owned hardware update")
			finished(removed, "manual target removal")

			if first == nil {
				var err error
				first, err = codec.Read()
				require.NoError(t, err)
			}
			require.Equal(t, "ollama:node/add-manual", first.Method)
			require.NoError(t, codec.Respond(first.ID, map[string]bool{"added": true}))
			withdrawal, err := codec.Read()
			require.NoError(t, err)
			assert.Equal(t, "ollama:node/remove-manual", withdrawal.Method)
			var params struct {
				ID string `json:"id"`
			}
			require.NoError(t, json.Unmarshal(withdrawal.Params, &params))
			assert.Equal(t, "manual:lab", params.ID)
			require.NoError(t, codec.Respond(withdrawal.ID, map[string]bool{"removed": true}))
			finished(added, "initial manual update")
			finished(removed, "manual target removal after proxy recovery")
			finished(owned, "owned hardware update after proxy recovery")
			waitForManualProxySender(t, b)
		})
	}
	test("delayed reply", false)
	test("blocked pipe write", true)
}

func manualProxyTestPeer(t *testing.T, b *Broker) (*Codec, net.Conn) {
	t.Helper()
	brokerSide, proxySide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, brokerSide.Close())
		assert.NoError(t, proxySide.Close())
	})
	require.NoError(t, proxySide.SetDeadline(time.Now().Add(3*time.Second)))
	proxy := &proxyProcess{peer: NewPeer(NewCodec(brokerSide))}
	go proxy.peer.Serve(nil, nil)
	b.setProxy(proxy)
	return NewCodec(proxySide), brokerSide
}

func waitForManualProxySender(t *testing.T, b *Broker) {
	t.Helper()
	bridge := &b.ollamaState().manualBridge
	// Joining the sender also proves that it has no unexpected extra request
	// waiting for a reply, without asserting an exact goroutine schedule.
	require.Eventually(t, func() bool {
		bridge.mu.Lock()
		defer bridge.mu.Unlock()
		return !bridge.sending
	}, time.Second, time.Millisecond)
}

// While one add waits for its reply, intermediate probes and aliases can change.
// Recovery must withdraw the retired key before adding only the latest target.
func TestManualProxyBridge_WithdrawsOldKeyBeforeSendingLatestTarget(t *testing.T) {
	b := newManualTestBroker()
	codec, _ := manualProxyTestPeer(t, b)
	first := manualStatus("first", "192.0.2.20", "old-peer")
	first.OllamaUp, first.OllamaPort = true, 11434
	b.upsertManualNode(first)
	initial, err := codec.Read()
	require.NoError(t, err)
	require.Equal(t, "ollama:node/add-manual", initial.Method)
	second := first
	second.ID, second.Address = "second", "192.0.2.21"
	b.upsertManualNode(second)
	for i := range 20 {
		b.upsertManualNode(first)
		// Configurations canceled before a send must not accumulate removals.
		transient := manualNodeStatus{ID: fmt.Sprintf("transient-%d", i), Address: "192.0.2.40", OllamaUp: true, OllamaPort: 11434}
		b.upsertManualNode(transient)
		b.removeManualNode(transient.ID)
	}
	first.HostUUID, first.Address = "new-peer", "192.0.2.30"
	first.OllamaModels = []string{"latest-model"}
	b.upsertManualNode(first)
	b.removeManualNode(second.ID)
	require.NoError(t, codec.Respond(initial.ID, map[string]bool{"added": true}))

	withdrawal, err := codec.Read()
	require.NoError(t, err)
	require.Equal(t, "ollama:node/remove-manual", withdrawal.Method)
	var removed struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(withdrawal.Params, &removed))
	assert.Equal(t, "old-peer", removed.ID)
	require.NoError(t, codec.Respond(withdrawal.ID, map[string]bool{"removed": true}))
	addition, err := codec.Read()
	require.NoError(t, err)
	require.Equal(t, "ollama:node/add-manual", addition.Method)
	var target proxyManualNode
	require.NoError(t, json.Unmarshal(addition.Params, &target))
	assert.Equal(t, "new-peer", target.ID)
	assert.Equal(t, "192.0.2.30", target.Host)
	assert.Equal(t, []string{"latest-model"}, target.Models)
	require.NoError(t, codec.Respond(addition.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
}

// The removed alias may have supplied the projected address. Its surviving
// alias must replace that address even when an older add is still in flight.
func TestManualProxyBridge_UsesSurvivingAliasAfterRemoval(t *testing.T) {
	b := newManualTestBroker()
	codec, _ := manualProxyTestPeer(t, b)
	first := manualStatus("first", "192.0.2.20", "peer")
	first.OllamaUp, first.OllamaPort = true, 11434
	b.upsertManualNode(first)
	initial, err := codec.Read()
	require.NoError(t, err)
	second := first
	second.ID, second.Address = "second", "192.0.2.21"
	b.upsertManualNode(second)
	b.removeManualNode(first.ID)
	require.NoError(t, codec.Respond(initial.ID, map[string]bool{"added": true}))

	addition, err := codec.Read()
	require.NoError(t, err)
	require.Equal(t, "ollama:node/add-manual", addition.Method)
	var target proxyManualNode
	require.NoError(t, json.Unmarshal(addition.Params, &target))
	assert.Equal(t, "peer", target.ID)
	assert.Equal(t, "192.0.2.21", target.Host)
	require.NoError(t, codec.Respond(addition.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
}

// A replacement proxy starts empty. It must receive the current configuration,
// including changes accepted while the retired proxy was not answering.
func TestManualProxyBridge_ReplaysLatestTargetAfterProxyRestart(t *testing.T) {
	b := newManualTestBroker()
	oldCodec, oldConnection := manualProxyTestPeer(t, b)
	status := manualNodeStatus{ID: "lab", Address: "192.0.2.20", OllamaUp: true, OllamaPort: 11434}
	b.upsertManualNode(status)
	_, err := oldCodec.Read()
	require.NoError(t, err)
	status.Address = "192.0.2.30"
	b.upsertManualNode(status)
	newCodec, _ := manualProxyTestPeer(t, b)
	b.replayProxyStateAfterSpawn()
	require.NoError(t, oldConnection.Close())

	addition, err := newCodec.Read()
	require.NoError(t, err)
	require.Equal(t, "ollama:node/add-manual", addition.Method)
	var target proxyManualNode
	require.NoError(t, json.Unmarshal(addition.Params, &target))
	assert.Equal(t, "manual:lab", target.ID)
	assert.Equal(t, "192.0.2.30", target.Host)
	require.NoError(t, newCodec.Respond(addition.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
}

// A rejected bridge request is retried when the next probe reports the same
// reachable engine; remembering an attempted snapshot must not suppress it.
func TestManualProxyBridge_RetriesIdenticalProbeAfterFailure(t *testing.T) {
	b := newManualTestBroker()
	codec, _ := manualProxyTestPeer(t, b)
	status := manualNodeStatus{ID: "lab", Address: "192.0.2.20", OllamaUp: true, OllamaPort: 11434}
	b.upsertManualNode(status)
	first, err := codec.Read()
	require.NoError(t, err)
	require.NoError(t, codec.RespondError(first.ID, -32000, "facade not enabled"))
	waitForManualProxySender(t, b)

	b.upsertManualNode(status)
	retried, err := codec.Read()
	require.NoError(t, err)
	assert.Equal(t, "ollama:node/add-manual", retried.Method)
	var target proxyManualNode
	require.NoError(t, json.Unmarshal(retried.Params, &target))
	assert.Equal(t, proxyManualNode{ID: "manual:lab", Host: "192.0.2.20", Port: 11434, Addresses: []string{"192.0.2.20"}}, target)
	require.NoError(t, codec.Respond(retried.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
}

// If a live proxy rejects a removal, later unreachable probes must retry it;
// otherwise the old route survives even though the broker withdrew the target.
func TestManualProxyBridge_RetriesWithdrawalAfterFailure(t *testing.T) {
	b := newManualTestBroker()
	codec, _ := manualProxyTestPeer(t, b)
	status := manualNodeStatus{ID: "lab", Address: "192.0.2.20", OllamaUp: true, OllamaPort: 11434}
	b.upsertManualNode(status)
	addition, err := codec.Read()
	require.NoError(t, err)
	require.NoError(t, codec.Respond(addition.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
	status.OllamaUp = false
	b.upsertManualNode(status)
	withdrawal, err := codec.Read()
	require.NoError(t, err)
	require.Equal(t, "ollama:node/remove-manual", withdrawal.Method)
	require.NoError(t, codec.RespondError(withdrawal.ID, -32000, "temporary failure"))
	waitForManualProxySender(t, b)

	b.upsertManualNode(status)
	retried, err := codec.Read()
	require.NoError(t, err, "the next unreachable probe must retry withdrawal")
	assert.Equal(t, "ollama:node/remove-manual", retried.Method)
	var params struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(retried.Params, &params))
	assert.Equal(t, "manual:lab", params.ID)
	require.NoError(t, codec.Respond(retried.ID, map[string]bool{"removed": true}))
	waitForManualProxySender(t, b)
}

// No new probe is needed to repopulate an idle target after proxy restart: the
// spawn replay restores the last accepted configuration into the empty process.
func TestManualProxyBridge_ReplaysIdleTargetsAfterProxyRestart(t *testing.T) {
	b := newManualTestBroker()
	oldCodec, oldConnection := manualProxyTestPeer(t, b)
	b.upsertManualNode(manualNodeStatus{ID: "lab", Address: "192.0.2.20", OllamaUp: true, OllamaPort: 11434})
	initial, err := oldCodec.Read()
	require.NoError(t, err)
	require.NoError(t, oldCodec.Respond(initial.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
	require.NoError(t, oldConnection.Close())
	newCodec, _ := manualProxyTestPeer(t, b)

	b.replayProxyStateAfterSpawn()
	addition, err := newCodec.Read()
	require.NoError(t, err)
	assert.Equal(t, "ollama:node/add-manual", addition.Method)
	var target proxyManualNode
	require.NoError(t, json.Unmarshal(addition.Params, &target))
	assert.Equal(t, proxyManualNode{ID: "manual:lab", Host: "192.0.2.20", Port: 11434, Addresses: []string{"192.0.2.20"}}, target)
	require.NoError(t, newCodec.Respond(addition.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
}

// Teardown closes the child pipe. Once its in-flight call completes, a queued
// newer target must not start another exchange with a proxy being shut down.
func TestManualProxyBridge_StopsSendingWhenBrokerShutsDown(t *testing.T) {
	b := newManualTestBroker()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.setServeCtx(ctx)
	codec, _ := manualProxyTestPeer(t, b)
	status := manualNodeStatus{ID: "lab", Address: "192.0.2.20", OllamaUp: true, OllamaPort: 11434}
	b.upsertManualNode(status)
	initial, err := codec.Read()
	require.NoError(t, err)
	status.Address = "192.0.2.30"
	b.upsertManualNode(status)

	cancel()
	require.NoError(t, codec.Respond(initial.ID, map[string]bool{"added": true}))
	waitForManualProxySender(t, b)
}
