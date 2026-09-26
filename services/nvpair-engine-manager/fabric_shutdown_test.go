// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFabricPrepareShutdownJoinsPendingApplyAndPreservesStableAddresses(t *testing.T) {
	m, _, _ := cableTestManager(t, nil)
	applyCtx, cancelApply := context.WithCancel(context.Background())
	defer cancelApply()
	done := make(chan struct{})
	var releaseOnce sync.Once
	releaseCleanup := func() { releaseOnce.Do(func() { close(done) }) }
	defer releaseCleanup()
	m.exec.fabric.runs[strings.Repeat("a", 32)] = &fabricRunRecord{
		Public: fabricOperation{State: "applying"}, cancel: cancelApply, done: done,
	}
	stableDone := make(chan struct{})
	close(stableDone)
	stable := &fabricRunRecord{Public: fabricOperation{State: "active", EffectsApplied: true}, done: stableDone}
	m.exec.fabric.runs[strings.Repeat("b", 32)] = stable

	// Only the in-memory RPC transport and an owned cleanup completion channel
	// are used. There is no fabric worker, socket listener or native action.
	managerConn, clientConn := net.Pipe()
	defer managerConn.Close()
	defer clientConn.Close()
	m.codec = NewCodec(managerConn)
	type reply struct {
		message Message
		err     error
	}
	replies := make(chan reply, 1)
	go func() {
		var response Message
		err := json.NewDecoder(clientConn).Decode(&response)
		replies <- reply{message: response, err: err}
	}()
	id := json.RawMessage("902")
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: prepareShutdownMethod})
	select {
	case <-applyCtx.Done():
	case response := <-replies:
		t.Fatalf("shutdown acknowledged before cancelling pending fabric apply: %+v %v", response.message, response.err)
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel pending fabric apply")
	}
	m.exec.diagnostics.mu.Lock()
	closed := m.exec.diagnostics.packageAdmissionClosed
	m.exec.diagnostics.mu.Unlock()
	if !closed {
		t.Fatal("fabric shutdown lost the setup admission fence")
	}
	select {
	case response := <-replies:
		t.Fatalf("shutdown acknowledged before fabric cleanup joined: %+v %v", response.message, response.err)
	default:
	}
	releaseCleanup()
	select {
	case response := <-replies:
		if response.err != nil || response.message.ID == nil || string(*response.message.ID) != string(id) || response.message.Error != nil {
			t.Fatalf("invalid shutdown acknowledgement: %+v %v", response.message, response.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not acknowledge joined fabric cleanup")
	}
	if stable.Public.State != "active" || !stable.Public.EffectsApplied || stable.Public.CleanupConfirmed {
		t.Fatal("shutdown changed stable applied-address ownership")
	}
}
