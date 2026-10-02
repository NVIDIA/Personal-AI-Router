// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// foreignStopDeclinedRe matches engine-manager's warning for the foreign
// engine, whose stop it can only decline. One sweep logs it exactly once, so
// counting the line counts the sweeps. It names the engine because a real
// engine on the host can be adopted and declined in the same sweep.
var foreignStopDeclinedRe = regexp.MustCompile(`engine stop during shutdown failed.*engine=foreign`)

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
			"status":    http.StatusOK,
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
		t.Fatalf("encode foreign engine manifest: %v", err)
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
			t.Fatalf("create engine manifest dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "foreign.json"), data, 0o644); err != nil {
			t.Fatalf("write foreign engine manifest: %v", err)
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
// CI machine. An engine whose stop can only be declined stays marked running,
// so every repeat sweep would re-pay its readiness probe on the quit path. The
// elapsed teardown is logged for reference.
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
	if resp := waitForResponseID(t, msgs, 98, 15*time.Second); resp.Error != nil {
		t.Fatalf("engine:get-installed errored: code=%d msg=%s", resp.Error.Code, resp.Error.Message)
	}

	// The desktop's quit sequence.
	started := time.Now()
	sendReq(t, stdin, 99, "engine:prepare-shutdown")
	sweeps := quitAndCountStderr(t, msgs, stderrLines, foreignStopDeclinedRe, stdin, 99, 30*time.Second)
	elapsed := time.Since(started)

	t.Logf("teardown took %s with %d declined-stop sweep(s)", elapsed, sweeps)

	if sweeps == 0 {
		t.Fatal("engine-manager never declined a stop; the foreign engine was not adopted, so this exercised nothing")
	}
	if sweeps > 1 {
		t.Fatalf("engine sweep ran %d times during one quit, want 1; each repeat re-pays the stop probe and is what made quitting slow", sweeps)
	}
}

// quitAndCountStderr completes the desktop's quit sequence and counts stderr
// lines matching re until the broker exits. Like the desktop, it closes stdin
// only once the engine:prepare-shutdown reply (replyID) has arrived. Both
// streams are drained throughout, so the broker never blocks writing to either.
func quitAndCountStderr(t *testing.T, msgs <-chan jsonrpc.Message, lines <-chan string, re *regexp.Regexp, stdin io.Closer, replyID int, timeout time.Duration) int {
	t.Helper()
	replied := false
	count := 0
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				msgs = nil
				continue
			}
			if replied || msg.Method != "" || !idEquals(msg.ID, replyID) {
				continue
			}
			if msg.Error != nil {
				t.Fatalf("engine:prepare-shutdown errored: code=%d msg=%s", msg.Error.Code, msg.Error.Message)
			}
			replied = true
			if err := stdin.Close(); err != nil {
				t.Fatalf("close broker stdin: %v", err)
			}
		case line, ok := <-lines:
			if !ok {
				if !replied {
					t.Fatalf("broker exited before replying to engine:prepare-shutdown; counted %d sweep(s)", count)
				}
				return count
			}
			if re.MatchString(line) {
				count++
				t.Logf("declined stop #%d: %s", count, line)
			}
		case <-timer.C:
			t.Fatalf("timed out (%s) waiting for the broker to exit; replied=%t, counted %d sweep(s)", timeout, replied, count)
		}
	}
}
