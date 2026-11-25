// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Custom's model inventory never comes from a loopback sweep — Custom
// endpoints are user-configured external URLs with no manifest and no local
// process. The broker pushes the current set over the local stdio link
// (custom:set-models) and the executor merges it into the same ModelsResult
// the /v1/models surface and engine:models RPC return, so a peer's model
// fetch attributes custom models without learning about the credential that
// may gate the endpoint. Key presence is the semantic: ByEngine["custom"]
// exists exactly while a non-empty set is configured, matching the
// documented "missing key = not running / not queryable" distinction.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetCustomModels_MergesIntoModelsResultWithoutAnEngineSweep(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())

	got := ex.ModelsResult(context.Background())
	require.NotContains(t, got.ByEngine, "custom", "no custom configuration means the key is absent, not empty")

	ex.SetCustomModels([]string{"external/model-a", "shared/model"})
	got = ex.ModelsResult(context.Background())
	require.Equal(t, []string{"external/model-a", "shared/model"}, got.ByEngine["custom"], "ByEngine[custom]")
	require.Contains(t, got.Models, "external/model-a", "the flat union carries the custom set for union-only consumers")

	ex.SetCustomModels(nil)
	got = ex.ModelsResult(context.Background())
	require.NotContains(t, got.ByEngine, "custom", "an empty set removes the key; it does not publish an empty list")
	require.NotContains(t, got.Models, "external/model-a", "the flat union must drop the cleared set")
}

// The dispatch half of the path: the broker's push arrives as a JSON-RPC
// notification on the same link that carries engine:restore-enabled. Without
// an explicit branch BEFORE the !IsRequest fallback, handleMessage would just
// log "ignoring incoming notification" and the push would vanish silently.
func TestCustomSetModels_NotificationDispatchUpdatesState(t *testing.T) {
	var out bytes.Buffer
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, t.TempDir())
	m := NewManager(NewCodec(&out), ex, nil)

	m.handleMessage(context.Background(), &Message{
		JSONRPC: "2.0",
		Method:  "custom:set-models",
		Params:  json.RawMessage(`{"models":["external/model-a"]}`),
	})
	require.Equal(t, []string{"external/model-a"}, ex.CustomModels(),
		"a custom:set-models notification must reach executor state, not the ignored-notification fallback")

	m.handleMessage(context.Background(), &Message{
		JSONRPC: "2.0",
		Method:  "custom:set-models",
		Params:  json.RawMessage(`{"models":"not-a-list"}`),
	})
	require.Equal(t, []string{"external/model-a"}, ex.CustomModels(),
		"a malformed push must not rewrite the inventory")
}
