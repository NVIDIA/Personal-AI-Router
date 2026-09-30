// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	"nvpair-shared/jsonrpc"
)

// safeBuffer is a bytes.Buffer guarded by a mutex so it can be read while a
// goroutine is still writing to it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// startProxyWithLog is like startProxy but routes stderr to a buffer and
// accepts an explicit --log-level flag for the child process.
func startProxyWithLog(t *testing.T, level string) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, stderr *safeBuffer, cleanup func()) {
	t.Helper()

	// No engine on argv: the binary starts with no facade and no listener, and
	// takes engines over facade/enable. Log level is process-scoped, so this
	// needs no facade at all.
	cmd := exec.Command(proxyBin, "--log-level", level)
	stderrBuf := &safeBuffer{}
	cmd.Stderr = stderrBuf

	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err, "proxy stdin pipe")
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "proxy stdout pipe")
	require.NoError(t, cmd.Start(), "start proxy")
	t.Logf("proxy started: pid=%d log-level=%s", cmd.Process.Pid, level)

	ch := startMsgReader(t, stdoutPipe)

	// Enable a facade so the process behaves like one the broker brought up:
	// it binds, announces ready, and starts its background work, which is what
	// produces the startup log lines this test counts. Which engine is
	// immaterial to log-level plumbing.
	//
	// The response is consumed here rather than left in the stream, so this
	// returns a proxy that is already up and the caller's first waitForResponse
	// still belongs to the caller's own request.
	const enableID = 900
	sendLine(t, stdinPipe, map[string]any{
		"jsonrpc": "2.0",
		"id":      enableID,
		"method":  "facade/enable",
		"params": map[string]any{
			"engine":              "ollama",
			"port":                freePort(t),
			"ignorePersistedPort": true,
		},
	})
	deadline := time.After(15 * time.Second)
	for enabled := false; !enabled; {
		select {
		case m, ok := <-ch:
			require.True(t, ok, "proxy stdout closed before facade/enable answered")
			if m.Method == "" && m.ID != nil && string(*m.ID) == fmt.Sprint(enableID) {
				require.Nil(t, m.Error, "facade/enable failed")
				enabled = true
			}
		case <-deadline:
			require.FailNow(t, "timed out waiting for facade/enable")
		}
	}

	return stdinPipe, ch, stderrBuf, func() {
		stdinPipe.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
}

// sendLine marshals msg to JSON and writes a single newline-terminated frame.
func sendLine(t *testing.T, w io.Writer, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err, "marshal")
	data = append(data, '\n')
	_, err = w.Write(data)
	require.NoError(t, err, "write")
}

// countLogLinesAtLevel counts stderr lines whose level tag matches `tag`
// (e.g. "DEBUG", "INFO"). The prefix handler emits `HH:MM:SS.mmm [name] TAG msg ...`.
func countLogLinesAtLevel(stderr string, tag string) int {
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(stderr))
	for scanner.Scan() {
		line := scanner.Text()
		// Lines look like: "15:04:05.000 [ollama-proxy] DEBUG ..."
		if strings.Contains(line, "] "+tag+" ") {
			count++
		}
	}
	return count
}

// TestLogSetLevelViaRPC verifies the log/set-level JSON-RPC contract and that
// lowering the level at runtime silences debug/info output.
func TestLogSetLevelViaRPC(t *testing.T) {
	stdin, msgs, stderr, cleanup := startProxyWithLog(t, "debug")
	t.Cleanup(cleanup)

	// startProxyWithLog already waited out facade/enable, so the proxy is up
	// and has emitted its startup lines at info (plus debug ones, because we
	// booted at debug).
	//
	// Give the proxy a moment to emit a few debug lines (the first mDNS
	// scan runs immediately in Discovery.Run).
	time.Sleep(2 * time.Second)

	initialOutput := stderr.String()
	initialDebug := countLogLinesAtLevel(initialOutput, "DEBUG")
	initialInfo := countLogLinesAtLevel(initialOutput, "INFO")
	require.NotEqual(t, 0, initialInfo, "expected at least one INFO line at startup (%v)", initialOutput)
	t.Logf("startup: %d INFO lines, %d DEBUG lines", initialInfo, initialDebug)

	// Send log/set-level as a REQUEST and verify response payload.
	reqID := json.RawMessage(`42`)
	sendLine(t, stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  "log/set-level",
		"params":  map[string]string{"level": "error"},
	})
	resp := waitForResponse(t, msgs, 2*time.Second)
	require.Equal(t, "42", string(*resp.ID), "response ID")
	var result struct {
		Level string `json:"level"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &result), "unmarshal result")
	require.Equal(t, "error", result.Level)

	// Clear the buffer, wait long enough for at least one more mDNS scan
	// (which would have emitted DEBUG lines previously), and confirm that
	// no debug/info lines show up.
	time.Sleep(200 * time.Millisecond) // let the "log level changed" INFO line settle
	stderr.Reset()
	time.Sleep(6 * time.Second)
	postSwitch := stderr.String()

	assert.Equal(t, 0, countLogLinesAtLevel(postSwitch, "DEBUG"), "expected 0 DEBUG lines after lowering to error, output: %s", postSwitch)
	assert.Equal(t, 0, countLogLinesAtLevel(postSwitch, "INFO"), "expected 0 INFO lines after lowering to error, output: %s", postSwitch)

	// Invalid level should produce a JSON-RPC error response.
	sendLine(t, stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(`43`),
		"method":  "log/set-level",
		"params":  map[string]string{"level": "bogus"},
	})
	errResp := waitForResponse(t, msgs, 2*time.Second)
	require.Equal(t, "43", string(*errResp.ID), "error response ID")
	assert.False(t, len(errResp.Result) != 0 && string(errResp.Result) != "null", "expected no result on bogus level")
}

// TestLogLevelEnvFallback verifies NVPAIR_LOG_LEVEL is honoured when --log-level
// is not passed.
func TestLogLevelEnvFallback(t *testing.T) {
	// --log-level deliberately omitted, so the env fallback is what this
	// exercises. No engine needed: log level is process-scoped.
	cmd := exec.Command(proxyBin)
	cmd.Env = append(cmd.Environ(), "NVPAIR_LOG_LEVEL=debug")
	stderrBuf := &safeBuffer{}
	cmd.Stderr = stderrBuf

	stdinPipe, err := cmd.StdinPipe()
	require.NoError(t, err, "stdin pipe")
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "stdout pipe")
	require.NoError(t, cmd.Start(), "start")
	msgs := startMsgReader(t, stdoutPipe)
	t.Cleanup(func() {
		stdinPipe.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	})

	// Enable a facade so the process reaches ready, the same way the broker
	// brings one up. The env-derived level applies from process init, before
	// any facade exists.
	sendLine(t, stdinPipe, map[string]any{
		"jsonrpc": "2.0",
		"id":      901,
		"method":  "facade/enable",
		"params": map[string]any{
			"engine":              "ollama",
			"port":                freePort(t),
			"ignorePersistedPort": true,
		},
	})

	// A facade announces itself addressed to its engine, so the broker can tell
	// which one is ready when a process hosts several.
	waitForMethod(t, msgs, engines.AddressMethod("ollama", "ready"), 5*time.Second)
	// The first mDNS scan browse runs with a 3s timeout before emitting
	// its DEBUG summary; wait longer than that to guarantee we see at
	// least one DEBUG line.
	time.Sleep(5 * time.Second)

	out := stderrBuf.String()
	assert.NotEqual(t, 0, countLogLinesAtLevel(out, "DEBUG"), "expected DEBUG lines with NVPAIR_LOG_LEVEL=debug, got none. output (%v)", out)
}
