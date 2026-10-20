// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersistManualNode_RoundTripsAndRemoves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	b := &Broker{manualNodesConfigPath: path}

	require.NoError(t, b.persistManualNode(persistedManualNode{ID: "lab", Address: "node.local", Name: "lab", TLSPort: 14319, MTLS: true}))
	entries, err := loadPersistedManualNodes(path)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "lab", entries[0].ID)
	assert.Equal(t, 14319, entries[0].TLSPort)
	assert.True(t, entries[0].MTLS)

	require.NoError(t, b.removePersistedManualNode("lab"))
	entries, err = loadPersistedManualNodes(path)
	require.NoError(t, err)
	assert.Empty(t, entries, "entries after remove")
}

func TestReplayPersistedManualNodes_ResendsSavedNodesToWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	require.NoError(t, writePersistedManualNodes(path, []persistedManualNode{{ID: "lab", Address: "node.local", Name: "lab"}}))

	brokerSide, workerSide := net.Pipe()
	defer brokerSide.Close()
	defer workerSide.Close()
	worker := &rpcWorker{peer: NewPeer(NewCodec(brokerSide))}
	go worker.peer.Serve(nil, nil)

	got := make(chan persistedManualNode, 1)
	go func() {
		codec := NewCodec(workerSide)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var entry persistedManualNode
		if json.Unmarshal(msg.Params, &entry) == nil {
			got <- entry
		}
		_ = codec.Respond(msg.ID, map[string]string{"id": "lab"})
	}()

	b := &Broker{manualNodesConfigPath: path}
	b.replayPersistedManualNodes(worker)

	select {
	case entry := <-got:
		assert.Equal(t, "node.local", entry.Address)
		assert.Equal(t, "lab", entry.Name)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "persisted manual node was not replayed")
	}
}

func TestManualNodeRelay_PersistsSuccessfulAdd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	clientBroker, clientSide := net.Pipe()
	workerBroker, workerSide := net.Pipe()
	defer clientBroker.Close()
	defer clientSide.Close()
	defer workerBroker.Close()
	defer workerSide.Close()

	worker := &rpcWorker{peer: NewPeer(NewCodec(workerBroker))}
	go worker.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(workerSide)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		_ = codec.Respond(msg.ID, map[string]any{"id": "manual:node.local", "address": "node.local"})
	}()

	b := &Broker{codec: NewCodec(clientBroker), manualNodesConfigPath: path}
	b.setManualNodes(worker)
	id := json.RawMessage("1")
	b.relayToManualNodes(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/add",
		Params:  json.RawMessage(`{"address":"node.local"}`),
	})
	response, err := NewCodec(clientSide).Read()
	require.NoError(t, err, "read add response")
	require.Nil(t, response.Error, "add response")

	entries, err := loadPersistedManualNodes(path)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "manual:node.local", entries[0].ID)
	assert.Equal(t, "node.local", entries[0].Address)
}

func TestManualNodeRelay_RemovesByNodeKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	require.NoError(t, writePersistedManualNodes(path, []persistedManualNode{{ID: "lab", Address: "node.local", Name: "lab"}}))
	clientBroker, clientSide := net.Pipe()
	workerBroker, workerSide := net.Pipe()
	defer clientBroker.Close()
	defer clientSide.Close()
	defer workerBroker.Close()
	defer workerSide.Close()

	worker := &rpcWorker{peer: NewPeer(NewCodec(workerBroker))}
	go worker.peer.Serve(nil, nil)
	seenID := make(chan string, 1)
	go func() {
		codec := NewCodec(workerSide)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		var params struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		seenID <- params.ID
		_ = codec.Respond(msg.ID, map[string]bool{"removed": true})
	}()

	b := &Broker{
		codec:                 NewCodec(clientBroker),
		manualNodesConfigPath: path,
		manualNodeKeys:        map[string]string{"lab": "node-uuid"},
	}
	b.setManualNodes(worker)
	id := json.RawMessage("2")
	b.relayToManualNodes(&Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "node/remove",
		Params:  json.RawMessage(`{"nodeId":"node-uuid"}`),
	})
	response, err := NewCodec(clientSide).Read()
	require.NoError(t, err, "read remove response")
	require.Nil(t, response.Error, "remove response")
	assert.Equal(t, "lab", <-seenID, "worker remove id")
	entries, err := loadPersistedManualNodes(path)
	require.NoError(t, err)
	assert.Empty(t, entries, "persisted entries after remove")
}
