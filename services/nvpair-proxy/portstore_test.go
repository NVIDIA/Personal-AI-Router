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

// redirectConfigDir points os.UserConfigDir() at a temp dir for the test, so
// the persisted-port file doesn't touch the real per-user config. Sets all
// four env vars os.UserConfigDir() consults across platforms: XDG_CONFIG_HOME
// on Linux, $HOME/Library on macOS, and APPDATA/LOCALAPPDATA on Windows.
// Missing any one of them means the test reads and writes the developer's
// real file, clobbering their saved port and failing on the next run.
func redirectConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", dir)
}

// freeTCPPort returns a port that was free a moment ago.
//
// A bind probe cannot be held open and handed over, so the port is genuinely
// free when checked and may not be by the time a facade binds it. Callers that
// can tolerate a retry should use one — see enableOnFreePort in
// twofacade_test.go — because this helper cannot close that window.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Each engine persists to its own file, so one engine's saved port can never
// be read back as the other's.
func TestPersistedPortRoundTrip(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		redirectConfigDir(t)

		if _, ok := loadPersistedPort(tc.profile); ok {
			t.Fatal("expected no persisted port before any save")
		}
		if err := savePersistedPort(tc.profile, 11500); err != nil {
			t.Fatalf("savePersistedPort: %v", err)
		}
		if p, ok := loadPersistedPort(tc.profile); !ok || p != 11500 {
			t.Errorf("round-trip: got %d ok=%v, want 11500", p, ok)
		}

		// An out-of-range stored value is treated as "none" so startup falls
		// back to the flag/default rather than trying to bind port 0.
		path, err := proxyPortPath(tc.profile)
		if err != nil {
			t.Fatalf("proxyPortPath: %v", err)
		}
		if err := os.WriteFile(path, []byte(`{"port":0}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := loadPersistedPort(tc.profile); ok {
			t.Error("port 0 should be treated as none")
		}
	})
}

// The two engines must not share a persisted-port file, or moving one proxy
// would silently move the other on its next start.
func TestPersistedPortIsPerEngine(t *testing.T) {
	redirectConfigDir(t)

	ollama := ollamaCase(t).profile
	lmstudio := lmstudioCase(t).profile

	if err := savePersistedPort(ollama, 11500); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadPersistedPort(lmstudio); ok {
		t.Fatal("LM Studio read a port only Ollama saved")
	}
	if err := savePersistedPort(lmstudio, 1300); err != nil {
		t.Fatal(err)
	}
	if p, ok := loadPersistedPort(ollama); !ok || p != 11500 {
		t.Errorf("Ollama's port changed when LM Studio saved: got %d ok=%v", p, ok)
	}
}

// TestSetPortRebinds drives a live rebind: the proxy starts serving on one
// port, set-port moves it to another, and afterward the new port accepts
// connections, the old one doesn't, the choice is persisted, and a fresh
// ready notification carries the new port.
func TestSetPortRebinds(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		redirectConfigDir(t)

		buf := &bytes.Buffer{}
		codec := NewCodec(buf)
		disc := NewDiscovery()

		portA := freeTCPPort(t)
		proxy := newTestProxy(tc.profile, codec, disc, portA)

		lnA, err := net.Listen("tcp", fmt.Sprintf(":%d", portA))
		if err != nil {
			t.Fatalf("listen on port A: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		proxy.soleFacade().serveHTTP(ctx, lnA)
		defer proxy.shutdown(context.Background())

		portB := freeTCPPort(t)
		if err := proxy.soleFacade().setPort(portB); err != nil {
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
		if p, ok := loadPersistedPort(tc.profile); !ok || p != portB {
			t.Errorf("persisted port: got %d ok=%v, want %d", p, ok, portB)
		}

		// A fresh ready notification announced the new port.
		if !strings.Contains(buf.String(), fmt.Sprintf("\"port\":%d", portB)) {
			t.Errorf("expected ready notification carrying port %d, got %q", portB, buf.String())
		}
	})
}

// Persistence failure must leave the old listener serving and the new port free.
func TestSetPortPersistenceFailurePreservesListener(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		redirectConfigDir(t)
		portA := freeTCPPort(t)
		proxy := newTestProxy(tc.profile, NewCodec(&bytes.Buffer{}), NewDiscovery(), portA)
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", portA))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		proxy.soleFacade().serveHTTP(ctx, ln)
		defer proxy.shutdown(context.Background())
		path, err := proxyPortPath(tc.profile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		portB := freeTCPPort(t)
		if err := proxy.soleFacade().setPort(portB); err == nil {
			t.Fatal("persistence failure reported success")
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", portA), time.Second)
		if err != nil {
			t.Fatal("old listener lost", err)
		}
		_ = conn.Close()
		next, err := net.Listen("tcp", fmt.Sprintf(":%d", portB))
		if err != nil {
			t.Fatal("failed candidate listener leaked", err)
		}
		_ = next.Close()
	})
}
