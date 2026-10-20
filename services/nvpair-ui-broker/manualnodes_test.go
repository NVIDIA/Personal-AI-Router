// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
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
