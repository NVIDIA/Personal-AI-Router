// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A client that restarted, or did not start an operation, must still find the
// applied addresses it has to roll back; closed and refused records stay out.
func TestFabricRetainedOperationsListOnlyUnconfirmedNewestFirst(t *testing.T) {
	m := &Manager{exec: &Executor{baseDir: t.TempDir()}}
	record := func(id, state string, created int64, confirmed bool) *fabricRunRecord {
		return &fabricRunRecord{Public: fabricOperation{SchemaVersion: 1, OperationID: strings.Repeat(id, 32), State: state, CreatedAt: created, CleanupConfirmed: confirmed}}
	}
	m.exec.fabric = &fabricService{m: m, runs: map[string]*fabricRunRecord{
		"active":    record("a", "active", 2, false),
		"completed": record("b", "completed", 4, true),
		"recovery":  record("c", "recovery-required", 3, false),
		"refused":   record("d", "not-started", 5, false),
	}}
	for _, params := range []string{`{}`, `{"operationId":"injected"}`, `{"administratorApproved":true}`} {
		t.Run(params, func(t *testing.T) {
			var output bytes.Buffer
			m.codec = NewCodec(&output)
			id := json.RawMessage(`1`)
			m.handleFabric(context.Background(), &Message{ID: &id, Method: "engine:fabric-retained-operations", Params: json.RawMessage(params)})
			var response struct {
				Result *fabricRetainedOperations `json:"result"`
				Error  *RPCError                 `json:"error"`
			}
			if json.Unmarshal(output.Bytes(), &response) != nil {
				t.Fatal("malformed retained fabric reply")
			}
			if params != `{}` {
				if response.Error == nil || response.Result != nil {
					t.Fatal("retained fabric discovery admitted action input")
				}
				return
			}
			if response.Error != nil || response.Result == nil || len(response.Result.Operations) != 2 ||
				response.Result.Operations[0].State != "recovery-required" || response.Result.Operations[1].State != "active" {
				t.Fatalf("retained fabric discovery listed the wrong operations: %+v", response)
			}
		})
	}
	m.exec.fabric.recoveryFailed = true
	if _, err := m.exec.fabric.retainedOperations(); err == nil {
		t.Fatal("incomplete retained history was reported as complete")
	}
}
