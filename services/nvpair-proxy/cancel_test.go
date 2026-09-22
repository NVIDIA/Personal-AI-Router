// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"nvpair-shared/engines"
)

// Exercise only the control dispatcher; no facade or listener is started.
func TestWorkloadCancelIsNotAProxyAPI(t *testing.T) {
	methods := []string{"workload/cancel"}
	for _, engine := range engines.All() {
		methods = append(methods, engines.AddressMethod(engine.Name, "workload/cancel"))
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			rec := &recordingWriter{}
			p := NewProxy(NewCodec(rec))
			id := json.RawMessage(`1`)
			p.handleMessage(&Message{ID: &id, Method: method, Params: json.RawMessage(`{"id":"1","runId":"a"}`)})
			lines := rec.lines()
			if len(lines) != 1 {
				t.Fatalf("responses = %d, want one method-not-found response", len(lines))
			}
			var response Message
			if json.Unmarshal(lines[0], &response) != nil || response.Error == nil || response.Error.Code != -32601 {
				t.Fatalf("removed cancellation API did not return method not found: %s", lines[0])
			}
		})
	}
}
