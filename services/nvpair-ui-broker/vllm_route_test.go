// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"nvpair-shared/engines"
)

// Routing is proven only when the local proxy would try this node for the
// model; another owner advertising it is not this coordinator's route.
func TestVLLMGroupRouteRequiresThisNodeAmongProxyModelOwners(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owners string
		want   bool
	}{
		{"listed", `{"nodes":["node-b","node-self"]}`, true},
		{"other owner only", `{"nodes":["node-b"]}`, false},
		{"no owner", `{"nodes":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxyClient, proxyServer := net.Pipe()
			defer proxyClient.Close()
			defer proxyServer.Close()
			proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient)), facadeState: readyFacade(vllmProxyProfile.Name, 8003)}
			go proxy.peer.Serve(nil, nil)
			asked := make(chan string, 1)
			go func() {
				codec := NewCodec(proxyServer)
				msg, err := codec.Read()
				if err != nil {
					return
				}
				var params struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				asked <- msg.Method + " " + params.Model
				_ = codec.Respond(msg.ID, json.RawMessage(tc.owners))
			}()
			b := &Broker{nodeID: "node-self"}
			b.setEngineProxyHandle(vllmProxyProfile, proxy)
			if got := b.localVLLMModelOwner("example/model"); got != tc.want {
				t.Fatalf("routable = %v, want %v", got, tc.want)
			}
			select {
			case got := <-asked:
				if got != engines.AddressMethod(vllmProxyProfile.Name, "node/model-owners")+" example/model" {
					t.Fatalf("proxy query = %q", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the proxy was not asked for the model's owners")
			}
		})
	}
	if (&Broker{nodeID: "node-self"}).localVLLMModelOwner("example/model") {
		t.Fatal("a route was reported without a supervised proxy")
	}
}

func TestVLLMGroupRouteAnswersOnlyTheCurrentEngineManager(t *testing.T) {
	var oldOutput, currentOutput bytes.Buffer
	old := &rpcWorker{peer: NewPeer(NewCodec(&oldOutput))}
	current := &rpcWorker{peer: NewPeer(NewCodec(&currentOutput))}
	b := &Broker{}
	b.setEngineMgr(old)
	b.setEngineMgr(current)
	b.replyVLLMGroupRoute(old, json.RawMessage(`{"requestId":"`+strings.Repeat("a", 32)+`","model":"example/model"}`))
	for _, malformed := range []string{`{"requestId":"short","model":"example/model"}`, `{"requestId":"` + strings.Repeat("b", 32) + `","model":""}`, `not json`} {
		b.replyVLLMGroupRoute(current, json.RawMessage(malformed))
	}
	if oldOutput.Len() != 0 || currentOutput.Len() != 0 {
		t.Fatal("a replaced worker or a malformed check received a route reply")
	}
}
