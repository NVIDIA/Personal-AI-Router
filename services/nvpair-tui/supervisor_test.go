// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain doubles as a fake nvpair-ui-broker when NVPAIR_TUI_FAKE_BROKER=1.
// The supervisor smoke test re-execs this same test binary with that env
// set, so Spawn can drive a real child process over stdio without needing
// the actual broker built.
func TestMain(m *testing.M) {
	if os.Getenv("NVPAIR_TUI_FAKE_BROKER") == "1" {
		runFakeBroker()
		return
	}
	if os.Getenv("NVPAIR_TUI_SILENT_BROKER") == "1" {
		runSilentBroker()
		return
	}
	os.Exit(m.Run())
}

// fakeBrokerPreemptedExit is the fake broker's exit code when the supervisor
// stops the engines itself instead of leaving teardown order to the broker.
const fakeBrokerPreemptedExit = 3

// runFakeBroker emits an app:ready handshake, then answers the shutdown request,
// exiting on it or on stdin EOF, mimicking the real broker's stdio contract
// closely enough to exercise the supervisor.
//
// An engine:prepare-shutdown from the supervisor is a failure, reported through
// the exit code: the broker stops the proxy before the engines, and a client
// stopping the engines first reverses that.
func runFakeBroker() {
	fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"app:ready","params":{"version":"fake"}}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		switch m.Method {
		case "engine:prepare-shutdown":
			os.Exit(fakeBrokerPreemptedExit)
		case "shutdown":
			fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%s,"result":null}`+"\n", m.ID)
			os.Exit(0)
		}
	}
}

// runSilentBroker handshakes and then answers nothing, standing in for a broker
// that has stopped responding.
func runSilentBroker() {
	fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"app:ready","params":{"version":"fake"}}`)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
	}
}

func TestResolveBrokerPathOverride(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-broker")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBrokerPath(bin)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if got != bin {
		t.Fatalf("got %q want %q", got, bin)
	}

	if _, err := resolveBrokerPath(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for missing override")
	}
}

// TestShutdownIsBoundedWhenTheBrokerIsSilent pins the quit budget.
//
// If a broker that stopped answering could stall shutdown, pressing q would
// leave a terminal that has stopped redrawing and an operator reaching for
// ctrl+c — which kills the tree the clean shutdown existed to avoid.
func TestShutdownIsBoundedWhenTheBrokerIsSilent(t *testing.T) {
	t.Setenv("NVPAIR_TUI_SILENT_BROKER", "1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup, err := Spawn(ctx, os.Args[0])
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	go func() {
		sc := bufio.NewScanner(sup.Stderr)
		for sc.Scan() {
		}
	}()

	done := make(chan struct{})
	go func() {
		sup.Shutdown()
		close(done)
	}()

	// Long enough for the shutdown request's own deadline plus the broker's
	// grace, and well short of hanging.
	budget := 2*time.Second + shutdownGrace + 5*time.Second
	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("shutdown still running after %s against an unresponsive broker", budget)
	}
}

func TestSupervisorReadyAndShutdown(t *testing.T) {
	t.Setenv("NVPAIR_TUI_FAKE_BROKER", "1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup, err := Spawn(ctx, os.Args[0])
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	go func() {
		sc := bufio.NewScanner(sup.Stderr)
		for sc.Scan() {
		}
	}()

	select {
	case msg, ok := <-sup.Client.Notifications():
		if !ok {
			t.Fatal("notifications closed before app:ready")
		}
		if msg.Method != "app:ready" {
			t.Fatalf("first notification = %s, want app:ready", msg.Method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for app:ready")
	}

	done := make(chan struct{})
	go func() {
		sup.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	if code := sup.cmd.ProcessState.ExitCode(); code == fakeBrokerPreemptedExit {
		t.Error("the supervisor stopped the engines itself, ahead of the broker's " +
			"proxy-first teardown")
	}
}
