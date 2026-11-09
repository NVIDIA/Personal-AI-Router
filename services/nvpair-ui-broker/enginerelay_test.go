// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Engine requests are otherwise each relayed from a goroutine of their own,
// which can reach the engine manager in any order. A cancel that overtakes the
// pull it names finds nothing to stop, and the download then runs to
// completion under a UI that shows it cancelled.
func TestRelayToEngine_KeepsDownloadRequestsInOrder(t *testing.T) {
	engineClient, engineServer := net.Pipe()
	brokerClient, brokerServer := net.Pipe()
	defer engineClient.Close()
	defer engineServer.Close()
	defer brokerClient.Close()
	defer brokerServer.Close()

	engine := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go engine.peer.Serve(nil, nil)
	b := &Broker{codec: NewCodec(brokerClient)}
	b.setEngineMgr(engine)
	go func() {
		codec := NewCodec(brokerServer)
		for {
			if _, err := codec.Read(); err != nil {
				return
			}
		}
	}()

	received := make(chan string, 8)
	go func() {
		codec := NewCodec(engineServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			received <- msg.Method
		}
	}()

	sent := []struct {
		method string
		params string
	}{
		{"engine:action", `{"engine":"ollama","action":"pull_model","params":{"name":"m"}}`},
		{"engine:cancel-pull", `{"engine":"ollama","model":"m"}`},
		{"engine:remote-pull-model", `{"node":"n","engine":"ollama","model":"m"}`},
		{"engine:remote-cancel-pull", `{"node":"n","engine":"ollama","model":"m"}`},
	}
	for i, request := range sent {
		id := json.RawMessage(`"` + string(rune('a'+i)) + `"`)
		b.relayToEngine(&Message{JSONRPC: "2.0", ID: &id, Method: request.method, Params: json.RawMessage(request.params)})
	}

	for _, want := range sent {
		select {
		case got := <-received:
			require.Equal(t, want.method, got, "the engine manager received the download requests out of order")
		case <-time.After(2 * time.Second):
			require.FailNow(t, "the engine manager never received "+want.method)
		}
	}
}
