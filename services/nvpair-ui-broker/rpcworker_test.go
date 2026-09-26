// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"nvpair-shared/enginelogs"
)

func TestEngineManagerWorkerReadsNearLimitLogsAndNextResponse(t *testing.T) {
	brokerSide, engineSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = engineSide.Close()
	})

	peer := NewPeer(newRPCWorkerCodec(engineManagerWorkerName, brokerSide))
	go peer.Serve(nil, nil)
	worker := &rpcWorker{peer: peer}

	textBytes := enginelogs.MaxLineBytes - 1024
	lines := make([]map[string]string, 4)
	for index := range lines {
		lines[index] = map[string]string{
			"time":   fmt.Sprintf("03:54:45.%03d", 481+index),
			"stream": "stderr",
			"text":   strings.Repeat("&", textBytes),
		}
	}
	logs := map[string]any{"lines": lines}
	encoded, err := json.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	if rawTextBytes := textBytes * len(lines); rawTextBytes > enginelogs.MaxSnapshotTextBytes || rawTextBytes < enginelogs.MaxSnapshotTextBytes-(8<<10) {
		t.Fatalf("near-limit raw log text bytes = %d", rawTextBytes)
	}
	if len(encoded) <= 1<<20 || len(encoded) >= enginelogs.MaxBrokerFrameBytes {
		t.Fatalf("escaped log frame bytes = %d, want (1 MiB, %d)", len(encoded), enginelogs.MaxBrokerFrameBytes)
	}

	serverDone := make(chan error, 1)
	go func() {
		codec := NewCodec(engineSide)
		for _, expected := range []string{"engine:logs", "engine:status"} {
			request, readErr := codec.Read()
			if readErr != nil {
				serverDone <- readErr
				return
			}
			if request.Method != expected {
				serverDone <- fmt.Errorf("method = %q, want %q", request.Method, expected)
				return
			}
			if expected == "engine:logs" {
				readErr = codec.Respond(request.ID, logs)
			} else {
				readErr = codec.Respond(request.ID, map[string]any{"running": true})
			}
			if readErr != nil {
				serverDone <- readErr
				return
			}
		}
		serverDone <- nil
	}()

	result, rpcErr, err := worker.Call(context.Background(), "engine:logs", nil)
	if err != nil || rpcErr != nil {
		t.Fatalf("near-limit engine:logs failed: rpc=%v err=%v", rpcErr, err)
	}
	var snapshot struct {
		Lines []json.RawMessage `json:"lines"`
	}
	if err := json.Unmarshal(result, &snapshot); err != nil || len(snapshot.Lines) != len(lines) {
		t.Fatalf("decode engine:logs: lines=%d err=%v", len(snapshot.Lines), err)
	}

	result, rpcErr, err = worker.Call(context.Background(), "engine:status", nil)
	if err != nil || rpcErr != nil || !strings.Contains(string(result), `"running":true`) {
		t.Fatalf("follow-up engine:status failed: result=%s rpc=%v err=%v", result, rpcErr, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
