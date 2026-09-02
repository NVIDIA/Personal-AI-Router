// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// engineStopDeclinedRe matches engine-manager's warning for an engine whose
// stop it can only decline. One sweep logs it exactly once per such engine, so
// counting the line counts the sweeps.
var engineStopDeclinedRe = regexp.MustCompile(`engine stop during shutdown failed`)

// startForeignEngine serves an engine readiness endpoint on a free port without
// engine-manager having launched it. That is what an externally-managed engine
// looks like from the inside: the readiness probe succeeds, so the engine is
// adopted as running, but nothing engine-manager owns can stop it, so its stop
// is declined and it stays marked running.
func startForeignEngine(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for foreign engine: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	port := ln.Addr().(*net.TCPAddr).Port
	t.Logf("foreign engine listening on 127.0.0.1:%d", port)
	return port
}

// writeForeignEngineManifest declares a process-mode engine pinned to port with
// an HTTP readiness probe. A process-mode engine is reconciled from its
// readiness endpoint alone, so it is adopted without any installed binary —
// which is the point: nothing here is ours to stop.
//
// The runtime goes in a host platform block, not just at the top level: an
// engine with no block for this os/arch has no per-engine state, so it would
// never be reconciled at all.
func writeForeignEngineManifest(t *testing.T, configDir string, port int) {
	t.Helper()
	runtimeSpec := map[string]any{
		"mode": "process",
		"bin":  filepath.Join(t.TempDir(), "not-ours"),
		"port": port,
		"ready": map[string]any{
			"http":      fmt.Sprintf("http://127.0.0.1:%d/", port),
			"status":    200,
			"timeout_s": 10,
		},
		"stop": map[string]any{"signal": "term", "grace_s": 3},
	}
	manifest := map[string]any{
		"engine":           "foreign",
		"display_name":     "Foreign Engine",
		"manifest_version": 1,
		"platforms": map[string]any{
			runtime.GOOS + "/" + runtime.GOARCH: map[string]any{"runtime": runtimeSpec},
		},
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// The per-user manifest dir differs by platform, and the broker points every
	// worker at one disposable root, so write both candidates rather than
	// branching on GOOS: the Windows %LocalAppData% / Linux $XDG_CONFIG_HOME
	// layout, and the macOS Application Support layout under $HOME.
	for _, dir := range []string{
		filepath.Join(configDir, "Nvidia Corporation", "Personal AI Router", "engines"),
		filepath.Join(configDir, "Library", "Application Support", "Nvidia Corporation", "Personal AI Router", "engines"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "foreign.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestQuitSweepsEnginesOnce is the end-to-end guard for quit latency, driving
// the real broker and engine-manager through the exact sequence the desktop
// uses on quit: engine:prepare-shutdown, then close stdin.
//
// Three separate paths ask engine-manager to stop its engines on that sequence
// — the desktop's engine:prepare-shutdown, the broker's own teardown call, and
// engine-manager's stdin-EOF path. Each is the right trigger for a different
// way of being shut down, so all three stay; the sweep behind them must run
// once.
//
// This counts sweeps rather than milliseconds so it cannot flake on a loaded
// CI machine. The cost is what makes it matter: an engine whose stop can only
// be declined stays marked running, so every repeat sweep re-pays its readiness
// probe. Field measurement with an externally-managed Ollama put the three
// sweeps at 1068ms + 1052ms + 1040ms — essentially the entire quit. The elapsed
// teardown is logged for reference.
func TestQuitSweepsEnginesOnce(t *testing.T) {
	configDir := t.TempDir()
	port := startForeignEngine(t)
	writeForeignEngineManifest(t, configDir, port)

	stdin, msgs, stderrLines, cleanup := startBrokerWithConfigDir(t, configDir,
		"--engine-manager-path", engineMgrBin)
	defer cleanup()

	waitForMethod(t, msgs, "app:ready", 15*time.Second)

	// engine:get-installed reconciles every engine's presence, which is what
	// adopts the foreign listener as running. Without it there is nothing for a
	// sweep to decline and the test would pass while covering nothing.
	sendReq(t, stdin, 98, "engine:get-installed")
	waitForResponseID(t, msgs, 98, 15*time.Second)

	// The desktop's quit sequence.
	started := time.Now()
	sendReq(t, stdin, 99, "engine:prepare-shutdown")
	sweeps := countStderrUntilClosed(t, stderrLines, engineStopDeclinedRe, stdin, 30*time.Second)
	elapsed := time.Since(started)

	t.Logf("teardown took %s with %d declined-stop sweep(s)", elapsed, sweeps)

	if sweeps == 0 {
		t.Fatal("engine-manager never declined a stop; the foreign engine was not adopted, so this exercised nothing")
	}
	if sweeps > 1 {
		t.Fatalf("engine sweep ran %d times during one quit, want 1; each repeat re-pays the stop probe and is what made quitting slow", sweeps)
	}
}

// countStderrUntilClosed closes the broker's stdin once (completing the quit
// sequence) and counts matching stderr lines until the stream closes with the
// process. Returns the count.
func countStderrUntilClosed(t *testing.T, lines <-chan string, re *regexp.Regexp, stdin interface{ Close() error }, timeout time.Duration) int {
	t.Helper()
	closed := false
	count := 0
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	// Give prepare-shutdown a moment to be handled before EOF, matching the
	// desktop, which awaits its reply first.
	settle := time.NewTimer(2 * time.Second)
	defer settle.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return count
			}
			if re.MatchString(line) {
				count++
				t.Logf("declined stop #%d: %s", count, line)
			}
		case <-settle.C:
			if !closed {
				closed = true
				_ = stdin.Close()
			}
		case <-timer.C:
			t.Fatalf("timed out (%s) waiting for the broker to exit; counted %d sweep(s)", timeout, count)
		}
	}
}
