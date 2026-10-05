// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"

	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "reserve free port")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Each engine persists to its own file, so one engine's saved port can never
// be read back as the other's.
func TestPersistedPortRoundTrip(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		redirectConfigDir(t)

		{
			_, ok := loadPersistedPort(tc.profile)
			require.False(t, ok, "expected no persisted port before any save")
		}
		require.NoError(t, savePersistedPort(tc.profile, 11500), "savePersistedPort")
		{
			p, ok := loadPersistedPort(tc.profile)
			assert.True(t, ok, "round-trip: (%v, %v)", p, ok)
			assert.Equal(t, 11500, p, "round-trip: (%v, %v)", p, ok)
		}

		// An out-of-range stored value is treated as "none" so startup falls
		// back to the flag/default rather than trying to bind port 0.
		path, err := proxyPortPath(tc.profile)
		require.NoError(t, err, "proxyPortPath")
		require.NoError(t, os.WriteFile(path, []byte(`{"port":0}`), 0o644))
		{
			_, ok := loadPersistedPort(tc.profile)
			assert.False(t, ok, "port 0 should be treated as none")
		}
	})
}

// The two engines must not share a persisted-port file, or moving one proxy
// would silently move the other on its next start.
func TestPersistedPortIsPerEngine(t *testing.T) {
	redirectConfigDir(t)

	ollama := ollamaCase(t).profile
	lmstudio := lmstudioCase(t).profile

	require.NoError(t, savePersistedPort(ollama, 11500))
	{
		_, ok := loadPersistedPort(lmstudio)
		require.False(t, ok, "LM Studio read a port only Ollama saved")
	}
	require.NoError(t, savePersistedPort(lmstudio, 1300))
	{
		p, ok := loadPersistedPort(ollama)
		assert.True(t, ok, "Ollama's port changed when LM Studio saved: (%v, %v)", p, ok)
		assert.Equal(t, 11500, p, "Ollama's port changed when LM Studio saved: (%v, %v)", p, ok)
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
		require.NoError(t, err, "listen on port A")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		proxy.soleFacade().serveHTTP(ctx, lnA)
		defer proxy.shutdown(context.Background())

		portB := freeTCPPort(t)
		require.NoError(t, proxy.soleFacade().setPort(portB), "setPort")

		// New port is now serving.
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", portB), 2*time.Second)
		require.NoError(t, err, "new port (%v, %v)", portB, err)
		conn.Close()

		// Old port stopped accepting.
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", portA), 500*time.Millisecond); err == nil {
			c.Close()
			assert.Fail(t, "test expectation failed", "old port %d should be closed after rebind", portA)
		}

		// Persisted for next startup.
		{
			p, ok := loadPersistedPort(tc.profile)
			assert.True(t, ok, "persisted port: (%v, %v, %v)", p, ok, portB)
			assert.True(t, p == portB, "persisted port: (%v, %v, %v)", p, ok, portB)
		}

		// A fresh ready notification announced the new port.
		assert.Contains(t, buf.String(), fmt.Sprintf("\"port\":%d", portB), "expected ready notification carrying port (%v)", portB)
	})
}

// Persistence failure must leave the old listener serving and the new port free.
func TestSetPortPersistenceFailurePreservesListener(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		redirectConfigDir(t)
		portA := freeTCPPort(t)
		proxy := newTestProxy(tc.profile, NewCodec(&bytes.Buffer{}), NewDiscovery(), portA)
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", portA))
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		proxy.soleFacade().serveHTTP(ctx, ln)
		defer proxy.shutdown(context.Background())
		path, err := proxyPortPath(tc.profile)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(path, 0700))
		portB := freeTCPPort(t)
		require.Error(t, proxy.soleFacade().setPort(portB), "persistence failure reported success")
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", portA), time.Second)
		require.NoError(t, err, "old listener lost")
		_ = conn.Close()
		next, err := net.Listen("tcp", fmt.Sprintf(":%d", portB))
		require.NoError(t, err, "failed candidate listener leaked")
		_ = next.Close()
	})
}
