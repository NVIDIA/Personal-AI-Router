// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

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
