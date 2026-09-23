// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Fixtures shared by more than one test file in this package. Go test files
// share a package, so a helper defined next to the tests that first needed it
// is reachable from every other file — which is how newTestRPCWorkerPipe came
// to live in lmstudioport_test.go while ollamahost_test.go depends on it. That
// works until the defining file moves or is consolidated, at which point the
// whole test binary stops compiling and the cause is invisible in the diff.
// Anything used across files belongs here instead.

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

// Both engines' port tests assert on an ownership gate opening or staying
// shut, and did it by hand with an inline select. Three shapes recurred, and
// the difference between two of them is easy to get wrong: waiting on
// time.After proves the gate stayed shut through a window, while a default arm
// only proves it was shut at that instant. Naming the three keeps a test from
// silently asserting the weaker one.
const (
	// gateSettleWindow is how long a gate is given to open before the test
	// calls it stuck. Generous, because opening one can involve a real RPC
	// round-trip over a pipe.
	gateSettleWindow = 2 * time.Second

	// gateQuietWindow is how long a gate must stay shut to count as shut.
	gateQuietWindow = 100 * time.Millisecond
)

// requireGateOpens fails unless the ownership gate opens. reason names what
// should have opened it.
func requireGateOpens(t *testing.T, gate <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-gate:
	case <-time.After(gateSettleWindow):
		t.Fatalf("ownership gate never opened: %s", reason)
	}
}

// requireGateStaysShut fails if the gate opens during the quiet window. Use it
// after something that must not settle ownership; the wait is what makes it
// meaningful, since the work under test may be on another goroutine.
func requireGateStaysShut(t *testing.T, gate <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-gate:
		t.Fatalf("ownership gate opened when it should not have: %s", reason)
	case <-time.After(gateQuietWindow):
	}
}

// requireGateShutNow fails if the gate is already open, without waiting. Use it
// only when nothing asynchronous is in flight; otherwise prefer
// requireGateStaysShut.
func requireGateShutNow(t *testing.T, gate <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-gate:
		t.Fatalf("ownership gate was already open: %s", reason)
	default:
	}
}

// requireGateOpenNow fails unless the gate is already open, without waiting.
// The synchronous counterpart to requireGateOpens, for a caller that settled
// ownership on this goroutine and should not need to wait for it.
func requireGateOpenNow(t *testing.T, gate <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-gate:
	default:
		t.Fatalf("ownership gate was not open: %s", reason)
	}
}

// newTestRPCWorkerPipe returns a worker whose peer is served over an in-memory
// pipe, plus the codec for the far end so a test can act as the worker.
func newTestRPCWorkerPipe(t *testing.T) (*rpcWorker, *Codec) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	worker := &rpcWorker{peer: NewPeer(NewCodec(client))}
	go worker.peer.Serve(nil, nil)
	return worker, NewCodec(server)
}

// brokerWithEngineStatus is brokerWithEngineInventory for a single engine.
func brokerWithEngineStatus(t *testing.T, engine string, port int) *Broker {
	return brokerWithEngineInventory(t, map[string]int{engine: port})
}

// brokerWithEngineInventory returns a broker whose engine-manager answers one
// engine:status or engine:get-installed request from a fixed port inventory.
func brokerWithEngineInventory(t *testing.T, ports map[string]int) *Broker {
	t.Helper()
	client, server := net.Pipe()
	worker := &rpcWorker{peer: NewPeer(NewCodec(client))}
	go worker.peer.Serve(nil, nil)
	b := &Broker{}
	b.setEngineMgr(worker)
	go func() {
		codec := NewCodec(server)
		request, err := codec.Read()
		if err != nil {
			return
		}
		switch request.Method {
		case "engine:status":
			var params struct {
				Engine string `json:"engine"`
			}
			if json.Unmarshal(request.Params, &params) != nil {
				_ = codec.RespondError(request.ID, -32602, "invalid engine status request")
				return
			}
			_ = codec.Respond(request.ID, map[string]any{"port": ports[params.Engine]})
		case "engine:get-installed":
			engines := make([]map[string]any, 0, len(ports))
			for engine, port := range ports {
				engines = append(engines, map[string]any{"engine": engine, "port": port})
			}
			_ = codec.Respond(request.ID, map[string]any{"engines": engines})
		default:
			_ = codec.RespondError(request.ID, -32602, "unexpected engine status request")
		}
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return b
}

// isolateOllamaHostTestConfig points the per-user config dir at a temp dir so a
// test's persisted state cannot touch the developer's real configuration.
//
// All four variables are required. appdir consults LOCALAPPDATA only on
// Windows and otherwise falls through to os.UserConfigDir, which reads HOME on
// macOS and XDG_CONFIG_HOME (then HOME) on Linux. Setting only the first two
// left macOS unisolated, so TestConfiguredLMStudioProxyPort wrote its fixture
// into the developer's real config dir and then failed on every subsequent run
// against the value it had planted. CI never saw it because its config dir
// starts empty.
func isolateOllamaHostTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
}
