// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process tests for nvpair-job-scheduler and the proxy node/set-priority
// method the broker fans schedule:priority out to.
//
//   - Scheduler process tests drive workload, telemetry, staleness, and restart
//     baselines over stdin and inspect complete schedule:priority snapshots.
//   - TestProxySetPriorityViaBroker exercises the proxy's node/set-priority
//     through the broker's proxy:<method> relay end-to-end.
//   - TestProxyConcurrentBurstDistribution holds real upstream requests open and
//     verifies the proxy balances before scheduler feedback can arrive.
//   - TestBrokerSpawnsScheduler confirms the broker comes up healthy with the
//     scheduler adopted as a supervised worker.
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sort"
	"sync"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/schedulerwire"
)

// startSchedulerProc starts the scheduler binary and returns its stdin, a
// reader over its stdout JSON-RPC frames, and a cleanup.
func startSchedulerProc(t *testing.T, args ...string) (io.WriteCloser, <-chan jsonrpc.Message, func()) {
	t.Helper()
	cmd := exec.Command(schedulerBin, args...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("scheduler stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("scheduler stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start scheduler: %v", err)
	}
	ch := startMsgReader(stdout)
	var cleanupOnce sync.Once
	return stdin, ch, func() {
		cleanupOnce.Do(func() {
			stdin.Close()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		})
	}
}

// waitForPriorityPair returns the next complete priority emission for both
// engine outputs. A shared node-wide ranking changes both outputs together.
func waitForPriorityPair(t *testing.T, ch <-chan jsonrpc.Message, timeout time.Duration) map[string]schedulerwire.EnginePriority {
	t.Helper()
	to := time.After(timeout)
	got := make(map[string]schedulerwire.EnginePriority, 2)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatal("stream closed before both schedule:priority outputs")
			}
			if msg.Method != "schedule:priority" {
				continue
			}
			var p schedulerwire.EnginePriority
			if json.Unmarshal(msg.Params, &p) != nil {
				continue
			}
			if p.Engine == "ollama" || p.Engine == "lmstudio" {
				got[p.Engine] = p
			}
			if len(got) == 2 {
				return got
			}
		case <-to:
			t.Fatalf("timed out (%s) waiting for both schedule:priority outputs; got %v", timeout, got)
		}
	}
}

func waitForSchedulePair(t *testing.T, ch <-chan jsonrpc.Message, timeout time.Duration) map[string][]string {
	t.Helper()
	priorities := waitForPriorityPair(t, ch, timeout)
	return map[string][]string{
		"ollama":   priorities["ollama"].Nodes,
		"lmstudio": priorities["lmstudio"].Nodes,
	}
}

func assertSchedulePair(t *testing.T, got map[string][]string, want []string) {
	t.Helper()
	for _, engine := range []string{"ollama", "lmstudio"} {
		assertScheduleOrder(t, engine, got[engine], want)
	}
}

func assertScheduleOrder(t *testing.T, engine string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s order = %v, want %v", engine, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s order = %v, want %v", engine, got, want)
		}
	}
}

// TestSchedulerRanksNodeWideWithoutWaitingForTimer uses a one-hour periodic
// interval: the mixed-engine C,B,A result must therefore come from immediate
// workload-event recomputation, not the timer.
func TestSchedulerRanksNodeWideWithoutWaitingForTimer(t *testing.T) {
	stdin, msgs, cleanup := startSchedulerProc(t, "--interval", "1h")
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "ready", 10*time.Second)

	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"discovery:nodes-changed","params":[{"id":"a","hostUuid":"a"},{"id":"b","hostUuid":"b"},{"id":"c","hostUuid":"c"}]}`)
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), []string{"a", "b", "c"})

	// A=3 (two Ollama, one LM Studio), B=1 (LM Studio), C=0.
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"workloads:upsert","params":{"workloadInfo":{"id":"wl1","engine":"ollama","runId":"o","state":"running","originatedFrom":"x","scheduledOn":"a"}}}`)
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), []string{"b", "c", "a"})
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"workloads:upsert","params":{"workloadInfo":{"id":"wl2","engine":"lmstudio","runId":"l","state":"queued","originatedFrom":"x","scheduledOn":"a"}}}`)
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), []string{"b", "c", "a"})
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"workloads:upsert","params":{"workloadInfo":{"id":"wl3","engine":"ollama","runId":"o","state":"queued","originatedFrom":"x","scheduledOn":"a"}}}`)
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), []string{"b", "c", "a"})
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"workloads:upsert","params":{"workloadInfo":{"id":"wl4","engine":"lmstudio","runId":"l","state":"running","originatedFrom":"x","scheduledOn":"b"}}}`)
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), []string{"c", "b", "a"})
}

func TestSchedulerRanksFreshUnknownAndStaleTelemetry(t *testing.T) {
	stdin, msgs, cleanup := startSchedulerProc(t, "--interval", "1h")
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "ready", 10*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"scheduler:telemetry","params":{"hostUuid":"a","gpuUtilizationPercent":0,"telemetryValid":true,"msSince":0}}`)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"scheduler:telemetry","params":{"hostUuid":"c","gpuUtilizationPercent":100,"telemetryValid":true,"msSince":10001}}`)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"scheduler:telemetry","params":{"hostUuid":"d","gpuUtilizationPercent":90,"telemetryValid":true,"msSince":0}}`)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"discovery:nodes-changed","params":[{"hostUuid":"a"},{"hostUuid":"b"},{"hostUuid":"c"},{"hostUuid":"d"}]}`)

	initial := waitForPriorityPair(t, msgs, 5*time.Second)
	assertPriorityPair(t, initial, []string{"a", "b", "c", "d"}, map[string]int{
		"a": 0,
		"b": 1,
		"c": 1,
		"d": 3,
	})

	// A fresh sample promotes c from stale/unknown pressure 1 to pressure 2.
	// Its order is unchanged, but the complete rank snapshot must still emit.
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"scheduler:telemetry","params":{"hostUuid":"c","gpuUtilizationPercent":75,"telemetryValid":true,"msSince":0}}`)
	fresh := waitForPriorityPair(t, msgs, 5*time.Second)
	assertPriorityPair(t, fresh, []string{"a", "b", "c", "d"}, map[string]int{
		"a": 0,
		"b": 1,
		"c": 2,
		"d": 3,
	})

	// Invalidating d immediately returns it to neutral pressure and moves it
	// ahead of c without waiting for the one-hour reconciliation timer.
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"scheduler:telemetry","params":{"hostUuid":"d","gpuUtilizationPercent":90,"telemetryValid":false,"msSince":0}}`)
	invalid := waitForPriorityPair(t, msgs, 5*time.Second)
	assertPriorityPair(t, invalid, []string{"a", "b", "d", "c"}, map[string]int{
		"a": 0,
		"b": 1,
		"c": 2,
		"d": 1,
	})
}

func TestSchedulerRestartBaselineUsesTelemetryReplay(t *testing.T) {
	stdin, msgs, stopFirst := startSchedulerProc(t, "--interval", "1h")
	t.Cleanup(stopFirst)
	waitForMethod(t, msgs, "ready", 10*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"discovery:nodes-changed","params":[{"hostUuid":"a"},{"hostUuid":"b"}]}`)
	waitForSchedulePair(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"scheduler:telemetry","params":{"hostUuid":"a","gpuUtilizationPercent":90,"telemetryValid":true,"msSince":0}}`)
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), []string{"b", "a"})
	stopFirst()

	// The broker replays telemetry before discovery to a replacement scheduler.
	// Its first non-empty snapshot must therefore already be GPU-aware.
	stdin, msgs, stopSecond := startSchedulerProc(t, "--interval", "1h")
	t.Cleanup(stopSecond)
	waitForMethod(t, msgs, "ready", 10*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"scheduler:telemetry","params":{"hostUuid":"a","gpuUtilizationPercent":90,"telemetryValid":true,"msSince":250}}`)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"discovery:nodes-changed","params":[{"hostUuid":"a"},{"hostUuid":"b"}]}`)
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), []string{"b", "a"})
}

func assertPriorityPair(
	t *testing.T,
	got map[string]schedulerwire.EnginePriority,
	wantOrder []string,
	wantPressure map[string]int,
) {
	t.Helper()
	for _, engine := range []string{"ollama", "lmstudio"} {
		priority := got[engine]
		assertScheduleOrder(t, engine, priority.Nodes, wantOrder)
		if len(priority.Ranks) != len(wantPressure) {
			t.Fatalf("%s ranks = %+v, want pressure for %v", engine, priority.Ranks, wantPressure)
		}
		for _, rank := range priority.Ranks {
			want, ok := wantPressure[rank.ID]
			if !ok {
				t.Fatalf("%s emitted unexpected rank %+v", engine, rank)
			}
			if rank.GPUPressure != want {
				t.Fatalf("%s gpuPressure[%s] = %d, want %d; ranks=%+v",
					engine, rank.ID, rank.GPUPressure, want, priority.Ranks)
			}
		}
	}
}

// TestSchedulerSyntheticMixedEngineBurst feeds each emitted first choice back
// as the next assignment. Across 50 alternating-engine jobs, node depths must
// remain within one even after every node is well above depth three.
func TestSchedulerSyntheticMixedEngineBurst(t *testing.T) {
	stdin, msgs, cleanup := startSchedulerProc(t, "--interval", "1h")
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "ready", 10*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","method":"discovery:nodes-changed","params":[{"hostUuid":"a"},{"hostUuid":"b"},{"hostUuid":"c"}]}`)
	initial := []string{"a", "b", "c"}
	assertSchedulePair(t, waitForSchedulePair(t, msgs, 5*time.Second), initial)

	depths := map[string]int{"a": 0, "b": 0, "c": 0}
	current := initial
	for i := 0; i < 50; i++ {
		target := current[0]
		engine := "ollama"
		if i%2 == 1 {
			engine = "lmstudio"
		}
		depths[target]++
		want := rankSyntheticDepths(depths)
		writeRawFrame(t, stdin, fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"workloads:upsert","params":{"workloadInfo":{"id":"burst-%d","engine":"%s","runId":"burst","state":"running","originatedFrom":"local","scheduledOn":"%s"}}}`,
			i, engine, target,
		))
		got := waitForSchedulePair(t, msgs, 5*time.Second)
		assertSchedulePair(t, got, want)
		current = want
	}

	minDepth, maxDepth := 50, 0
	for _, depth := range depths {
		if depth < minDepth {
			minDepth = depth
		}
		if depth > maxDepth {
			maxDepth = depth
		}
	}
	if minDepth <= 3 || maxDepth-minDepth > 1 {
		t.Fatalf("50-job mixed-engine depths = %v, want all >3 with skew <=1", depths)
	}
}

func rankSyntheticDepths(depths map[string]int) []string {
	nodes := make([]string, 0, len(depths))
	for node := range depths {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		if depths[nodes[i]] != depths[nodes[j]] {
			return depths[nodes[i]] < depths[nodes[j]]
		}
		return nodes[i] < nodes[j]
	})
	return nodes
}

// TestProxyConcurrentBurstDistribution drives the real broker and proxy
// binaries against blocked upstream HTTP servers. Because every request stays
// in flight until all destinations have been observed, scheduler feedback
// cannot explain the distribution: the proxy's atomic reservations must do it.
func TestProxyConcurrentBurstDistribution(t *testing.T) {
	t.Run("equal load", func(t *testing.T) {
		pending := []int{0, 0, 0, 0}
		pressure := []int{0, 0, 0, 0}
		counts, ids := runBlockedProxyBurst(t, pending, pressure, 100)
		assertBurstTotalsSkew(t, counts, ids, pending, pressure, 1)
	})

	t.Run("unequal load converges", func(t *testing.T) {
		pending := []int{0, 2, 4}
		pressure := []int{0, 0, 0}
		counts, ids := runBlockedProxyBurst(t, pending, pressure, 6)
		assertBurstTotalsSkew(t, counts, ids, pending, pressure, 0)
	})

	t.Run("GPU pressure converges", func(t *testing.T) {
		pending := []int{0, 1, 0}
		pressure := []int{3, 0, 2}
		counts, ids := runBlockedProxyBurst(t, pending, pressure, 3)
		assertBurstTotalsSkew(t, counts, ids, pending, pressure, 0)
	})
}

func runBlockedProxyBurst(t *testing.T, pending, pressure []int, requests int) (map[string]int, []string) {
	t.Helper()
	if len(pending) != len(pressure) {
		t.Fatalf("pending/pressure length mismatch: %d != %d", len(pending), len(pressure))
	}

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	hits := make(chan string, requests*len(pending))
	ids := make([]string, len(pending))
	servers := make([]*httptest.Server, len(pending))
	for i := range pending {
		id := fmt.Sprintf("burst-%c", 'a'+i)
		ids[i] = id
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case hits <- id:
			case <-r.Context().Done():
				return
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"done":true}`)
		}))
	}

	stdin, msgs, stderr, cleanup := startBrokerWith(t, "--proxy-path", proxyBin)
	t.Cleanup(cleanup)
	t.Cleanup(func() {
		unblock()
		for _, server := range servers {
			server.Close()
		}
	})
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	proxyPort := waitProxyReady(t, stdin, msgs, 15*time.Second)

	requestID := 100
	for i, server := range servers {
		callBrokerRPC(t, stdin, msgs, requestID, "proxy:node/add-manual", map[string]any{
			"id":        ids[i],
			"host":      "127.0.0.1",
			"port":      portOfURL(t, server.URL),
			"addresses": []string{"127.0.0.1"},
			"models":    []string{"burst-model"},
		})
		requestID++
	}

	order := append([]string(nil), ids...)
	pendingByID := make(map[string]int, len(ids))
	pressureByID := make(map[string]int, len(ids))
	for i, id := range ids {
		pendingByID[id] = pending[i]
		pressureByID[id] = pressure[i]
	}
	sort.Slice(order, func(i, j int) bool {
		left := pendingByID[order[i]] + pressureByID[order[i]]
		right := pendingByID[order[j]] + pressureByID[order[j]]
		if left != right {
			return left < right
		}
		if pressureByID[order[i]] != pressureByID[order[j]] {
			return pressureByID[order[i]] < pressureByID[order[j]]
		}
		return order[i] < order[j]
	})
	ranks := make([]schedulerwire.NodeRank, 0, len(order))
	for rank, id := range order {
		ranks = append(ranks, schedulerwire.NodeRank{
			ID:          id,
			Pending:     pendingByID[id],
			GPUPressure: pressureByID[id],
			Rank:        rank,
		})
	}
	callBrokerRPC(t, stdin, msgs, requestID, "proxy:node/set-priority",
		schedulerwire.Priority{Nodes: order, Ranks: ranks})

	// Workload and request notifications can exceed the reader's buffer while
	// the upstreams are blocked. Drain them after setup so proxy writes never
	// become an accidental serialization point.
	go func() {
		for range msgs {
		}
	}()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        requests,
			MaxIdleConnsPerHost: requests,
			MaxConnsPerHost:     requests,
		},
	}
	t.Cleanup(func() { client.CloseIdleConnections() })
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/api/chat", proxyPort)
	start := make(chan struct{})
	results := make(chan error, requests)
	for range requests {
		go func() {
			<-start
			resp, err := client.Post(endpoint, "application/json",
				bytes.NewReader([]byte(`{"model":"burst-model","messages":[]}`)))
			if err != nil {
				results <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				results <- fmt.Errorf("proxy status %d", resp.StatusCode)
				return
			}
			results <- nil
		}()
	}
	close(start)

	counts := make(map[string]int, len(ids))
	hitTimer := time.NewTimer(25 * time.Second)
	defer hitTimer.Stop()
	for range requests {
		select {
		case id := <-hits:
			counts[id]++
		case <-hitTimer.C:
			t.Fatalf("timed out waiting for %d blocked upstream hits; got %v", requests, counts)
		}
	}

	unblock()
	resultTimer := time.NewTimer(25 * time.Second)
	defer resultTimer.Stop()
	for range requests {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("burst request failed: %v", err)
			}
		case <-resultTimer.C:
			t.Fatalf("timed out waiting for %d burst responses", requests)
		}
	}
	return counts, ids
}

func callBrokerRPC(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, id int, method string, params any) {
	t.Helper()
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal %s request: %v", method, err)
	}
	writeRawFrame(t, stdin, string(frame))
	response := waitForResponseID(t, msgs, id, 5*time.Second)
	if response.Error != nil {
		t.Fatalf("%s rejected: %d %s", method, response.Error.Code, response.Error.Message)
	}
}

func assertBurstTotalsSkew(
	t *testing.T,
	counts map[string]int,
	ids []string,
	pending, pressure []int,
	wantMaxSkew int,
) {
	t.Helper()
	minTotal, maxTotal := int(^uint(0)>>1), 0
	totals := make(map[string]int, len(ids))
	for i, id := range ids {
		total := pending[i] + pressure[i] + counts[id]
		totals[id] = total
		if total < minTotal {
			minTotal = total
		}
		if total > maxTotal {
			maxTotal = total
		}
	}
	if maxTotal-minTotal > wantMaxSkew {
		t.Fatalf("burst assignments did not balance: assigned=%v totals=%v, max skew %d",
			counts, totals, wantMaxSkew)
	}
}

// TestProxySetPriorityViaBroker: the proxy's node/set-priority is reachable
// through the broker's proxy:<method> relay and returns {count}.
func TestProxySetPriorityViaBroker(t *testing.T) {
	stdin, msgs, cleanup := startBrokerProc(t,
		"--scanner-path", scannerBin,
		"--proxy-path", proxyBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	waitProxyReady(t, stdin, msgs, 15*time.Second)

	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":70,"method":"proxy:node/set-priority","params":{"nodes":["alpha","beta","gamma"]}}`)
	resp := waitForResponse(t, msgs, 5*time.Second)
	var r struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("node/set-priority result = %s (err %v)", resp.Result, err)
	}
	if r.Count != 3 {
		t.Fatalf("node/set-priority count = %d, want 3", r.Count)
	}
	t.Logf("proxy accepted priority list of %d nodes via broker relay", r.Count)
}

// TestLMStudioProxyIgnoresPriorityNodesAbsentFromDiscovery: the scheduler
// emits a node-wide ranking that may include peers without LM Studio. The
// lmstudio-proxy must intersect that list with its own discovery set and never
// dial a priority id that never advertised the lm service.
func TestLMStudioProxyIgnoresPriorityNodesAbsentFromDiscovery(t *testing.T) {
	if portBusy(1234) {
		t.Skip("lmstudio-proxy default port 1234 already in use; skipping")
	}

	var hitsMu sync.Mutex
	hits := map[string]int{}
	realEngine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsMu.Lock()
		hits["real-lm"]++
		hitsMu.Unlock()
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	t.Cleanup(realEngine.Close)
	realPort := portOfURL(t, realEngine.URL)

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--lmstudio-proxy-path", lmstudioProxyBin,
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	proxyPort := waitLMStudioProxyReady(t, stdin, msgs, 15*time.Second)

	writeRawFrame(t, stdin, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":90,"method":"lmstudio-proxy:node/add-manual","params":{"id":"real-lm","engine":"lmstudio","host":"127.0.0.1","port":%d,"addresses":["127.0.0.1"],"models":["chat-model"]}}`,
		realPort,
	))
	if resp := waitForResponse(t, msgs, 5*time.Second); resp.Error != nil {
		t.Fatalf("lmstudio-proxy:node/add-manual rejected: %d %s", resp.Error.Code, resp.Error.Message)
	}

	// Priority puts a peer that never advertised LM Studio first. The proxy must
	// skip it and route to the discovered real-lm node.
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":91,"method":"lmstudio-proxy:node/set-priority","params":{"nodes":["no-lm-peer","real-lm"]}}`)
	if resp := waitForResponse(t, msgs, 5*time.Second); resp.Error != nil {
		t.Fatalf("lmstudio-proxy:node/set-priority rejected: %d %s", resp.Error.Code, resp.Error.Message)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxyPort),
		"application/json",
		bytes.NewReader([]byte(`{"model":"chat-model","messages":[]}`)),
	)
	if err != nil {
		t.Fatalf("chat request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status %d, want 200", resp.StatusCode)
	}

	hitsMu.Lock()
	defer hitsMu.Unlock()
	if hits["real-lm"] != 1 {
		t.Fatalf("real-lm hits = %d, want 1 (priority phantom should be ignored)", hits["real-lm"])
	}
}

// TestBrokerSpawnsScheduler: with the scheduler adopted, the broker still
// reaches app:ready (a fatal spawn would abort startup) and a log/set-level
// round-trips (proving the read loop is alive with the scheduler in the tree).
func TestBrokerSpawnsScheduler(t *testing.T) {
	stdin, msgs, cleanup := startBrokerProc(t,
		"--scanner-path", scannerBin,
		"--scheduler-path", schedulerBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 10*time.Second)

	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":80,"method":"log/set-level","params":{"level":"debug"}}`))
	resp := waitForResponse(t, msgs, 5*time.Second)
	var r struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil || r.Level != "debug" {
		t.Fatalf("log/set-level result = %s (err %v), want level=debug", resp.Result, err)
	}
}
