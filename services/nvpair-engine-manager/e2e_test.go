// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"io"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestE2EOverStdio drives the real engine-manager binary end-to-end
// over JSON-RPC stdio: it discovers an injected "fake" engine, starts
// it (spawning the fake-engine child), runs an action, stops it, and
// shuts down — the same path the supervising broker uses.
func TestE2EOverStdio(t *testing.T) {
	cfg := t.TempDir()
	home := t.TempDir()
	// Drop the test manifest in every location os.UserConfigDir might
	// resolve to, so the child finds it regardless of OS.
	for _, dir := range []string{
		filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"),                                    // Windows %LocalAppData%, Linux $XDG_CONFIG_HOME
		filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"), // macOS
	} {
		writeFakeManifest(t, dir)
	}

	cmd := exec.Command(managerBin)
	cmd.Env = overrideEnv(map[string]string{"APPDATA": cfg, "LOCALAPPDATA": cfg, "XDG_CONFIG_HOME": cfg, "HOME": home})
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

	send(t, stdin, 1, "engine:get-installed", nil)
	require.Contains(t, string(waitResult(t, frames, "1", 5*time.Second)), `"engine":"fake"`, "get-installed did not list the injected engine")

	send(t, stdin, 2, "engine:start", map[string]any{"engine": "fake"})
	require.Contains(t, string(waitResult(t, frames, "2", 20*time.Second)), `"running":true`, "start did not report running")

	send(t, stdin, 3, "engine:action", map[string]any{"engine": "fake", "action": "list_models"})
	require.Contains(t, string(waitResult(t, frames, "3", 10*time.Second)), "llama3.2", "action result unexpected")

	send(t, stdin, 4, "engine:stop", map[string]any{"engine": "fake"})
	require.Contains(t, string(waitResult(t, frames, "4", 10*time.Second)), `"running":false`, "stop did not report stopped")

	send(t, stdin, 5, "shutdown", nil)
	waitResult(t, frames, "5", 5*time.Second)
}

// Saving a bundled engine's port through the real worker must preserve the
// rest of its override across a new worker process, not just in-memory state.
func TestE2EPortSavePreservesLaunchOverrides(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	override := map[string]any{
		"engine": "ollama",
		"runtime": map[string]any{
			"args": []string{"serve", "--custom-option"},
			"env":  map[string]string{"CUSTOM_SETTING": "retained"},
		},
	}
	for _, dir := range []string{
		filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, writeJSONAtomic(filepath.Join(dir, "ollama.json"), override))
	}
	first := startE2EManager(t, cfg, home)
	send(t, first.stdin, 1, "engine:set-port", map[string]any{"engine": "ollama", "port": 26001})
	var saved EngineStatus
	require.NoError(t, json.Unmarshal(waitResult(t, first.frames, "1", 10*time.Second), &saved), "saved status")
	require.Equal(t, 26001, saved.Port, "saved status (%v)", saved)
	require.False(t, saved.Running, "saved status (%v)", saved)
	first.stop(t)
	second := startE2EManager(t, cfg, home)
	send(t, second.stdin, 1, "engine:describe", map[string]any{"engine": "ollama"})
	var manifest Manifest
	require.NoError(t, json.Unmarshal(waitResult(t, second.frames, "1", 10*time.Second), &manifest))
	platform, ok := manifest.HostPlatform()
	require.True(t, ok, "worker restart lost settings (%v)", platform)
	require.Equal(t, 26001, platform.Runtime.Port, "worker restart lost settings (%v)", platform)
	require.Len(t, platform.Runtime.Args, 2, "worker restart lost settings (%v)", platform)
	require.Equal(t, "--custom-option", platform.Runtime.Args[1], "worker restart lost settings (%v)", platform)
	require.Equal(t, "retained", platform.Runtime.Env["CUSTOM_SETTING"], "worker restart lost settings (%v)", platform)
	second.stop(t)
}

func TestE2EDesiredStateAcrossShutdownRPC(t *testing.T) {
	cfg := t.TempDir()
	home := t.TempDir()
	for _, dir := range []string{
		filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		writeFakeManifest(t, dir)
	}

	first := startE2EManager(t, cfg, home)
	send(t, first.stdin, 1, "engine:start", map[string]any{"engine": "fake"})
	var started EngineStatus
	require.NoError(t, json.Unmarshal(waitResult(t, first.frames, "1", 20*time.Second), &started), "start status")
	require.True(t, started.Running, "start status (%v)", started)
	send(t, first.stdin, 2, prepareShutdownMethod, nil)
	waitResult(t, first.frames, "2", 15*time.Second)
	require.False(t, portServing(started.Port), "engine port")
	first.stop(t)

	second := startE2EManager(t, cfg, home)
	notify(t, second.stdin, restoreEnabledMethod, nil)
	waitNotify(t, second.frames, "engine:state-changed", 20*time.Second)
	send(t, second.stdin, 1, "engine:status", map[string]any{"engine": "fake"})
	require.Contains(t, string(waitResult(t, second.frames, "1", 5*time.Second)), `"running":true`, "saved ON state was not restored")
	send(t, second.stdin, 2, "engine:stop", map[string]any{"engine": "fake"})
	waitResult(t, second.frames, "2", 10*time.Second)
	second.stop(t)

	third := startE2EManager(t, cfg, home)
	notify(t, third.stdin, restoreEnabledMethod, nil)
	send(t, third.stdin, 1, "engine:status", map[string]any{"engine": "fake"})
	require.Contains(t, string(waitResult(t, third.frames, "1", 5*time.Second)), `"running":false`, "explicit OFF state was not preserved")
	third.stop(t)
}

// TestE2EPrepareShutdownCancelsStartingEngine proves shutdown preparation
// cancels a spawned engine that is still inside its readiness allowance,
// responds while the manager transport remains alive, and joins the child
// before reporting completion.
func TestE2EPrepareShutdownCancelsStartingEngine(t *testing.T) {
	cfg := t.TempDir()
	home := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "fake.pid")
	manifest := testEngineManifest(fakeEngineBin)
	platform := manifest.Platforms[hostKey()]
	platform.Runtime.Env["FAKE_PID_FILE"] = pidFile
	platform.Runtime.Env["FAKE_START_DELAY"] = "1m"
	platform.Runtime.Ready.TimeoutS = 120
	manifest.Platforms[hostKey()] = platform
	for _, dir := range []string{
		filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		writeE2EManifest(t, dir, manifest)
	}

	manager := startE2EManager(t, cfg, home)
	send(t, manager.stdin, 1, "engine:start", map[string]any{"engine": "fake"})

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if pid == 0 {
			time.Sleep(25 * time.Millisecond)
		}
	}
	require.NotEqual(t, 0, pid, "fake engine did not publish its PID")
	t.Cleanup(func() {
		if pidAlive(pid) {
			_ = signalPID(pid, true)
		}
	})

	send(t, manager.stdin, 2, prepareShutdownMethod, nil)
	waitResult(t, manager.frames, "2", 10*time.Second)
	require.False(t, pidAlive(pid), "managed fake engine PID (%v)", pid)

	send(t, manager.stdin, 3, "engine:status", map[string]any{"engine": "fake"})
	var stopped EngineStatus
	require.NoError(t, json.Unmarshal(waitResult(t, manager.frames, "3", 5*time.Second), &stopped), "status after shutdown preparation")
	require.False(t, stopped.Running, "status after shutdown preparation (%v)", stopped)

	send(t, manager.stdin, 4, "engine:errors", nil)
	var reported struct {
		Errors []serviceError `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(waitResult(t, manager.frames, "4", 5*time.Second), &reported), "decode errors after shutdown preparation")
	require.False(t, hasErr(reported.Errors, startFailedID("fake")), "shutdown cancellation retained a start-failed error")
	manager.stop(t)
}

type e2eManager struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	frames  chan frame
	stopped bool
}

func startE2EManager(t *testing.T, cfg, home string, args ...string) *e2eManager {
	t.Helper()
	cmd := exec.Command(managerBin, args...)
	cmd.Env = overrideEnv(map[string]string{"APPDATA": cfg, "LOCALAPPDATA": cfg, "XDG_CONFIG_HOME": cfg, "HOME": home})
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	manager := &e2eManager{cmd: cmd, stdin: stdin, frames: make(chan frame, 128)}
	t.Cleanup(func() {
		if !manager.stopped {
			_ = manager.stdin.Close()
			_ = manager.cmd.Process.Kill()
			_ = manager.cmd.Wait()
		}
	})
	go readFrames(t, stdout, manager.frames)
	waitNotify(t, manager.frames, "engine:ready", 5*time.Second)
	return manager
}

func (m *e2eManager) stop(t *testing.T) {
	t.Helper()
	if m.stopped {
		return
	}
	m.stopped = true
	_ = m.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- m.cmd.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err, "manager exit")
	case <-time.After(10 * time.Second):
		_ = m.cmd.Process.Kill()
		<-done
		require.FailNow(t, "manager did not exit after stdin closed")
	}
}

func writeFakeManifest(t *testing.T, dir string) {
	writeE2EManifest(t, dir, testEngineManifest(fakeEngineBin))
}

func writeE2EManifest(t *testing.T, dir string, manifest *Manifest) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fake.json"), data, 0o644))
}

// overrideEnv returns the current environment with the given keys
// replaced (case-insensitively, for Windows %AppData%).
func overrideEnv(over map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToUpper(kv[:eq])
		if _, ok := over[key]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range over {
		out = append(out, k+"="+v)
	}
	return out
}

type frame struct {
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func readFrames(t *testing.T, r io.Reader, out chan<- frame) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var f frame
		if !assert.NoError(t, json.Unmarshal(sc.Bytes(), &f), "decode frame") {
			continue
		}
		out <- f
	}
}

func send(t *testing.T, w io.Writer, id int, method string, params any) {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	assert.NoError(t, err)
	_, err = w.Write(append(data, '\n'))
	require.NoError(t, err, "send (%v, %v)", method, err)
}

func notify(t *testing.T, w io.Writer, method string, params any) {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	assert.NoError(t, err)
	_, err = w.Write(append(data, '\n'))
	require.NoError(t, err, "notify (%v, %v)", method, err)
}

func waitResult(t *testing.T, frames <-chan frame, id string, timeout time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f := <-frames:
			if string(f.ID) != id {
				continue
			}
			require.Contains(t, []string{"", "null"}, string(f.Error), "rpc id (%v)", id)
			return f.Result
		case <-deadline:
			require.FailNowf(t, "timed out waiting for response", "id %s", id)
			return nil
		}
	}
}

func waitNotify(t *testing.T, frames <-chan frame, method string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f := <-frames:
			if f.Method == method {
				return
			}
		case <-deadline:
			require.FailNowf(t, "timed out waiting for notification", "%q", method)
		}
	}
}

// Configure can request a parent rebind while both stdio readers continue
// servicing unrelated messages; literal settings survive a fresh worker.
func TestE2ESettingsRebindRelayAndWorkerReload(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	fixture := settingsExecutor(t, false)
	manifest, _ := fixture.reg.Get("fake")
	for _, dir := range []string{filepath.Join(cfg, "Nvidia Corporation", "Personal AI Router", "engines"), filepath.Join(home, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines")} {
		writeE2EManifest(t, dir, manifest)
	}
	manager := startE2EManager(t, cfg, home)
	send(t, manager.stdin, 1, "engine:get-installed", nil)
	waitResult(t, manager.frames, "1", 5*time.Second)
	send(t, manager.stdin, 2, "engine:get-launch", map[string]string{"engine": "fake"})
	var launch settings.LaunchState
	require.NoError(t, json.Unmarshal(waitResult(t, manager.frames, "2", 5*time.Second), &launch))
	desired := settings.Config{ServerPort: launch.ServerPort, ProxyPort: 54301, LaunchText: `OLLAMA_ORIGINS="http://localhost" PAIR_TEST_LITERAL="$HOME {port}" ` + launch.LaunchText}
	send(t, manager.stdin, 3, "engine:configure-launch", settings.Configure{Engine: "fake", Settings: desired, OperationID: "operation"})
	var relay settings.Relay
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	waiting := true
	for waiting {
		select {
		case f := <-manager.frames:
			if f.Method == "engine:settings-request" {
				require.NoError(t, json.Unmarshal(f.Params, &relay))
				waiting = false
			}
		case <-timer.C:
			require.FailNow(t, "worker never requested parent rebind")
		}
	}
	require.Equal(t, "rebind", relay.Method, "bad relay (%v)", relay)
	require.Equal(t, "operation", relay.Request.RequestID, "bad relay (%v)", relay)
	require.Equal(t, 54301, relay.Request.Settings.ProxyPort, "bad relay (%v)", relay)
	send(t, manager.stdin, 4, "engine:describe", map[string]string{"engine": "fake"})
	waitResult(t, manager.frames, "4", 5*time.Second)
	notify(t, manager.stdin, "engine:settings-reply", settingsReply{ID: relay.ID, Result: json.RawMessage(`{}`)})
	require.NoError(t, json.Unmarshal(waitResult(t, manager.frames, "3", 5*time.Second), &launch), "configure result")
	require.False(t, launch.Running, "configure result (%v)", launch)
	saved := launch.LaunchText
	manager.stop(t)
	restored := startE2EManager(t, cfg, home)
	send(t, restored.stdin, 1, "engine:get-installed", nil)
	waitResult(t, restored.frames, "1", 5*time.Second)
	send(t, restored.stdin, 2, "engine:get-launch", map[string]string{"engine": "fake"})
	require.NoError(t, json.Unmarshal(waitResult(t, restored.frames, "2", 5*time.Second), &launch), "worker reload lost literal settings")
	require.Equal(t, saved, launch.LaunchText, "worker reload lost literal settings (%v)", launch)
	restored.stop(t)
}
