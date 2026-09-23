// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"io"
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
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	frames := make(chan frame, 128)
	go readFrames(stdout, frames)

	waitNotify(t, frames, "engine:ready", 5*time.Second)

	send(t, stdin, 1, "engine:get-installed", nil)
	if r := waitResult(t, frames, "1", 5*time.Second); !strings.Contains(string(r), `"engine":"fake"`) {
		t.Fatalf("get-installed did not list the injected engine: %s", r)
	}

	send(t, stdin, 2, "engine:start", map[string]any{"engine": "fake"})
	if r := waitResult(t, frames, "2", 20*time.Second); !strings.Contains(string(r), `"running":true`) {
		t.Fatalf("start did not report running: %s", r)
	}

	send(t, stdin, 3, "engine:action", map[string]any{"engine": "fake", "action": "list_models"})
	if r := waitResult(t, frames, "3", 10*time.Second); !strings.Contains(string(r), "llama3.2") {
		t.Fatalf("action result unexpected: %s", r)
	}

	send(t, stdin, 4, "engine:stop", map[string]any{"engine": "fake"})
	if r := waitResult(t, frames, "4", 10*time.Second); !strings.Contains(string(r), `"running":false`) {
		t.Fatalf("stop did not report stopped: %s", r)
	}

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
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSONAtomic(filepath.Join(dir, "ollama.json"), override); err != nil {
			t.Fatal(err)
		}
	}
	first := startE2EManager(t, cfg, home)
	send(t, first.stdin, 1, "engine:set-port", map[string]any{"engine": "ollama", "port": 26001})
	var saved EngineStatus
	if err := json.Unmarshal(waitResult(t, first.frames, "1", 10*time.Second), &saved); err != nil || saved.Port != 26001 || saved.Running {
		t.Fatalf("saved status=%+v, err=%v", saved, err)
	}
	first.stop(t)
	second := startE2EManager(t, cfg, home)
	send(t, second.stdin, 1, "engine:describe", map[string]any{"engine": "ollama"})
	var manifest Manifest
	if err := json.Unmarshal(waitResult(t, second.frames, "1", 10*time.Second), &manifest); err != nil {
		t.Fatal(err)
	}
	platform, ok := manifest.HostPlatform()
	if !ok || platform.Runtime.Port != 26001 || len(platform.Runtime.Args) != 2 || platform.Runtime.Args[1] != "--custom-option" || platform.Runtime.Env["CUSTOM_SETTING"] != "retained" {
		t.Fatalf("worker restart lost settings: %+v", platform)
	}
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
	if err := json.Unmarshal(waitResult(t, first.frames, "1", 20*time.Second), &started); err != nil || !started.Running {
		t.Fatalf("start status=%+v err=%v", started, err)
	}
	send(t, first.stdin, 2, prepareShutdownMethod, nil)
	waitResult(t, first.frames, "2", 15*time.Second)
	if portServing(started.Port) {
		t.Fatalf("engine port %d remained open after shutdown preparation", started.Port)
	}
	first.stop(t)

	second := startE2EManager(t, cfg, home)
	notify(t, second.stdin, restoreEnabledMethod, nil)
	waitNotify(t, second.frames, "engine:state-changed", 20*time.Second)
	send(t, second.stdin, 1, "engine:status", map[string]any{"engine": "fake"})
	if r := waitResult(t, second.frames, "1", 5*time.Second); !strings.Contains(string(r), `"running":true`) {
		t.Fatalf("saved ON state was not restored: %s", r)
	}
	send(t, second.stdin, 2, "engine:stop", map[string]any{"engine": "fake"})
	waitResult(t, second.frames, "2", 10*time.Second)
	second.stop(t)

	third := startE2EManager(t, cfg, home)
	notify(t, third.stdin, restoreEnabledMethod, nil)
	send(t, third.stdin, 1, "engine:status", map[string]any{"engine": "fake"})
	if r := waitResult(t, third.frames, "1", 5*time.Second); !strings.Contains(string(r), `"running":false`) {
		t.Fatalf("explicit OFF state was not preserved: %s", r)
	}
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
	if pid == 0 {
		t.Fatal("fake engine did not publish its PID")
	}
	t.Cleanup(func() {
		if pidAlive(pid) {
			_ = signalPID(pid, true)
		}
	})

	send(t, manager.stdin, 2, prepareShutdownMethod, nil)
	waitResult(t, manager.frames, "2", 10*time.Second)
	if pidAlive(pid) {
		t.Fatalf("managed fake engine PID %d survived shutdown preparation", pid)
	}

	send(t, manager.stdin, 3, "engine:status", map[string]any{"engine": "fake"})
	var stopped EngineStatus
	if err := json.Unmarshal(waitResult(t, manager.frames, "3", 5*time.Second), &stopped); err != nil || stopped.Running {
		t.Fatalf("status after shutdown preparation=%+v err=%v", stopped, err)
	}

	send(t, manager.stdin, 4, "engine:errors", nil)
	var reported struct {
		Errors []serviceError `json:"errors"`
	}
	if err := json.Unmarshal(waitResult(t, manager.frames, "4", 5*time.Second), &reported); err != nil {
		t.Fatalf("decode errors after shutdown preparation: %v", err)
	}
	if hasErr(reported.Errors, startFailedID("fake")) {
		t.Fatalf("shutdown cancellation retained a start-failed error: %+v", reported.Errors)
	}
	manager.stop(t)
}

type e2eManager struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	frames  chan frame
	stopped bool
}

func startE2EManager(t *testing.T, cfg, home string) *e2eManager {
	t.Helper()
	cmd := exec.Command(managerBin)
	cmd.Env = overrideEnv(map[string]string{"APPDATA": cfg, "LOCALAPPDATA": cfg, "XDG_CONFIG_HOME": cfg, "HOME": home})
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	manager := &e2eManager{cmd: cmd, stdin: stdin, frames: make(chan frame, 128)}
	t.Cleanup(func() {
		if !manager.stopped {
			_ = manager.stdin.Close()
			_ = manager.cmd.Process.Kill()
			_ = manager.cmd.Wait()
		}
	})
	go readFrames(stdout, manager.frames)
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
		if err != nil {
			t.Fatalf("manager exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = m.cmd.Process.Kill()
		<-done
		t.Fatal("manager did not exit after stdin closed")
	}
}

func writeFakeManifest(t *testing.T, dir string) {
	writeE2EManifest(t, dir, testEngineManifest(fakeEngineBin))
}

func writeE2EManifest(t *testing.T, dir string, manifest *Manifest) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fake.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
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

func readFrames(r io.Reader, out chan<- frame) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var f frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
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
	data, _ := json.Marshal(msg)
	if _, err := w.Write(append(data, '\n')); err != nil {
		t.Fatalf("send %s: %v", method, err)
	}
}

func notify(t *testing.T, w io.Writer, method string, params any) {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	if _, err := w.Write(append(data, '\n')); err != nil {
		t.Fatalf("notify %s: %v", method, err)
	}
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
			if len(f.Error) > 0 && string(f.Error) != "null" {
				t.Fatalf("rpc id %s returned error: %s", id, f.Error)
			}
			return f.Result
		case <-deadline:
			t.Fatalf("timed out waiting for response id %s", id)
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
			t.Fatalf("timed out waiting for notification %q", method)
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
	if err := json.Unmarshal(waitResult(t, manager.frames, "2", 5*time.Second), &launch); err != nil {
		t.Fatal(err)
	}
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
				if err := json.Unmarshal(f.Params, &relay); err != nil {
					t.Fatal(err)
				}
				waiting = false
			}
		case <-timer.C:
			t.Fatal("worker never requested parent rebind")
		}
	}
	if relay.Method != "rebind" || relay.Request.RequestID != "operation" || relay.Request.Settings.ProxyPort != 54301 {
		t.Fatalf("bad relay: %+v", relay)
	}
	send(t, manager.stdin, 4, "engine:describe", map[string]string{"engine": "fake"})
	waitResult(t, manager.frames, "4", 5*time.Second)
	notify(t, manager.stdin, "engine:settings-reply", settingsReply{ID: relay.ID, Result: json.RawMessage(`{}`)})
	if err := json.Unmarshal(waitResult(t, manager.frames, "3", 5*time.Second), &launch); err != nil || launch.Running {
		t.Fatalf("configure result %+v %v", launch, err)
	}
	saved := launch.LaunchText
	manager.stop(t)
	restored := startE2EManager(t, cfg, home)
	send(t, restored.stdin, 1, "engine:get-installed", nil)
	waitResult(t, restored.frames, "1", 5*time.Second)
	send(t, restored.stdin, 2, "engine:get-launch", map[string]string{"engine": "fake"})
	if err := json.Unmarshal(waitResult(t, restored.frames, "2", 5*time.Second), &launch); err != nil || launch.LaunchText != saved {
		t.Fatalf("worker reload lost literal settings: %+v %v", launch, err)
	}
	restored.stop(t)
}
