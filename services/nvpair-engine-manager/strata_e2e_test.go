// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStrataFakeEngine_LifecycleOverStdio drives the real engine-manager
// binary against the real strata manifest (paths rewritten to a fake
// `strata` CLI, port auto-assigned) and the fake CLI's detached server:
// detect, start with the readiness probe, a download while stopped,
// model listing from run configs, a model switch through launch_model's
// family split, and a stop through the CLI. It is the cross-process
// stand-in for a live Strata; no engine download or network is needed.
// The fake detaches with Setsid, so it is POSIX-only.
func TestStrataFakeEngine_LifecycleOverStdio(t *testing.T) {
	if fakeStrataBin == "" {
		t.Skip("the fake strata CLI is built for POSIX only")
	}
	cfg := t.TempDir()
	home := t.TempDir()
	manifestDir := filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines")
	writeStrataE2EManifest(t, manifestDir)

	cmd := exec.Command(managerBin)
	cmd.Env = overrideEnv(map[string]string{"APPDATA": cfg, "LOCALAPPDATA": cfg, "XDG_CONFIG_HOME": cfg, "HOME": home})
	// Whatever the test does, a stray fake server from a failed step is
	// stopped through the same CLI the manifest uses.
	t.Cleanup(func() {
		stop := exec.Command(fakeStrataBin, "launch", "--stop")
		stop.Env = append(os.Environ(), "HOME="+home)
		_ = stop.Run()
	})
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	frames := make(chan frame, 128)
	go readFrames(t, stdout, frames)
	waitNotify(t, frames, "engine:ready", 5*time.Second)

	nextID := 1
	sendAction := func(action string, params map[string]any) string {
		t.Helper()
		p := map[string]any{"engine": "strata", "action": action}
		if params != nil {
			p["params"] = params
		}
		id := nextID
		nextID++
		send(t, stdin, id, "engine:action", p)
		return string(waitResult(t, frames, itoa(id), 20*time.Second))
	}
	actionEventuallyContains := func(action string, params map[string]any, want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			last = sendAction(action, params)
			if strings.Contains(last, want) {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("action %q never reported %q; last result: %s", action, want, last)
	}

	send(t, stdin, nextID, "engine:get-installed", nil)
	installed := string(waitResult(t, frames, itoa(nextID), 5*time.Second))
	nextID++
	require.Contains(t, installed, `"strata"`, "the fake strata CLI was not detected")

	// A download runs with no engine up; the fake records it as an
	// installed run config, visible to list_models.
	sendAction("pull_model", map[string]any{"model": "unsloth/Q8_0"})
	actionEventuallyContains("list_models", nil, "unsloth-q8_0")

	// Start: the manifest's launch argv brings the fake server up detached,
	// and readiness is its /health endpoint.
	id := nextID
	nextID++
	send(t, stdin, id, "engine:start", map[string]any{"engine": "strata"})
	require.Contains(t, string(waitResult(t, frames, itoa(id), 30*time.Second)),
		`"running":true`, "start did not reach a ready strata server")

	// The served model is exactly the one the start argv selected.
	require.Contains(t, sendAction("loaded_models", nil), "qwen-q2_0")

	// A model switch writes both halves of the family/quant id into the
	// saved launch text and restarts on them.
	sendAction("load_model", map[string]any{"model": "coder/IQ1_M"})
	actionEventuallyContains("loaded_models", nil, "coder-iq1_m")

	id = nextID
	nextID++
	send(t, stdin, id, "engine:stop", map[string]any{"engine": "strata"})
	require.Contains(t, string(waitResult(t, frames, itoa(id), 30*time.Second)),
		`"running":false`, "stop through `strata launch --stop` did not take the server down")

	send(t, stdin, nextID, "shutdown", nil)
	waitResult(t, frames, itoa(nextID), 5*time.Second)
}

// writeStrataE2EManifest drops the bundled strata manifest into the
// user-manifest directory, where the registry lets it override the
// bundled one by name. Three things change: the control CLI points at
// the fake binary, the fixed engine port 8091 becomes a free loopback
// port this test just reserved, and the stock-port adoption probe goes. The port stays a real fixed
// port because launch settings (model switches) validate and persist it;
// only process-mode fake engines use the port 0 auto-assign path.
func writeStrataE2EManifest(t *testing.T, dir string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	raw, err := bundledManifests.ReadFile("manifests/strata.json")
	require.NoError(t, err)
	edited := strings.ReplaceAll(string(raw), "~/.local/bin/strata", fakeStrataBin)
	edited = strings.ReplaceAll(edited, `"port": 8091`, fmt.Sprintf(`"port": %d`, port))
	// The ready-file port probe belongs to the machine, not the scenario: a
	// real Strata instance's ready file would nominate its port and this
	// test's engine would adopt it. User manifests deep-merge onto the
	// bundled JSON (arrays are replaced wholesale), so pin the globs empty
	// rather than deleting them. The fake owns only the reserved port above.
	require.Contains(t, edited, `"adopt_port_files": { "globs": ["~/.local/state/strata/runtime/*.ready.json"], "port_field": "bound_port" },`)
	edited = strings.ReplaceAll(edited, `"adopt_port_files": { "globs": ["~/.local/state/strata/runtime/*.ready.json"], "port_field": "bound_port" },`, `"adopt_port_files": { "globs": [] },`)
	require.True(t, json.Valid([]byte(edited)), "the rewritten manifest is not valid JSON")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "strata.json"), []byte(edited), 0o644))
}

func itoa(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}
