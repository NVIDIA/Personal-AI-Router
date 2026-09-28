// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"nvpair-shared/engines"
)

// node/model-owners answers with exactly the nodes a model-bearing inference
// request would try. This node counts only once the broker has handed the
// facade its local backend, and unpinned relay peers never count.
func TestModelOwnersReportsInferenceCandidatesForOneModel(t *testing.T) {
	p := twoFacadeProxy(t)
	engine := engines.All()[0]
	f := p.facadeFor(engine.Name)
	f.httpMu.Lock()
	selfPort := f.port
	f.httpMu.Unlock()
	f.discovery.SetSubscribed([]Node{
		{ID: "node-self", Host: "127.0.0.1", Port: selfPort, Addresses: []string{"127.0.0.1"}, Models: []string{"example/model"}},
		{ID: "node-relay", Host: "10.0.0.3", Port: 8003, Addresses: []string{"10.0.0.3"}, Models: []string{"example/model"}},
		{ID: "node-other", Host: "10.0.0.4", Port: 8003, Addresses: []string{"10.0.0.4"}, Models: []string{"other/model"}},
	})
	var output bytes.Buffer
	p.codec = NewCodec(&output)
	owners := func(params string) (ModelOwnersResult, *RPCError) {
		t.Helper()
		output.Reset()
		id := json.RawMessage(`3`)
		p.handleMessage(&Message{Method: engines.AddressMethod(engine.Name, "node/model-owners"), Params: json.RawMessage(params), ID: &id})
		var reply Message
		if err := json.Unmarshal(output.Bytes(), &reply); err != nil {
			t.Fatalf("no model-owners reply: %v", err)
		}
		var result ModelOwnersResult
		if reply.Error == nil && json.Unmarshal(reply.Result, &result) != nil {
			t.Fatalf("malformed model-owners result: %s", output.String())
		}
		return result, reply.Error
	}
	if got, err := owners(`{"model":"example/model"}`); err != nil || got.Nodes == nil || len(got.Nodes) != 0 {
		t.Fatalf("an owner was reported before the local backend existed: %+v %v", got, err)
	}
	if err := f.setLocalBackend(localBackend{Engine: engine.Name, Host: "127.0.0.1", Port: 9, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := owners(`{"model":"example/model"}`); err != nil || !slices.Equal(got.Nodes, []string{"node-self"}) {
		t.Fatalf("owners = %+v %v", got, err)
	}
	if got, err := owners(`{"model":"absent/model"}`); err != nil || len(got.Nodes) != 0 {
		t.Fatalf("an unadvertised model reported owners: %+v %v", got, err)
	}
	if _, err := owners(`{}`); err == nil {
		t.Fatal("a model-less owner query was answered")
	}
}
