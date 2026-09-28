// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestRelayToEngineKeepsDownloadRequestsInOrder(t *testing.T) {
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
			if got != want.method {
				t.Fatalf("engine-manager received %s, want %s", got, want.method)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("engine-manager never received %s", want.method)
		}
	}
}
