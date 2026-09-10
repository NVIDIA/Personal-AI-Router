// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistManualNodeRoundTripsAndRemoves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	b := &Broker{manualNodesConfigPath: path}

	if err := b.persistManualNode(persistedManualNode{ID: "lab", Address: "node.local", Name: "lab", TLSPort: 14319, MTLS: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "lab" || entries[0].TLSPort != 14319 || !entries[0].MTLS {
		t.Fatalf("persisted entries = %+v", entries)
	}

	if err := b.removePersistedManualNode("lab"); err != nil {
		t.Fatal(err)
	}
	entries, err = loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries after remove = %+v, want empty", entries)
	}
}

func TestReplayPersistedManualNodesIntoWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{ID: "lab", Address: "node.local", Name: "lab"}}); err != nil {
		t.Fatal(err)
	}

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
		if entry.Address != "node.local" || entry.Name != "lab" {
			t.Fatalf("replayed entry = %+v", entry)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("persisted manual node was not replayed")
	}
}

func TestManualAliasForOperationalKey(t *testing.T) {
	b := &Broker{manualNodeKeys: map[string]string{"second": "node-uuid", "first": "node-uuid"}}
	if got := b.manualAliasForKey("node-uuid"); got != "first" {
		t.Fatalf("alias = %q, want first", got)
	}
	if got := b.manualAliasForKey("second"); got != "second" {
		t.Fatalf("direct alias = %q, want second", got)
	}
}

func TestManualNodeRelayPersistsSuccessfulAdd(t *testing.T) {
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
	if err != nil || response.Error != nil {
		t.Fatalf("add response error = %v, frame = %+v", err, response)
	}

	entries, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "manual:node.local" || entries[0].Address != "node.local" {
		t.Fatalf("persisted add = %+v", entries)
	}
}

func TestManualNodeRelayRemovesByOperationalKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs", "manual-nodes.json")
	if err := writePersistedManualNodes(path, []persistedManualNode{{ID: "lab", Address: "node.local", Name: "lab"}}); err != nil {
		t.Fatal(err)
	}
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
		Params:  json.RawMessage(`{"id":"node-uuid"}`),
	})
	response, err := NewCodec(clientSide).Read()
	if err != nil || response.Error != nil {
		t.Fatalf("remove response error = %v, frame = %+v", err, response)
	}
	if got := <-seenID; got != "lab" {
		t.Fatalf("worker remove id = %q, want lab", got)
	}
	entries, err := loadPersistedManualNodes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("persisted entries after remove = %+v", entries)
	}
}
