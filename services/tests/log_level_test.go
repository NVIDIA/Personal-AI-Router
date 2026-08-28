// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

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

	cmd := exec.Command(proxyBin, "--log-level", level)
	stderrBuf := &safeBuffer{}
	cmd.Stderr = stderrBuf

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("proxy stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("proxy stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	t.Logf("proxy started: pid=%d log-level=%s", cmd.Process.Pid, level)

	ch := startMsgReader(stdoutPipe)

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
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
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

	// Wait for ready (emitted at startup at info; also discovery/startup
	// emits debug lines because we booted at debug).
	waitForMethod(t, msgs, "ready", 5*time.Second)

	// Give the proxy a moment to emit a few debug lines (the first mDNS
	// scan runs immediately in Discovery.Run).
	time.Sleep(2 * time.Second)

	initialOutput := stderr.String()
	initialDebug := countLogLinesAtLevel(initialOutput, "DEBUG")
	initialInfo := countLogLinesAtLevel(initialOutput, "INFO")
	if initialInfo == 0 {
		t.Fatalf("expected at least one INFO line at startup, got:\n%s", initialOutput)
	}
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
	if string(*resp.ID) != "42" {
		t.Fatalf("response ID = %s, want 42", string(*resp.ID))
	}
	var result struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v (raw=%s)", err, string(resp.Result))
	}
	if result.Level != "error" {
		t.Fatalf("result.level = %q, want %q", result.Level, "error")
	}

	// Clear the buffer, wait long enough for at least one more mDNS scan
	// (which would have emitted DEBUG lines previously), and confirm that
	// no debug/info lines show up.
	time.Sleep(200 * time.Millisecond) // let the "log level changed" INFO line settle
	stderr.Reset()
	time.Sleep(6 * time.Second)
	postSwitch := stderr.String()

	if got := countLogLinesAtLevel(postSwitch, "DEBUG"); got != 0 {
		t.Errorf("expected 0 DEBUG lines after lowering to error, got %d. output:\n%s", got, postSwitch)
	}
	if got := countLogLinesAtLevel(postSwitch, "INFO"); got != 0 {
		t.Errorf("expected 0 INFO lines after lowering to error, got %d. output:\n%s", got, postSwitch)
	}

	// Invalid level should produce a JSON-RPC error response.
	sendLine(t, stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(`43`),
		"method":  "log/set-level",
		"params":  map[string]string{"level": "bogus"},
	})
	errResp := waitForResponse(t, msgs, 2*time.Second)
	if string(*errResp.ID) != "43" {
		t.Fatalf("error response ID = %s, want 43", string(*errResp.ID))
	}
	if len(errResp.Result) != 0 && string(errResp.Result) != "null" {
		t.Errorf("expected no result on bogus level, got %s", string(errResp.Result))
	}
}

// TestLogLevelEnvFallback verifies NVPAIR_LOG_LEVEL is honoured when --log-level
// is not passed.
func TestLogLevelEnvFallback(t *testing.T) {
	cmd := exec.Command(proxyBin)
	cmd.Env = append(cmd.Environ(), "NVPAIR_LOG_LEVEL=debug")
	stderrBuf := &safeBuffer{}
	cmd.Stderr = stderrBuf

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	msgs := startMsgReader(stdoutPipe)
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

	waitForMethod(t, msgs, "ready", 5*time.Second)
	// The first mDNS scan browse runs with a 3s timeout before emitting
	// its DEBUG summary; wait longer than that to guarantee we see at
	// least one DEBUG line.
	time.Sleep(5 * time.Second)

	out := stderrBuf.String()
	if countLogLinesAtLevel(out, "DEBUG") == 0 {
		t.Errorf("expected DEBUG lines with NVPAIR_LOG_LEVEL=debug, got none. output:\n%s", out)
	}
}
