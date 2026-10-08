// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build live

// Live validation of the pound-build fixes against the REAL installed
// Ollama, with no download: it uses the bundled manifest and the engine's
// already-installed binary, spawning a fresh instance on a spare port so
// it never disturbs the Ollama already serving on 11434.
//
//	go test -tags live -run TestLivePoundFixesOllama -v -timeout 300s   # needs NVPAIR_LIVE_OLLAMA=1 + a real Ollama on :11434
package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLivePoundFixesOllama(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_OLLAMA") == "" {
		t.Skip("set NVPAIR_LIVE_OLLAMA=1 (needs a real Ollama already serving on :11434)")
	}
	if runtime.GOOS != "windows" {
		t.Skip("listener-address assertions use Windows Get-NetTCPConnection")
	}

	// #3 adoption: a fresh manager must report the already-running Ollama
	// (manifest port 11434) as running, without us ever starting it.
	func() {
		cfg := t.TempDir()
		frames, stdin, stop := startManager(t, map[string]string{"APPDATA": cfg, "XDG_CONFIG_HOME": cfg})
		defer stop()
		send(t, stdin, 1, "engine:get-installed", nil)
		r := string(waitResult(t, frames, "1", 10*time.Second))
		require.Contains(t, r, `"engine":"ollama"`, "#3 adoption: expected ollama running:true")
		require.Contains(t, r, `"running":true`, "#3 adoption: expected ollama running:true")
		t.Logf("#3 adoption OK: ollama reported running without a start")
	}()

	// Spawn tests: start on a spare port so the manager SPAWNS a fresh
	// instance (the adoption probe on the spare port finds nothing).
	spare, err := freePort()
	require.NoError(t, err)
	cfg := t.TempDir()
	frames, stdin, stop := startManager(t, map[string]string{"APPDATA": cfg, "XDG_CONFIG_HOME": cfg})
	defer stop()

	// Bind default: the bundled manifest now declares runtime.bind 127.0.0.1,
	// so the spawned engine listens on loopback only — never directly
	// LAN-reachable. Cluster peers reach it through the proxy's mTLS ingress.
	send(t, stdin, 1, "engine:start", map[string]any{"engine": "ollama", "port": spare})
	require.Contains(t, string(waitResult(t, frames, "1", 90*time.Second)), `"running":true`, "start{port}: expected running:true")
	if addrs := listenAddrs(t, spare); !isLoopbackOnly(addrs) {
		assert.Failf(t, "bind default: want loopback only (127.0.0.1/::1)", "got %v", addrs)
	} else {
		t.Logf("bind default OK: loopback only %v on %d", addrs, spare)
	}
	send(t, stdin, 2, "engine:stop", map[string]any{"engine": "ollama"})
	waitResult(t, frames, "2", 30*time.Second)
	waitGone(t, spare)

	// Bind override to loopback: spawned engine must listen on loopback only.
	send(t, stdin, 3, "engine:start", map[string]any{"engine": "ollama", "port": spare, "bind": "127.0.0.1"})
	require.Contains(t, string(waitResult(t, frames, "3", 90*time.Second)), `"running":true`, "start{port,bind}: expected running:true")
	if addrs := listenAddrs(t, spare); !isLoopbackOnly(addrs) {
		assert.Failf(t, "bind override: want loopback only (127.0.0.1/::1)", "got %v", addrs)
	} else {
		t.Logf("bind override OK: loopback only %v on %d", addrs, spare)
	}
	send(t, stdin, 4, "engine:stop", map[string]any{"engine": "ollama"})
	waitResult(t, frames, "4", 30*time.Second)
	waitGone(t, spare)

	// The pre-existing Ollama on 11434 must be untouched the whole time.
	if len(listenAddrs(t, 11434)) == 0 {
		assert.Fail(t, "the pre-existing Ollama on 11434 must remain running")
	} else {
		t.Logf("pre-existing Ollama on 11434 untouched")
	}
}

// listenAddrs returns every LocalAddress with a listener on port.
func listenAddrs(t *testing.T, port int) []string {
	t.Helper()
	out, _ := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-NetTCPConnection -LocalPort "+strconv.Itoa(port)+" -State Listen -ErrorAction SilentlyContinue | "+
			"Select-Object -ExpandProperty LocalAddress").Output()
	var addrs []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			addrs = append(addrs, s)
		}
	}
	return addrs
}

// isLoopbackOnly reports whether there is at least one listener and every
// listener is a loopback address.
func isLoopbackOnly(addrs []string) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if a != "127.0.0.1" && a != "::1" {
			return false
		}
	}
	return true
}

func waitGone(t *testing.T, port int) {
	t.Helper()
	for i := 0; i < 60; i++ {
		if len(listenAddrs(t, port)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.FailNowf(t, "port still listening after stop", "port %d", port)
}
