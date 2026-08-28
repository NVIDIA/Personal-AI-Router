// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestChooseStartupPort(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flagPort  int
		ignore    bool
		persisted int
		has       bool
		want      int
	}{
		{"restores persisted by default", 11434, false, 11435, true, 11435},
		{"managed startup overrides persisted facade", 11435, true, 11434, true, 11435},
		{"flag used without persisted value", 11434, false, 0, false, 11434},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseStartupPort(tc.flagPort, tc.ignore, tc.persisted, tc.has); got != tc.want {
				t.Fatalf("chooseStartupPort() = %d, want %d", got, tc.want)
			}
		})
	}
}

// redirectConfigDir points os.UserConfigDir() at a temp dir for the test, so
// proxy-port.json reads/writes don't touch the real per-user config. Sets all
// three env vars os.UserConfigDir() consults across platforms: XDG_CONFIG_HOME
// on Linux, $HOME/Library on macOS, and APPDATA on Windows. Missing APPDATA
// meant the Windows-first-class path read/wrote the real %AppData% file —
// clobbering the user's saved port and making the test fail on repeat runs.
func redirectConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", dir)
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestPersistedPortRoundTrip(t *testing.T) {
	redirectConfigDir(t)

	if _, ok := loadPersistedPort(); ok {
		t.Fatal("expected no persisted port before any save")
	}
	if err := savePersistedPort(11500); err != nil {
		t.Fatalf("savePersistedPort: %v", err)
	}
	if p, ok := loadPersistedPort(); !ok || p != 11500 {
		t.Errorf("round-trip: got %d ok=%v, want 11500", p, ok)
	}

	// An out-of-range stored value is treated as "none" so startup falls
	// back to the flag/default rather than trying to bind port 0.
	path, err := proxyPortPath()
	if err != nil {
		t.Fatalf("proxyPortPath: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"port":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadPersistedPort(); ok {
		t.Error("port 0 should be treated as none")
	}
}

// TestSetPortRebinds drives a live rebind: the proxy starts serving on one
// port, set-port moves it to another, and afterward the new port accepts
// connections, the old one doesn't, the choice is persisted, and a fresh
// ready notification carries the new port.
func TestSetPortRebinds(t *testing.T) {
	redirectConfigDir(t)

	buf := &bytes.Buffer{}
	codec := NewCodec(buf)
	disc := NewDiscovery()

	portA := freeTCPPort(t)
	proxy := NewProxy(codec, disc, portA)

	lnA, err := net.Listen("tcp", fmt.Sprintf(":%d", portA))
	if err != nil {
		t.Fatalf("listen on port A: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy.serveHTTP(ctx, lnA)
	defer proxy.shutdown(context.Background())

	portB := freeTCPPort(t)
	if err := proxy.setPort(portB); err != nil {
		t.Fatalf("setPort: %v", err)
	}

	// New port is now serving.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", portB), 2*time.Second)
	if err != nil {
		t.Fatalf("new port %d not listening after rebind: %v", portB, err)
	}
	conn.Close()

	// Old port stopped accepting.
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", portA), 500*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("old port %d should be closed after rebind", portA)
	}

	// Persisted for next startup.
	if p, ok := loadPersistedPort(); !ok || p != portB {
		t.Errorf("persisted port: got %d ok=%v, want %d", p, ok, portB)
	}

	// A fresh ready notification announced the new port.
	if !strings.Contains(buf.String(), fmt.Sprintf("\"port\":%d", portB)) {
		t.Errorf("expected ready notification carrying port %d, got %q", portB, buf.String())
	}
}
