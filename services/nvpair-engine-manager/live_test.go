// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build live

// Opt-in live validations against REAL engines. Excluded from the normal
// build/test (they require the `live` build tag), so the standard suite
// stays hermetic. Each installs an engine entirely through the service's
// own API, into a temp dir, then start → list models → stop → uninstall,
// proving the runner is engine-agnostic. Both use the no-elevation
// (no-UAC / headless) install path and an auto-assigned port, so they
// never touch any GUI-installed engine already on the machine.
//
//	go test -tags live -run TestLiveOllamaCleanRoom   -v -timeout 900s   # needs NVPAIR_LIVE_OLLAMA=1
//	go test -tags live -run TestLiveLMStudioCleanRoom -v -timeout 1200s  # needs NVPAIR_LIVE_LMSTUDIO=1
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestLiveOllamaCleanRoom installs NVPAIR's own standalone Ollama — the
// no-UAC zip (ollama-windows-amd64.zip), not the GUI OllamaSetup.exe —
// into a temp dir on an auto-assigned port, then exercises the full
// lifecycle via the API without disturbing any GUI Ollama on the box.
func TestLiveOllamaCleanRoom(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_OLLAMA") == "" {
		t.Skip("set NVPAIR_LIVE_OLLAMA=1 to run the Ollama standalone clean-room test")
	}
	goos := runtime.GOOS
	if goos != "windows" && goos != "linux" {
		t.Skipf("Ollama clean-room live test runs via `go test` on windows/linux; %s is covered by the stdio driver", goos)
	}
	platKey := goos + "/" + runtime.GOARCH

	// Per-OS standalone artifact + install/run/bin/uninstall — the same
	// shapes the bundled manifest declares, but pinned to the exact bytes
	// we serve so the checksum verifies (the bundled SHAs are placeholders).
	var artifact, bin string
	var runCmd, uninstallCmd []string
	env := map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}"}
	switch goos {
	case "windows":
		artifact = "ollama-windows-" + runtime.GOARCH + ".zip"
		runCmd = []string{"tar", "-xf", "{download}", "-C", "{install_dir}"}
		bin = `{install_dir}\ollama.exe`
		uninstallCmd = []string{"cmd", "/c", "rmdir", "/s", "/q", "{install_dir}"}
	case "linux":
		artifact = "ollama-linux-" + runtime.GOARCH + ".tar.zst"
		runCmd = []string{"tar", "--zstd", "-xf", "{download}", "-C", "{install_dir}"}
		bin = "{install_dir}/bin/ollama"
		env["LD_LIBRARY_PATH"] = "{install_dir}/lib/ollama"
		uninstallCmd = []string{"rm", "-rf", "{install_dir}"}
	}

	// Two modes. Default: serve a local copy with a computed SHA, so the
	// checksum-verify path is exercised. NVPAIR_LIVE_OLLAMA_UNPINNED=1: fetch
	// straight from the vendor over HTTPS with NO checksum — exactly what
	// the as-shipped bundled manifest now does (placeholder SHAs dropped).
	var fetchURL, sum string
	if os.Getenv("NVPAIR_LIVE_OLLAMA_UNPINNED") != "" {
		fetchURL = "https://ollama.com/download/" + artifact
		t.Logf("UNPINNED live install from %s (no checksum, as-shipped path)", fetchURL)
	} else {
		zipPath := os.Getenv("NVPAIR_LIVE_OLLAMA_ZIP")
		if zipPath == "" {
			zipPath = downloadTo(t, "https://ollama.com/download/"+artifact)
		}
		sum = sha256File(t, zipPath)
		t.Logf("standalone artifact %s sha256: %s", artifact, sum)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, zipPath)
		}))
		defer srv.Close()
		fetchURL = srv.URL + "/" + artifact
	}

	m := Manifest{
		Engine:          "ollama",
		DisplayName:     "Ollama (standalone live)",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			platKey: {
				// Clean room: detect only our temp dir, not a GUI install.
				Detect: []string{bin},
				Install: &Install{
					Fetch: &Fetch{URL: fetchURL, SHA256: sum},
					Run:   runCmd,
					Mode:  "user",
				},
				Uninstall: &Uninstall{Run: uninstallCmd},
				Runtime: Runtime{
					Bin:    bin,
					Args:   []string{"serve"},
					Env:    env,
					Port:   0, // auto-assign: never collide with a GUI Ollama on 11434
					Ready:  &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 60},
					Stop:   &StopSpec{Signal: "term", GraceS: 5},
					Health: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, IntervalS: 5},
				},
			},
		},
		Actions: map[string]Action{
			"list_models":  {HTTP: &ActionHTTP{Method: "GET", Path: "/api/tags"}},
			"pull_model":   {HTTP: &ActionHTTP{Method: "POST", Path: "/api/pull"}},
			"run_model":    {HTTP: &ActionHTTP{Method: "POST", Path: "/api/generate"}},
			"delete_model": {HTTP: &ActionHTTP{Method: "DELETE", Path: "/api/delete"}},
		},
	}

	frames, stdin, stop := startManagerWithManifest(t, m)
	defer stop()

	send(t, stdin, 1, "engine:get-installed", nil)
	if r := waitResult(t, frames, "1", 5*time.Second); !strings.Contains(string(r), `"engine":"ollama"`) {
		t.Fatalf("ollama not listed: %s", r)
	}
	send(t, stdin, 2, "engine:install", map[string]string{"engine": "ollama"})
	if r := waitResult(t, frames, "2", 600*time.Second); !strings.Contains(string(r), `"installed":true`) {
		t.Errorf("expected installed:true after install, got %s", r)
	}
	send(t, stdin, 3, "engine:start", map[string]string{"engine": "ollama"})
	if r := waitResult(t, frames, "3", 90*time.Second); !strings.Contains(string(r), `"running":true`) {
		t.Errorf("expected running:true after start, got %s", r)
	}
	send(t, stdin, 4, "engine:action", map[string]any{"engine": "ollama", "action": "list_models"})
	pre := waitResult(t, frames, "4", 30*time.Second)

	// Opt-in full model lifecycle against the REAL engine:
	// list -> pull -> list -> run(generate) -> delete -> list. Gated (a real
	// multi-hundred-MB download) and guarded: we only delete a model we
	// pulled ourselves, never one already in the shared ~/.ollama store.
	if os.Getenv("NVPAIR_LIVE_OLLAMA_MODELS") != "" {
		model := os.Getenv("NVPAIR_LIVE_OLLAMA_MODEL")
		if model == "" {
			model = "smollm2:135m" // tiny (~270 MB)
		}
		preexisting := strings.Contains(string(pre), model)
		send(t, stdin, 41, "engine:action", map[string]any{"engine": "ollama", "action": "pull_model", "params": map[string]string{"name": model}})
		waitResult(t, frames, "41", 1200*time.Second)
		send(t, stdin, 42, "engine:action", map[string]any{"engine": "ollama", "action": "list_models"})
		if r := waitResult(t, frames, "42", 30*time.Second); !strings.Contains(string(r), model) {
			t.Errorf("model %q not listed after pull: %s", model, r)
		}
		send(t, stdin, 43, "engine:action", map[string]any{"engine": "ollama", "action": "run_model", "params": map[string]any{"model": model, "prompt": "Say OK.", "stream": false}})
		if r := waitResult(t, frames, "43", 180*time.Second); !strings.Contains(string(r), `"response"`) {
			t.Errorf("no response from run_model: %s", r)
		}
		if preexisting {
			t.Logf("model %q pre-existed in the shared store; leaving it (only deleting models we pull)", model)
		} else {
			send(t, stdin, 44, "engine:action", map[string]any{"engine": "ollama", "action": "delete_model", "params": map[string]string{"name": model}})
			waitResult(t, frames, "44", 60*time.Second)
			send(t, stdin, 45, "engine:action", map[string]any{"engine": "ollama", "action": "list_models"})
			if r := waitResult(t, frames, "45", 30*time.Second); strings.Contains(string(r), model) {
				t.Errorf("model %q still listed after delete: %s", model, r)
			}
		}
	}
	send(t, stdin, 5, "engine:stop", map[string]string{"engine": "ollama"})
	waitResult(t, frames, "5", 30*time.Second)
	send(t, stdin, 6, "engine:uninstall", map[string]string{"engine": "ollama"})
	waitResult(t, frames, "6", 60*time.Second)
	send(t, stdin, 7, "shutdown", nil)
	waitResult(t, frames, "7", 5*time.Second)
}

// TestLiveLMStudioCleanRoom is the genericness proof for a daemon +
// control-CLI engine: LM Studio isn't installed, so the whole lifecycle
// — install (vendor script), start (daemon via `lms`), list models
// (HTTP), stop, uninstall — runs entirely through the API against the
// bundled manifest, with no LM-Studio-specific code.
func TestLiveLMStudioCleanRoom(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_LMSTUDIO") == "" {
		t.Skip("set NVPAIR_LIVE_LMSTUDIO=1 to run the LM Studio clean-room install test (this installs LM Studio)")
	}
	// The bundled manifest installs into (and uninstalls) the real
	// ~/.lmstudio. Refuse to run if one already exists, so we never delete
	// a user's pre-existing LM Studio.
	if home, _ := os.UserHomeDir(); home != "" {
		if _, err := os.Stat(filepath.Join(home, ".lmstudio")); err == nil {
			t.Skip("~/.lmstudio already exists; skipping so we don't uninstall a real LM Studio install")
		}
	}

	// Empty config dir so only the bundled lmstudio manifest is loaded.
	cfg := t.TempDir()
	frames, stdin, stop := startManager(t, map[string]string{"APPDATA": cfg, "XDG_CONFIG_HOME": cfg})
	defer stop()

	send(t, stdin, 1, "engine:get-installed", nil)
	if r := waitResult(t, frames, "1", 5*time.Second); !strings.Contains(string(r), `"engine":"lmstudio"`) {
		t.Fatalf("lmstudio not listed: %s", r)
	}
	send(t, stdin, 2, "engine:install", map[string]string{"engine": "lmstudio"})
	if r := waitResult(t, frames, "2", 600*time.Second); !strings.Contains(string(r), `"installed":true`) {
		t.Errorf("expected installed:true after install, got %s", r)
	}
	send(t, stdin, 3, "engine:start", map[string]string{"engine": "lmstudio"})
	waitResult(t, frames, "3", 120*time.Second)
	send(t, stdin, 4, "engine:action", map[string]any{"engine": "lmstudio", "action": "list_models"})
	waitResult(t, frames, "4", 30*time.Second)
	send(t, stdin, 5, "engine:stop", map[string]string{"engine": "lmstudio"})
	waitResult(t, frames, "5", 30*time.Second)
	send(t, stdin, 6, "engine:uninstall", map[string]string{"engine": "lmstudio"})
	waitResult(t, frames, "6", 60*time.Second)
	send(t, stdin, 7, "shutdown", nil)
	waitResult(t, frames, "7", 5*time.Second)
}

// startManager spawns the manager binary with env overrides and returns
// a frame stream, its stdin, and a cleanup func. It logs every frame.
func startManager(t *testing.T, env map[string]string) (chan frame, io.WriteCloser, func()) {
	t.Helper()
	cmd := exec.Command(managerBin)
	cmd.Env = overrideEnv(env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	frames := make(chan frame, 256)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			var f frame
			_ = json.Unmarshal([]byte(line), &f)
			t.Logf("[%6dms] %s", time.Since(start).Milliseconds(), line)
			frames <- f
		}
	}()
	waitNotify(t, frames, "engine:ready", 5*time.Second)
	cleanup := func() {
		// Closing stdin is EOF to the manager, which runs StopAll (stops
		// the spawned engine) before exiting. Give it a moment to do that
		// gracefully, then force-kill so a test can't hang/orphan.
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return frames, stdin, cleanup
}

// startManagerWithManifest writes m into a temp config dir, then spawns
// the manager pointed at it (so the override manifest shadows bundled).
func startManagerWithManifest(t *testing.T, m Manifest) (chan frame, io.WriteCloser, func()) {
	t.Helper()
	cfg := t.TempDir()
	engdir := filepath.Join(cfg, configSubdir, "engines")
	if err := os.MkdirAll(engdir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(engdir, m.Engine+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return startManager(t, map[string]string{"APPDATA": cfg, "XDG_CONFIG_HOME": cfg})
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func downloadTo(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.CreateTemp(t.TempDir(), "engine-dl-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}
