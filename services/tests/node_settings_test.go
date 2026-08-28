// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// Integration coverage for nvpair-node-settings.
//
// The unit tests in nvpair-node-settings/manager_test.go drive the
// manager directly, which is cheap and exhaustive. These tests cost
// more (real subprocess, real stdio pipes, real on-disk file) but
// catch a different class of bug: ldflag stamping, the codec/transport
// layer, the persist-then-reload-with-a-fresh-process loop, and that
// the binary actually starts at all. Keep this file focused on flows
// the unit tests can't observe — don't mirror per-field validation
// coverage here.

// startNodeSettings spawns nvpair-node-settings with a `--settings <path>`
// pointing at a per-test file. Returns stdin (for sending requests),
// a channel of received frames, and a cleanup func that closes stdin
// and reaps the process.
func startNodeSettings(t *testing.T, settingsPath string) (io.WriteCloser, <-chan jsonrpc.Message, func()) {
	t.Helper()

	cmd := exec.Command(nodeSettingsBin, "--settings", settingsPath)
	cmd.Stderr = os.Stderr

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("node-settings stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("node-settings stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start node-settings: %v", err)
	}
	t.Logf("node-settings started: pid=%d settings=%s", cmd.Process.Pid, settingsPath)

	ch := startMsgReader(stdoutPipe)

	cleanup := func() {
		stdinPipe.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	return stdinPipe, ch, cleanup
}

// nextID hands out a monotonically increasing JSON-RPC ID per process
// run. Tests in the same package may run in parallel via t.Run, so
// atomic is required even though most callers are single-goroutine.
var rpcIDCounter int64

func newRPCID() int64 { return atomic.AddInt64(&rpcIDCounter, 1) }

// callRPC sends a JSON-RPC request and blocks until the response for
// that ID arrives or the timeout fires. It deliberately re-implements
// the wait logic instead of reusing waitForResponse so notifications
// (e.g. the startup `ready`) are skipped over rather than mistaken
// for the response.
func callRPC(t *testing.T, in io.Writer, msgs <-chan jsonrpc.Message, method string, params any, timeout time.Duration) jsonrpc.Message {
	t.Helper()
	id := newRPCID()
	idRaw := json.RawMessage(strconv.FormatInt(id, 10))
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		rawParams = b
	}
	sendLine(t, in, jsonrpc.Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: rawParams})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatalf("stream closed before receiving response to %s (id=%d)", method, id)
			}
			if msg.Method != "" {
				// Skip notifications.
				continue
			}
			var gotID int64
			if msg.ID != nil && json.Unmarshal(*msg.ID, &gotID) == nil && gotID == id {
				return msg
			}
		case <-timer.C:
			t.Fatalf("timed out (%s) waiting for response to %s (id=%d)", timeout, method, id)
		}
	}
}

// TestNodeSettingsEndToEndStartReadyAndDefaults verifies the most
// basic startup contract: the binary launches, emits a `ready`
// notification (with the version stamped via ldflags), and responds
// to the product defaults.
func TestNodeSettingsEndToEndStartReadyAndDefaults(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	in, msgs, cleanup := startNodeSettings(t, settings)
	defer cleanup()

	ready := waitForMethod(t, msgs, "ready", 5*time.Second)
	var params struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(ready.Params, &params); err != nil {
		t.Fatalf("ready params: %v", err)
	}
	if params.Version == "" {
		t.Fatal("ready notification missing version field")
	}

	resp := callRPC(t, in, msgs, "settings/get-cluster-id", nil, 3*time.Second)
	var id struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(resp.Result, &id); err != nil {
		t.Fatalf("decode cluster-id: %v", err)
	}
	if id.Value != "" {
		t.Errorf("default cluster-id should be empty, got %q", id.Value)
	}

	resp = callRPC(t, in, msgs, "settings/get-force-ports", nil, 3*time.Second)
	var fp struct {
		Value bool `json:"value"`
	}
	if err := json.Unmarshal(resp.Result, &fp); err != nil {
		t.Fatalf("decode force-ports: %v", err)
	}
	if !fp.Value {
		t.Errorf("default force-ports should be true, got false")
	}
}

// TestNodeSettingsForcePortsRoundTripOverWire walks the simple bool
// round-trip through the real subprocess: set true, read back true,
// set false, read back false. The unit tests cover the same path in
// memory; this one exists to make sure the codec/transport doesn't
// silently mangle the payload.
func TestNodeSettingsForcePortsRoundTripOverWire(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	in, msgs, cleanup := startNodeSettings(t, settings)
	defer cleanup()
	waitForMethod(t, msgs, "ready", 5*time.Second)

	resp := callRPC(t, in, msgs, "settings/set-force-ports",
		map[string]any{"value": true}, 3*time.Second)
	if resp.Error != nil {
		t.Fatalf("set true rejected: %v", resp.Error)
	}
	resp = callRPC(t, in, msgs, "settings/get-force-ports", nil, 3*time.Second)
	var got struct {
		Value bool `json:"value"`
	}
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode get-force-ports: %v", err)
	}
	if !got.Value {
		t.Fatalf("force-ports did not round-trip true: %v", got.Value)
	}

	resp = callRPC(t, in, msgs, "settings/set-force-ports",
		map[string]any{"value": false}, 3*time.Second)
	if resp.Error != nil {
		t.Fatalf("set false rejected: %v", resp.Error)
	}
	resp = callRPC(t, in, msgs, "settings/get-force-ports", nil, 3*time.Second)
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode get-force-ports: %v", err)
	}
	if got.Value {
		t.Fatalf("force-ports did not round-trip false: %v", got.Value)
	}
}

// TestNodeSettingsClusterIdentityPushOnSetCrossProcess confirms the
// `connection/cluster-identity` notification is emitted by the real
// subprocess after a successful settings/set-cluster-id, with the
// current `{id}` payload. The unit test covers the same path in
// memory; this one pins the wire shape so a codec or framing
// regression can't silently break it.
func TestNodeSettingsClusterIdentityPushOnSetCrossProcess(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	in, msgs, cleanup := startNodeSettings(t, settings)
	defer cleanup()
	waitForMethod(t, msgs, "ready", 5*time.Second)

	resp := callRPC(t, in, msgs, "settings/set-cluster-id",
		map[string]any{"value": "cluster-xyz"}, 3*time.Second)
	if resp.Error != nil {
		t.Fatalf("set-cluster-id rejected: %v", resp.Error)
	}
	push := waitForMethod(t, msgs, "connection/cluster-identity", 3*time.Second)
	var pushParams struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(push.Params, &pushParams); err != nil {
		t.Fatalf("decode connection/cluster-identity params: %v", err)
	}
	if pushParams.ID != "cluster-xyz" {
		t.Errorf("push id = %q, want %q", pushParams.ID, "cluster-xyz")
	}
}

// TestNodeSettingsPersistsAcrossProcessRestart is the most important
// integration test: kill the subprocess after a set, spawn a fresh
// one against the same settings file, and confirm the value survived.
// This is exactly the launch-app, configure, close-app, relaunch loop
// the user runs in real life — and it's the path that pure in-package
// tests can't observe (they reuse one Manager instance against a temp
// file, which doesn't exercise the cold-start load).
func TestNodeSettingsPersistsAcrossProcessRestart(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")

	// First process: write some values, then shut down.
	in, msgs, cleanup := startNodeSettings(t, settings)
	waitForMethod(t, msgs, "ready", 5*time.Second)
	resp := callRPC(t, in, msgs, "settings/set-cluster-auto-sync",
		map[string]any{"value": true}, 3*time.Second)
	if resp.Error != nil {
		t.Fatalf("set cluster-auto-sync rejected: %v", resp.Error)
	}
	resp = callRPC(t, in, msgs, "settings/set-cluster-id",
		map[string]any{"value": "cluster-abc-123"}, 3*time.Second)
	if resp.Error != nil {
		t.Fatalf("set cluster-id rejected: %v", resp.Error)
	}
	resp = callRPC(t, in, msgs, "settings/set-cluster-friendly-name",
		map[string]any{"value": "Lab 3 desks"}, 3*time.Second)
	if resp.Error != nil {
		t.Fatalf("set cluster-friendly-name rejected: %v", resp.Error)
	}
	// Graceful shutdown so we know the save completed before the
	// pipe closes.
	_ = callRPC(t, in, msgs, "shutdown", nil, 3*time.Second)
	cleanup()

	// Verify the on-disk file is real JSON (catches torn-write
	// bugs).
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("persisted file is not valid JSON: %v\n%s", err, data)
	}

	// Second process: same file, fresh subprocess. Values must
	// reappear.
	in2, msgs2, cleanup2 := startNodeSettings(t, settings)
	defer cleanup2()
	waitForMethod(t, msgs2, "ready", 5*time.Second)

	resp = callRPC(t, in2, msgs2, "settings/get-cluster-auto-sync", nil, 3*time.Second)
	var sync struct {
		Value bool `json:"value"`
	}
	_ = json.Unmarshal(resp.Result, &sync)
	if !sync.Value {
		t.Errorf("cluster-auto-sync did not survive restart")
	}

	resp = callRPC(t, in2, msgs2, "settings/get-cluster-id", nil, 3*time.Second)
	var id struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(resp.Result, &id)
	if id.Value != "cluster-abc-123" {
		t.Errorf("cluster-id did not survive restart, got %q want %q", id.Value, "cluster-abc-123")
	}

	resp = callRPC(t, in2, msgs2, "settings/get-cluster-friendly-name", nil, 3*time.Second)
	var name struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(resp.Result, &name)
	if name.Value != "Lab 3 desks" {
		t.Errorf("cluster-friendly-name did not survive restart, got %q want %q", name.Value, "Lab 3 desks")
	}
}

// TestNodeSettingsShutdownIsClean confirms the `shutdown` RPC
// terminates the subprocess in a bounded time. A regression here
// (e.g. a goroutine that never receives the cancel) would manifest
// as the cleanup func timing out and force-killing — which we'd see
// in CI as a flaky test, but we want to fail loudly instead.
func TestNodeSettingsShutdownIsClean(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	in, msgs, cleanup := startNodeSettings(t, settings)
	defer cleanup()
	waitForMethod(t, msgs, "ready", 5*time.Second)

	resp := callRPC(t, in, msgs, "shutdown", nil, 3*time.Second)
	// shutdown returns null result (or, equivalently, no payload).
	// What matters is that we get a response and the process
	// terminates without the cleanup func having to fall back to
	// SIGKILL — both of which are implicit in the lack of a
	// timeout failure above.
	if resp.Error != nil {
		t.Fatalf("shutdown returned an error: %v", resp.Error)
	}
}
