// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process tests for the two identity/anti-entropy guarantees the
// workload store + workload-manager must uphold:
//
//   - Cross-engine identity: Ollama and LM Studio each begin their request
//     counter at 1, so two concurrent cross-engine jobs both mint workload id
//     "1". They must remain DISTINCT records all the way to the client stream
//     ((origin,engine,runId,id) key), not collapse into one.
//   - Rehydration on restart: when the supervised workload-manager crashes and
//     the supervisor respawns it, the broker replays its still-active local
//     workloads so the fresh manager can keep re-asserting them to peers. Kill
//     the manager mid-flight and assert a discovered peer hears the active
//     workload again from the restarted process.
//
// Both drive the real binaries TestMain builds. Test B needs mDNS (it
// advertises a stub peer over zeroconf), so it runs outside the network
// sandbox.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
)

// TestWorkloadCrossEngineIdentityDistinct fires exactly one inference through
// each supervised proxy — the first request each serves, so both mint workload
// id "1". They differ only by engine (ollama vs lmstudio) and the proxy's
// per-process runId, which must keep them as two separate records on the
// broker's workloads:* stream. Before the identity fix the (origin,id) store
// key collapsed them into one.
func TestWorkloadCrossEngineIdentityDistinct(t *testing.T) {
	if portBusy(11435) || portBusy(1234) {
		t.Skip("ollama-proxy (11435) or lmstudio-proxy (1234) default port already in use; skipping")
	}

	// Fake engines: 200 on any request so each inference completes promptly.
	fakeEngine := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(body))
		}))
	}
	ollama := fakeEngine(`{"done":true}`)
	defer ollama.Close()
	lmstudio := fakeEngine(`{"choices":[]}`)
	defer lmstudio.Close()
	ollamaPort := portOfURL(t, ollama.URL)
	lmstudioPort := portOfURL(t, lmstudio.URL)

	stdin, msgs, _, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin,
		"--lmstudio-proxy-path", lmstudioProxyBin,
		"--workload-manager-path", workloadMgrBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	t.Log("broker ready")

	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"workloads:subscribe"}`)
	ack := waitForResponse(t, msgs, 5*time.Second)
	var sr subscriptionResult
	if err := json.Unmarshal(ack.Result, &sr); err != nil || !sr.Subscribed {
		t.Fatalf("workloads:subscribe ack = %s (err %v), want subscribed:true", ack.Result, err)
	}

	ollamaProxyPort := waitProxyReady(t, stdin, msgs, 15*time.Second)
	lmstudioProxyPort := waitLMStudioProxyReady(t, stdin, msgs, 15*time.Second)
	t.Logf("proxies ready: ollama=%d lmstudio=%d", ollamaProxyPort, lmstudioProxyPort)

	// Pin each fake engine into its proxy so inference routes deterministically.
	// These are stdio control notifications, not HTTP hits on the proxy port,
	// so the first HTTP request each proxy serves is the inference below → id "1".
	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":50,"method":"proxy:node/add-manual","params":{"id":"fake-ollama","host":"127.0.0.1","port":%d,"addresses":["127.0.0.1"],"models":["crossengine-model"]}}`, ollamaPort))
	waitForResponse(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":51,"method":"proxy:node/select","params":{"id":"fake-ollama"}}`)
	waitForResponse(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":52,"method":"lmstudio-proxy:node/add-manual","params":{"id":"fake-lmstudio","host":"127.0.0.1","port":%d,"addresses":["127.0.0.1"],"models":["crossengine-model"]}}`, lmstudioPort))
	waitForResponse(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":53,"method":"lmstudio-proxy:node/select","params":{"id":"fake-lmstudio"}}`)
	waitForResponse(t, msgs, 5*time.Second)
	t.Log("both fake engines pinned as proxy targets")

	client := &http.Client{Timeout: 5 * time.Second}
	fire := func(port int, path string) {
		resp, err := client.Post(
			fmt.Sprintf("http://127.0.0.1:%d%s", port, path),
			"application/json",
			strings.NewReader(`{"model":"crossengine-model","messages":[]}`),
		)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	fire(ollamaProxyPort, "/api/chat")
	fire(lmstudioProxyPort, "/v1/chat/completions")

	// Collect the first workload id observed per engine. Both cross-engine
	// workloads must surface; the fix is proven when they share one id yet
	// arrive as two distinct engine records.
	engineIDs := map[string]string{}
	deadline := time.After(20 * time.Second)
	for len(engineIDs) < 2 {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before both engine workloads seen")
			}
			if msg.Method != "workloads:upsert" {
				continue
			}
			var p wlParams
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				continue
			}
			eng := p.WorkloadInfo.Engine
			if (eng == "ollama" || eng == "lmstudio") && p.WorkloadInfo.ID != "" {
				if _, seen := engineIDs[eng]; !seen {
					engineIDs[eng] = p.WorkloadInfo.ID
					t.Logf("tracked %s workload id=%q state=%q", eng, p.WorkloadInfo.ID, p.WorkloadInfo.State)
				}
			}
		case <-deadline:
			t.Fatalf("timed out: only saw engine workloads %v, want both ollama and lmstudio (cross-engine records collapsed?)", engineIDs)
		}
	}

	if engineIDs["ollama"] != engineIDs["lmstudio"] {
		t.Fatalf("expected the two proxies to collide on one workload id (both counters start at 1); got ollama=%q lmstudio=%q — determinism assumption broke", engineIDs["ollama"], engineIDs["lmstudio"])
	}
	t.Logf("cross-engine OK: ollama and lmstudio both tracked distinct workloads sharing id %q", engineIDs["ollama"])
}

// TestWorkloadManagerRehydratesActiveWorkloadOnRestart drives the rehydration
// path end-to-end. A proxied inference is held open by a blocking fake engine,
// so its workload stays "running" (workload:started is emitted up front). The
// supervised workload-manager is then killed; the broker's supervisor respawns
// it and replays the still-active local workload. A discovered stub peer must
// hear the running workload AGAIN from the restarted manager — which only
// happens if the broker rehydrated the fresh process's active set.
func TestWorkloadManagerRehydratesActiveWorkloadOnRestart(t *testing.T) {
	if portBusy(11435) {
		t.Skip("ollama-proxy default port 11435 already in use; skipping")
	}

	// Fake Ollama that accepts the request then blocks, keeping the proxied
	// inference in-flight so the workload never reaches a terminal state. The
	// handler also unblocks if the request is cancelled, so a client-side abort
	// can't wedge it.
	release := make(chan struct{})
	var releaseOnce sync.Once
	stopEngine := func() { releaseOnce.Do(func() { close(release) }) }
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	// Defer order matters: stopEngine() must run BEFORE ollama.Close(), because
	// Close() blocks until the deliberately-held request finishes, which only
	// happens once release is closed. Registering ollama.Close() first makes it
	// run last (LIFO).
	defer ollama.Close()
	defer stopEngine()
	ollamaPort := portOfURL(t, ollama.URL)

	// Stub peer: a cross-pinned cluster peer that records every workload frame the
	// manager fans out to it. The inter-node interface is cluster mTLS only, so the
	// stub must hold a cluster identity the node under test pins.
	fx := newInterNodeCluster(t)
	received := fx.startStubClusterPeer(t, "rehydrate-wm-peer", "rehydrate-peer-uuid")

	stdin, msgs, stderr, cleanup := startBrokerWithDirs(t, t.TempDir(), fx.nodeDir,
		"--proxy-path", proxyBin,
		"--workload-manager-path", workloadMgrBin,
	)
	t.Cleanup(cleanup)

	// Drain broker stderr continuously (avoids pipe backpressure over the long
	// waits) and surface each workload-manager start pid — the initial spawn and
	// every supervisor respawn.
	wmPids := make(chan int, 8)
	go func() {
		for line := range stderr {
			if m := wmStartedPidRe.FindStringSubmatch(line); m != nil {
				if pid, err := strconv.Atoi(m[1]); err == nil {
					select {
					case wmPids <- pid:
					default:
					}
				}
			}
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	proxyPort := waitProxyReady(t, stdin, msgs, 15*time.Second)
	t.Logf("broker ready; proxy on %d", proxyPort)

	pid1 := awaitInt(t, wmPids, 10*time.Second, "initial workload-manager start")
	t.Logf("original workload-manager pid=%d", pid1)

	// Pin the fake ollama, then fire one inference in the background — the
	// backend blocks, so the workload stays running until teardown.
	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":50,"method":"proxy:node/add-manual","params":{"id":"fake-ollama","host":"127.0.0.1","port":%d,"addresses":["127.0.0.1"],"models":["rehydrate-model"]}}`, ollamaPort))
	waitForResponse(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":51,"method":"proxy:node/select","params":{"id":"fake-ollama"}}`)
	waitForResponse(t, msgs, 5*time.Second)
	go func() {
		c := &http.Client{Timeout: 120 * time.Second}
		resp, err := c.Post(
			fmt.Sprintf("http://127.0.0.1:%d/api/chat", proxyPort),
			"application/json",
			strings.NewReader(`{"model":"rehydrate-model","messages":[]}`),
		)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// The original manager backfills its active set to the discovered peer.
	waitStubPeerWorkload(t, received, "rehydrate-model", "running", 45*time.Second)
	t.Log("original manager asserted the running workload to the peer")

	// Kill the manager; drain what the peer has already seen so the assertion
	// below can only pass on a post-restart re-assertion.
	drainWorkloadMessages(received)
	t.Logf("killing supervised workload-manager pid=%d", pid1)
	if proc, err := os.FindProcess(pid1); err == nil {
		_ = proc.Kill()
	}

	pid2 := awaitInt(t, wmPids, 30*time.Second, "workload-manager respawn")
	if pid2 == pid1 {
		t.Fatalf("respawned workload-manager reused pid %d; expected a fresh process", pid1)
	}
	t.Logf("workload-manager respawned pid=%d", pid2)

	// The rehydrated manager must re-assert the still-active workload to the
	// (re-discovered) peer. Without rehydration its activeLocal is empty after
	// the restart and the peer never hears about the job again.
	waitStubPeerWorkload(t, received, "rehydrate-model", "running", 75*time.Second)
	t.Log("rehydrate OK: restarted manager re-asserted the active workload to the peer")
}

// TestWorkloadManagerRehydratesRecentTerminalOnRestart is the restart-during-
// terminal path the running-workload test doesn't cover. A proxied inference
// COMPLETES (terminal), which the manager keeps in its re-sync set for a couple
// of heartbeats so a peer that missed the terminal still converges. When the
// manager is killed mid-window, the broker must replay the recent terminal (not
// just active workloads) so the restarted manager re-asserts the completed
// state — otherwise a peer's wrongly-inferred "failed" (or a missed terminal)
// can never be repaired after a restart.
func TestWorkloadManagerRehydratesRecentTerminalOnRestart(t *testing.T) {
	if portBusy(11435) {
		t.Skip("ollama-proxy default port 11435 already in use; skipping")
	}

	// Fake Ollama that completes immediately, so the workload reaches a terminal
	// (completed) state right away.
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"done":true}`))
	}))
	defer ollama.Close()
	ollamaPort := portOfURL(t, ollama.URL)

	fx := newInterNodeCluster(t)
	received := fx.startStubClusterPeer(t, "rehydrate-term-peer", "rehydrate-term-peer-uuid")

	stdin, msgs, stderr, cleanup := startBrokerWithDirs(t, t.TempDir(), fx.nodeDir,
		"--proxy-path", proxyBin,
		"--workload-manager-path", workloadMgrBin,
	)
	t.Cleanup(cleanup)

	wmPids := make(chan int, 8)
	go func() {
		for line := range stderr {
			if m := wmStartedPidRe.FindStringSubmatch(line); m != nil {
				if pid, err := strconv.Atoi(m[1]); err == nil {
					select {
					case wmPids <- pid:
					default:
					}
				}
			}
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	proxyPort := waitProxyReady(t, stdin, msgs, 15*time.Second)
	pid1 := awaitInt(t, wmPids, 10*time.Second, "initial workload-manager start")

	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":50,"method":"proxy:node/add-manual","params":{"id":"fake-ollama","host":"127.0.0.1","port":%d,"addresses":["127.0.0.1"],"models":["rehydrate-term-model"]}}`, ollamaPort))
	waitForResponse(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":51,"method":"proxy:node/select","params":{"id":"fake-ollama"}}`)
	waitForResponse(t, msgs, 5*time.Second)

	// Fire one inference; it completes promptly → terminal workload.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/chat", proxyPort),
		"application/json",
		strings.NewReader(`{"model":"rehydrate-term-model","messages":[]}`),
	)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// The original manager backfills the recent terminal to the discovered peer.
	waitStubPeerWorkload(t, received, "rehydrate-term-model", "completed", 45*time.Second)
	t.Log("original manager asserted the completed workload to the peer")

	drainWorkloadMessages(received)
	t.Logf("killing supervised workload-manager pid=%d", pid1)
	if proc, err := os.FindProcess(pid1); err == nil {
		_ = proc.Kill()
	}

	pid2 := awaitInt(t, wmPids, 30*time.Second, "workload-manager respawn")
	if pid2 == pid1 {
		t.Fatalf("respawned workload-manager reused pid %d; expected a fresh process", pid1)
	}
	t.Logf("workload-manager respawned pid=%d", pid2)

	// The rehydrated manager must re-assert the recent terminal. If the broker
	// only replayed active workloads, the completed state is gone after the
	// restart and the peer never hears it again.
	waitStubPeerWorkload(t, received, "rehydrate-term-model", "completed", 75*time.Second)
	t.Log("rehydrate OK: restarted manager re-asserted the recent terminal to the peer")
}

// --- helpers ---

var wmStartedPidRe = regexp.MustCompile(`workload-manager started.*\bpid=(\d+)`)

// waitLMStudioProxyReady polls lmstudio-proxy:get-status until the LM Studio
// proxy reports ready and returns its bound port (mirrors waitProxyReady).
func waitLMStudioProxyReady(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, timeout time.Duration) int {
	t.Helper()
	id := 9500
	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"lmstudio-proxy:get-status"}`, id))
	deadline := time.After(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed waiting for lmstudio-proxy:get-status")
			}
			if msg.ID != nil && msg.Method == "" {
				var st struct {
					Ready bool `json:"ready"`
					Port  int  `json:"port"`
				}
				if json.Unmarshal(msg.Result, &st) == nil && st.Ready {
					return st.Port
				}
			}
		case <-tick.C:
			id++
			writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"lmstudio-proxy:get-status"}`, id))
		case <-deadline:
			t.Fatalf("timed out (%s) waiting for lmstudio-proxy to become ready", timeout)
		}
	}
}

// waitStubPeerWorkload blocks until the stub peer records a workload:* frame
// for the given model in the given state.
func waitStubPeerWorkload(t *testing.T, received <-chan jsonrpc.Message, model, state string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-received:
			if !ok {
				t.Fatalf("stub peer channel closed before %q workload %s", model, state)
			}
			if !strings.HasPrefix(msg.Method, "workload:") {
				continue
			}
			var p wlParams
			if json.Unmarshal(msg.Params, &p) != nil {
				continue
			}
			if p.WorkloadInfo.Model == model && p.WorkloadInfo.State == state {
				return
			}
		case <-deadline:
			t.Fatalf("timed out (%s) waiting for stub peer to receive %q workload in state %q", timeout, model, state)
		}
	}
}

// drainWorkloadMessages empties the channel of anything buffered so far.
func drainWorkloadMessages(ch <-chan jsonrpc.Message) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// awaitInt receives one int from ch or fails after timeout.
func awaitInt(t *testing.T, ch <-chan int, timeout time.Duration, what string) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatalf("timed out (%s) waiting for %s", timeout, what)
		return 0
	}
}

// portOfURL extracts the TCP port from an httptest server URL.
func portOfURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host:port %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi port %q: %v", portStr, err)
	}
	return port
}
