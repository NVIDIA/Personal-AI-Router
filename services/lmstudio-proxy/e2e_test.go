// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// proxyBin is the real lmstudio-proxy binary, built once in TestMain so the
// e2e test exercises the shipped artifact (not just in-process handlers).
var proxyBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "nvpair-lmproxy-e2e-*")
	if err != nil {
		panic(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	proxyBin = filepath.Join(tmp, "lmstudio-proxy"+suffix)
	if out, err := exec.Command("go", "build", "-o", proxyBin, ".").CombinedOutput(); err != nil {
		panic("build lmstudio-proxy: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

type e2eFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func e2eReadFrames(r io.Reader, out chan<- e2eFrame) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var f e2eFrame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			continue
		}
		out <- f
	}
}

func e2eSend(t *testing.T, w io.Writer, id int, method string, params any) {
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

func e2eWaitResult(t *testing.T, frames <-chan e2eFrame, id string, timeout time.Duration) {
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
			return
		case <-deadline:
			t.Fatalf("timed out waiting for response id %s", id)
		}
	}
}

func e2eWaitReadyPort(t *testing.T, frames <-chan e2eFrame, timeout time.Duration) int {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f := <-frames:
			if f.Method != "ready" {
				continue
			}
			var p struct {
				Port int `json:"port"`
			}
			if err := json.Unmarshal(f.Params, &p); err != nil {
				t.Fatalf("parse ready params: %v", err)
			}
			return p.Port
		case <-deadline:
			t.Fatalf("timed out waiting for ready notification")
			return 0
		}
	}
}

func e2eFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func e2eSplitHostPort(t *testing.T, serverURL string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", serverURL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}

// TestE2EFailoverOverRealBinary spawns the real lmstudio-proxy binary and
// drives it the way the broker/UI does: register a busy (503) and a healthy
// (200) upstream as manual nodes over JSON-RPC stdio, then send a genuine
// OpenAI inference POST to the proxy's real HTTP port. It asserts the request
// fails over from the busy node to the healthy one, the original body is
// replayed, and CORS headers are present — the whole shipped path (binary +
// stdio control plane + HTTP forwarding + failover) end-to-end, no mocks.
func TestE2EFailoverOverRealBinary(t *testing.T) {
	var gotBody string
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer busy.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer good.Close()

	port := e2eFreePort(t)
	cmd := exec.Command(proxyBin, "--port", strconv.Itoa(port))
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

	frames := make(chan e2eFrame, 256)
	go e2eReadFrames(stdout, frames)

	if got := e2eWaitReadyPort(t, frames, 10*time.Second); got != port {
		t.Fatalf("ready port = %d, want %d", got, port)
	}

	busyHost, busyPort := e2eSplitHostPort(t, busy.URL)
	goodHost, goodPort := e2eSplitHostPort(t, good.URL)
	e2eSend(t, stdin, 1, "node/add-manual", map[string]any{"id": "busy", "engine": "lmstudio", "host": busyHost, "port": busyPort, "addresses": []string{busyHost}, "models": []string{"m"}})
	e2eWaitResult(t, frames, "1", 5*time.Second)
	e2eSend(t, stdin, 2, "node/add-manual", map[string]any{"id": "good", "engine": "lmstudio", "host": goodHost, "port": goodPort, "addresses": []string{goodHost}, "models": []string{"m"}})
	e2eWaitResult(t, frames, "2", 5*time.Second)
	// Select the busy node so the failover path is deterministic.
	e2eSend(t, stdin, 3, "node/select", map[string]any{"id": "busy"})
	e2eWaitResult(t, frames, "3", 5*time.Second)

	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("inference POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (should fail over from the 503 node)", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if gotBody != `{"model":"m"}` {
		t.Errorf("healthy upstream got body %q, want the original request body", gotBody)
	}

	e2eSend(t, stdin, 9, "shutdown", nil)
	e2eWaitResult(t, frames, "9", 5*time.Second)
}
