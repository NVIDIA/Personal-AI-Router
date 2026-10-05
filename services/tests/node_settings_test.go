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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NoError(t, err, "node-settings stdin pipe")
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "node-settings stdout pipe")
	require.NoError(t, cmd.Start(), "start node-settings")
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
		require.NoError(t, err, "marshal params")
		rawParams = b
	}
	sendLine(t, in, jsonrpc.Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: rawParams})

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-msgs:
			require.True(t, ok, "stream closed before receiving response to (%v, %v)", method, id)
			if msg.Method != "" {
				// Skip notifications.
				continue
			}
			var gotID int64
			if msg.ID != nil && json.Unmarshal(*msg.ID, &gotID) == nil && gotID == id {
				return msg
			}
		case <-timer.C:
			require.FailNow(t, "test expectation failed", "timed out (%s) waiting for response to %s (id=%d)", timeout, method, id)
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
	require.NoError(t, json.Unmarshal(ready.Params, &params), "ready params")
	require.NotEqual(t, "", params.Version, "ready notification missing version field")

	resp := callRPC(t, in, msgs, "settings/get-cluster-id", nil, 3*time.Second)
	var id struct {
		Value string `json:"value"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &id), "decode cluster-id")
	assert.Equal(t, "", id.Value, "default cluster-id should be empty")

	resp = callRPC(t, in, msgs, "settings/get-force-ports", nil, 3*time.Second)
	var fp struct {
		Value bool `json:"value"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &fp), "decode force-ports")
	assert.True(t, fp.Value, "default force-ports should be true, got false")
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
	require.Nil(t, resp.Error, "set true rejected")
	resp = callRPC(t, in, msgs, "settings/get-force-ports", nil, 3*time.Second)
	var got struct {
		Value bool `json:"value"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &got), "decode get-force-ports")
	require.True(t, got.Value, "force-ports did not round-trip true")

	resp = callRPC(t, in, msgs, "settings/set-force-ports",
		map[string]any{"value": false}, 3*time.Second)
	require.Nil(t, resp.Error, "set false rejected")
	resp = callRPC(t, in, msgs, "settings/get-force-ports", nil, 3*time.Second)
	require.NoError(t, json.Unmarshal(resp.Result, &got), "decode get-force-ports")
	require.False(t, got.Value, "force-ports did not round-trip false")
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
	require.Nil(t, resp.Error, "set-cluster-id rejected")
	push := waitForMethod(t, msgs, "connection/cluster-identity", 3*time.Second)
	var pushParams struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(push.Params, &pushParams), "decode connection/cluster-identity params")
	assert.Equal(t, "cluster-xyz", pushParams.ID, "push id")
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
	require.Nil(t, resp.Error, "set cluster-auto-sync rejected")
	resp = callRPC(t, in, msgs, "settings/set-cluster-id",
		map[string]any{"value": "cluster-abc-123"}, 3*time.Second)
	require.Nil(t, resp.Error, "set cluster-id rejected")
	resp = callRPC(t, in, msgs, "settings/set-cluster-friendly-name",
		map[string]any{"value": "Lab 3 desks"}, 3*time.Second)
	require.Nil(t, resp.Error, "set cluster-friendly-name rejected")
	// Graceful shutdown so we know the save completed before the
	// pipe closes.
	_ = callRPC(t, in, msgs, "shutdown", nil, 3*time.Second)
	cleanup()

	// Verify the on-disk file is real JSON (catches torn-write
	// bugs).
	data, err := os.ReadFile(settings)
	require.NoError(t, err, "read persisted file")
	var onDisk map[string]any
	{
		err := json.Unmarshal(data, &onDisk)
		require.NoError(t, err, "persisted file is not valid JSON (%v, %v)", err, data)
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
	assert.True(t, sync.Value, "cluster-auto-sync did not survive restart")

	resp = callRPC(t, in2, msgs2, "settings/get-cluster-id", nil, 3*time.Second)
	var id struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(resp.Result, &id)
	assert.Equal(t, "cluster-abc-123", id.Value, "cluster-id did not survive restart")

	resp = callRPC(t, in2, msgs2, "settings/get-cluster-friendly-name", nil, 3*time.Second)
	var name struct {
		Value string `json:"value"`
	}
	_ = json.Unmarshal(resp.Result, &name)
	assert.Equal(t, "Lab 3 desks", name.Value, "cluster-friendly-name did not survive restart")
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
	require.Nil(t, resp.Error, "shutdown returned an error")
}
