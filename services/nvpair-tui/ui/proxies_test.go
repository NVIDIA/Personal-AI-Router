// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"testing"

	"nvpair-tui/rpc"
)

func TestProxiesViewIncludesLlamaCpp(t *testing.T) {
	v := newProxiesView(nil)
	if len(v.engines) != 3 {
		t.Fatalf("engines = %d, want 3", len(v.engines))
	}
	got := v.engines[2]
	if got.label != "llama.cpp" || got.prefix != "llamacpp-proxy" {
		t.Fatalf("engine[2] = {%q, %q}, want {llama.cpp, llamacpp-proxy}", got.label, got.prefix)
	}
}

func TestHandleNotificationLlamaCppProxy(t *testing.T) {
	v := newProxiesView(nil)
	params, err := json.Marshal(map[string]int{"port": 8084})
	if err != nil {
		t.Fatal(err)
	}
	v.handleNotification(&rpc.Message{
		Method: "llamacpp-proxy:ready",
		Params: params,
	})
	e := v.engines[2]
	if !e.ready || e.port != 8084 {
		t.Fatalf("llamacpp engine ready=%v port=%d, want ready :8084", e.ready, e.port)
	}
	if v.engines[0].ready {
		t.Fatal("ollama proxy must not consume llamacpp-proxy notifications")
	}
}
