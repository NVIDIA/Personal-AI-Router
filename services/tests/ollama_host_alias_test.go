// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestInheritedOllamaHostAliasEndToEnd proves the environment-to-listener
// path with compiled processes: the broker reads OLLAMA_HOST, passes a safe
// loopback alias to ollama-proxy, and the alias uses the same routing/workload
// handler as the primary compatibility facade.
func TestInheritedOllamaHostAliasEndToEnd(t *testing.T) {
	if portBusy(11434) {
		t.Skip("managed Ollama facade port 11434 already in use; skipping")
	}

	aliasPort := freeDualLoopbackPort(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"routed-through-alias"},"done":true}`)
	}))
	defer upstream.Close()
	upstreamPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	stdin, msgs, _, cleanup := startBrokerWithEnv(t,
		[]string{fmt.Sprintf("OLLAMA_HOST=localhost:%d", aliasPort)},
		"--settings-path", nodeSettingsBin,
		"--engine-manager-path", engineMgrBin,
		"--proxy-path", proxyBin,
	)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	if got := waitProxyReady(t, stdin, msgs, 15*time.Second); got != 11434 {
		t.Fatalf("primary proxy port = %d, want managed facade 11434", got)
	}

	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"workloads:subscribe"}`)
	waitForResponse(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"proxy:node/add-manual","params":{"id":"alias-upstream","host":"127.0.0.1","port":%d,"addresses":["127.0.0.1"],"models":["alias-e2e-model"]}}`, upstreamPort))
	waitForResponse(t, msgs, 5*time.Second)
	writeRawFrame(t, stdin, `{"jsonrpc":"2.0","id":3,"method":"proxy:node/select","params":{"id":"alias-upstream"}}`)
	waitForResponse(t, msgs, 5*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		resp, err := client.Post(
			fmt.Sprintf("http://%s:%d/api/chat", host, aliasPort),
			"application/json",
			strings.NewReader(`{"model":"alias-e2e-model","messages":[]}`),
		)
		if err != nil {
			t.Fatalf("POST through inherited OLLAMA_HOST alias %s: %v", host, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "routed-through-alias") {
			t.Fatalf("alias %s response = %d %s", host, resp.StatusCode, body)
		}
	}

	wantStates := map[string]bool{"running": false, "completed": false}
	deadline := time.After(10 * time.Second)
	for !wantStates["running"] || !wantStates["completed"] {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed before alias workload lifecycle arrived")
			}
			if msg.Method != "workloads:upsert" {
				continue
			}
			var p struct {
				WorkloadInfo struct {
					Model string `json:"model"`
					State string `json:"state"`
				} `json:"workloadInfo"`
			}
			if json.Unmarshal(msg.Params, &p) == nil && p.WorkloadInfo.Model == "alias-e2e-model" {
				wantStates[p.WorkloadInfo.State] = true
			}
		case <-deadline:
			t.Fatalf("alias request workload states = %+v, want running and completed", wantStates)
		}
	}

}

func freeDualLoopbackPort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 10; attempt++ {
		ipv6, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		port := ipv6.Addr().(*net.TCPAddr).Port
		_ = ipv6.Close()
		ipv4, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = ipv4.Close()
			return port
		}
	}
	t.Fatal("could not find a free port on both loopback families")
	return 0
}
