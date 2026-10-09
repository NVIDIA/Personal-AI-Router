// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLifecycleValid(t *testing.T) {
	params := json.RawMessage(`{"workloadInfo":{"id":"wl-1","model":"llama-3","engine":"trt-llm","state":"queued","originatedFrom":"node-A","createdAt":1,"startedAt":null,"completedAt":null,"error":null,"requesterId":null}}`)
	w, err := parseLifecycle(params)
	require.NoError(t, err)
	assert.Equal(t, "wl-1", w.ID)
	assert.Equal(t, "llama-3", w.Model)
	assert.Equal(t, "trt-llm", w.Engine)
}

func TestParseLifecycleRejectsMissingFields(t *testing.T) {
	test := func(name, body string) {
		t.Run(name, func(t *testing.T) {
			_, err := parseLifecycle(json.RawMessage(body))
			assert.Error(t, err)
		})
	}
	test("no workloadInfo", `{}`)
	test("empty id", `{"workloadInfo":{"id":"","model":"m","engine":"e","state":"queued","originatedFrom":"n"}}`)
	test("empty engine", `{"workloadInfo":{"id":"x","model":"m","engine":"","state":"queued","originatedFrom":"n"}}`)
	test("empty state", `{"workloadInfo":{"id":"x","model":"m","engine":"e","state":"","originatedFrom":"n"}}`)
	test("empty originatedFrom", `{"workloadInfo":{"id":"x","model":"m","engine":"e","state":"queued","originatedFrom":""}}`)
}

func TestParseRemove(t *testing.T) {
	id, node, err := parseRemove(json.RawMessage(`{"workloadId":"wl-9","originatedFrom":"node-C"}`))
	require.NoError(t, err)
	assert.Equal(t, "wl-9", id)
	assert.Equal(t, "node-C", node)
	// originatedFrom is optional (backward compatible): a legacy payload
	// without it still parses, with an empty originatedFrom.
	id, node, err = parseRemove(json.RawMessage(`{"workloadId":"wl-9"}`))
	require.NoError(t, err)
	assert.Equal(t, "wl-9", id)
	assert.Empty(t, node)
	_, _, err = parseRemove(json.RawMessage(`{"workloadId":""}`))
	require.Error(t, err, "empty workloadId")
}

func TestLifecycleMethodMapping(t *testing.T) {
	test := func(name, method string, want WorkloadState) {
		t.Run(name, func(t *testing.T) {
			assert.True(t, isLifecycleMethod(method), "must be a lifecycle method")
			assert.Equal(t, want, lifecycleMethods[method])
		})
	}
	test("submitted", MethodSubmitted, StateQueued)
	test("started", MethodStarted, StateRunning)
	test("completed", MethodCompleted, StateCompleted)
	test("errored", MethodErrored, StateFailed)
	assert.False(t, isLifecycleMethod(MethodRemove), "workloads:remove is not a lifecycle method")
	assert.False(t, isLifecycleMethod("bogus:method"), "unknown method should not be a lifecycle method")
}
