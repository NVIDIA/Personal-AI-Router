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
	"sync"
	"testing"
	"time"

	"nvpair-shared/engines"
)

// The real binary, built on first use rather than in TestMain.
//
// A TestMain that builds unconditionally charges the go build to all ~130
// tests in this package, and moving the whole file behind a build tag —  the
// obvious alternative — would take the only end-to-end test of the shipped
// artifact out of the CI gate entirely, in the change that replaces that
// artifact. A sync.Once pays the cost once, only when an e2e case runs.
var (
	proxyBinOnce sync.Once
	proxyBinPath string
	proxyBinErr  error
)

func proxyBinary(t *testing.T) string {
	t.Helper()
	proxyBinOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "nvpair-proxy-e2e-*")
		if err != nil {
			proxyBinErr = err
			return
		}
		suffix := ""
		if runtime.GOOS == "windows" {
			suffix = ".exe"
		}
		proxyBinPath = filepath.Join(tmp, "nvpair-proxy"+suffix)
		if out, err := exec.Command("go", "build", "-o", proxyBinPath, ".").CombinedOutput(); err != nil {
			proxyBinErr = fmt.Errorf("build nvpair-proxy: %w\n%s", err, out)
		}
	})
	if proxyBinErr != nil {
		t.Fatal(proxyBinErr)
	}
	return proxyBinPath
}

type e2eFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func e2eReadFrames(r io.Reader, out chan<- e2eFrame) {
	defer close(out)
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

// e2eInbox retains interleaved replies and notifications for later waits.
// All waits for a child process share one inbox and run on the test goroutine.
type e2eInbox struct {
	frames  <-chan e2eFrame
	pending []e2eFrame
}

func (in *e2eInbox) wait(t *testing.T, match func(e2eFrame) bool, description string, timeout time.Duration) e2eFrame {
	t.Helper()
	for i, f := range in.pending {
		if match(f) {
			in.pending = append(in.pending[:i], in.pending[i+1:]...)
			return f
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-in.frames:
			if !ok {
				t.Fatalf("stream closed waiting for %s", description)
			}
			if match(f) {
				return f
			}
			in.pending = append(in.pending, f)
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func e2eWaitResult(t *testing.T, frames *e2eInbox, id string, timeout time.Duration) {
	t.Helper()
	f := frames.wait(t, func(f e2eFrame) bool { return string(f.ID) == id }, "response id "+id, timeout)
	if len(f.Error) > 0 && string(f.Error) != "null" {
		t.Fatalf("rpc id %s returned error: %s", id, f.Error)
	}
}

// e2eEnableWithRetry enables a facade, trying a fresh port whenever the child
// reports a lost bind race, and returns the port it actually bound.
func e2eEnableWithRetry(t *testing.T, stdin io.Writer, frames *e2eInbox, engine string, port int) int {
	t.Helper()
	for attempt := 0; attempt < 8; attempt++ {
		id := 100 + attempt
		e2eSend(t, stdin, id, "facade/enable", map[string]any{
			"engine":              engine,
			"port":                port,
			"ignorePersistedPort": true,
		})
		bound, bindRace := e2eEnabledPort(t, frames, fmt.Sprint(id), 10*time.Second)
		if bindRace {
			port = e2eFreePort(t)
			continue
		}
		if bound != port {
			t.Fatalf("enabled port = %d, want %d", bound, port)
		}
		return port
	}
	t.Fatalf("enable %s: every probed port was taken before the child could bind", engine)
	return 0
}

// e2eEnabledPort reads the port a facade/enable bound, reporting bindRace when
// the child refused because the port was taken. Only a bind race is retryable;
// any other rejection is a real failure.
func e2eEnabledPort(t *testing.T, frames *e2eInbox, id string, timeout time.Duration) (port int, bindRace bool) {
	t.Helper()
	f := frames.wait(t, func(f e2eFrame) bool { return string(f.ID) == id }, "facade/enable response id "+id, timeout)
	if len(f.Error) > 0 && string(f.Error) != "null" {
		var rpcErr struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(f.Error, &rpcErr) == nil && rpcErr.Code == codeFacadeBindFailed {
			return 0, true
		}
		t.Fatalf("facade/enable returned error: %s", f.Error)
	}
	var res struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil {
		t.Fatalf("parse facade/enable result: %v", err)
	}
	return res.Port, false
}

func e2eWaitReadyPort(t *testing.T, frames *e2eInbox, timeout time.Duration) int {
	t.Helper()
	f := frames.wait(t, func(f e2eFrame) bool { return f.Method == "ready" }, "ready notification", timeout)
	var p struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(f.Params, &p); err != nil {
		t.Fatalf("parse ready params: %v", err)
	}
	return p.Port
}

func TestE2EInboxPreservesInterleavedFrames(t *testing.T) {
	for _, readyFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("ready-first-%t", readyFirst), func(t *testing.T) {
			source := make(chan e2eFrame, 4)
			source <- e2eFrame{ID: json.RawMessage("2"), Result: json.RawMessage(`{}`)}
			ready := e2eFrame{Method: "ready", Params: json.RawMessage(`{"port":12345}`)}
			enabled := e2eFrame{ID: json.RawMessage("100"), Result: json.RawMessage(`{"port":12345}`)}
			if readyFirst {
				source <- ready
				source <- enabled
			} else {
				source <- enabled
				source <- ready
			}
			source <- e2eFrame{ID: json.RawMessage("1"), Result: json.RawMessage(`{}`)}
			close(source)
			frames := &e2eInbox{frames: source}
			if readyFirst {
				port, retry := e2eEnabledPort(t, frames, "100", time.Second)
				if retry || port != 12345 {
					t.Fatalf("enabled port=%d retry=%v", port, retry)
				}
				if port := e2eWaitReadyPort(t, frames, time.Second); port != 12345 {
					t.Fatalf("ready port=%d", port)
				}
			} else {
				if port := e2eWaitReadyPort(t, frames, time.Second); port != 12345 {
					t.Fatalf("ready port=%d", port)
				}
				port, retry := e2eEnabledPort(t, frames, "100", time.Second)
				if retry || port != 12345 {
					t.Fatalf("enabled port=%d retry=%v", port, retry)
				}
			}
			e2eWaitResult(t, frames, "1", time.Second)
			e2eWaitResult(t, frames, "2", time.Second)
			if len(frames.pending) != 0 {
				t.Fatalf("unconsumed frames: %+v", frames.pending)
			}
		})
	}
}

// e2eFreePort returns a port that was free a moment ago. A process spawn sits
// between this probe and the child's bind, so the caller retries — see
// e2eEnableWithRetry.
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

// TestE2EFailoverOverRealBinary spawns the real binary and drives it the way
// the broker and UI do: register a busy (503) and a healthy (200) upstream as
// manual nodes over JSON-RPC stdio, then send a genuine inference POST to the
// proxy's real HTTP port. It asserts the request fails over from the busy node
// to the healthy one, the original body is replayed, and CORS headers are
// present — the whole shipped path end-to-end, no mocks.
//
// Only LM Studio had this. Running it per engine also makes it the one place
// Ollama's implied-:latest naming is proven end-to-end: the nodes advertise
// the tagged spelling while the client asks for the untagged one, so a
// regression in normalizeModel shows up as an unroutable request rather than
// a unit-test diff.
func TestE2EFailoverOverRealBinary(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		bin := proxyBinary(t)

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
		// No engine on argv: the binary starts with no facade and no listener,
		// and the broker chooses what it fronts over facade/enable. This is the
		// bring-up sequence exercised end to end.
		cmd := exec.Command(bin)
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

		source := make(chan e2eFrame, 256)
		go e2eReadFrames(stdout, source)
		frames := &e2eInbox{frames: source}

		// Retried on a bind race: e2eFreePort can only probe-then-close, and a
		// process spawn sits between the probe and the child's bind, so the
		// port can be taken in between. Treating the probe as authoritative is
		// what made this flake on "address already in use".
		port = e2eEnableWithRetry(t, stdin, frames, tc.profile.Name, port)

		// Facade-scoped methods are addressed to their engine, the same way the
		// broker addresses them: an unaddressed one resolves to no facade
		// rather than to "the only one", so it cannot be misrouted later.
		addressed := func(method string) string {
			return engines.AddressMethod(tc.profile.Name, method)
		}
		busyHost, busyPort := e2eSplitHostPort(t, busy.URL)
		goodHost, goodPort := e2eSplitHostPort(t, good.URL)
		e2eSend(t, stdin, 1, addressed("node/add-manual"), map[string]any{
			"id": "busy", "host": busyHost, "port": busyPort,
			"addresses": []string{busyHost}, "models": []string{tc.advertisedModel},
		})
		e2eWaitResult(t, frames, "1", 5*time.Second)
		e2eSend(t, stdin, 2, addressed("node/add-manual"), map[string]any{
			"id": "good", "host": goodHost, "port": goodPort,
			"addresses": []string{goodHost}, "models": []string{tc.advertisedModel},
		})
		e2eWaitResult(t, frames, "2", 5*time.Second)
		// Select the busy node so the failover path is deterministic.
		e2eSend(t, stdin, 3, addressed("node/select"), map[string]any{"id": "busy"})
		e2eWaitResult(t, frames, "3", 5*time.Second)

		body := fmt.Sprintf(`{"model":%q}`, tc.requestedModel)
		resp, err := http.Post(
			fmt.Sprintf("http://127.0.0.1:%d%s", port, tc.inferencePath),
			"application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("inference POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (should fail over from the 503 node)", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no CORS header", got)
		}
		if gotBody != body {
			t.Errorf("healthy upstream got body %q, want the original request body %q", gotBody, body)
		}

		e2eSend(t, stdin, 9, "shutdown", nil)
		e2eWaitResult(t, frames, "9", 5*time.Second)
	})
}
