// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process test for the mDNS-independent model-inventory refresh: a node
// advertises an engine-manager (em) service whose /v1/models is initially empty,
// the broker discovers it with no models, and then the endpoint starts serving
// models and later reports an explicit empty inventory WITHOUT any change to the
// mDNS record. The promoted discovery daemon's periodic refresh loop must
// converge the broker's directory in both directions — including a last-model
// deletion that used to resurrect stale state.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"

	"nvpair-shared/jsonrpc"
)

// swapModels is a /v1/models stub whose body can be swapped at runtime, standing
// in for an engine-manager whose model list changes (a pull, or a late engine
// start) while its mDNS record stays fixed.
type swapModels struct {
	mu   sync.Mutex
	body map[string]any
}

func (s *swapModels) set(body map[string]any) {
	s.mu.Lock()
	s.body = body
	s.mu.Unlock()
}

func (s *swapModels) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		body := s.body
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	}
}

func TestModelsPeriodicRefreshConvergesWithoutMDNSChange(t *testing.T) {
	// Start with an empty inventory (engine present but serving no models yet).
	stub := &swapModels{}
	stub.set(map[string]any{"models": []string{}})
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	emPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}

	const instance = "xproc-models-refresh-1"
	// Advertise em=<port> + ip=127.0.0.1 once and never touch the record again,
	// so the only thing that changes during the test is the /v1/models body.
	txt := []string{"v=1", "uuid=" + instance + "-uuid", "ip=127.0.0.1", fmt.Sprintf("em=%d", emPort)}
	zsrv, err := zeroconf.Register(instance, nodeRecordService, testDomain, emPort, txt, nil)
	if err != nil {
		t.Fatalf("register %s: %v", instance, err)
	}
	t.Cleanup(zsrv.Shutdown)
	t.Logf("advertising %s @ %s (em=%d), models initially empty", instance, nodeRecordService, emPort)

	stdin, msgs, cleanup := startBroker(t)
	t.Cleanup(cleanup)

	// Phase 1: the node is discovered and enriched, but with no models.
	pollForNode(t, stdin, msgs, instance, 30*time.Second, func(n availableNode) bool {
		return len(n.Models) == 0
	})
	t.Log("node discovered with an empty inventory; now serving models without an mDNS change")

	// Phase 2: the endpoint starts serving models. No mDNS record changes, so
	// only the periodic refresh loop can converge the directory. The deadline
	// comfortably exceeds the refresh interval so at least one sweep runs.
	stub.set(map[string]any{
		"models": []string{"llama3:8b", "qwen:0.5b"},
		"modelsByEngine": map[string][]string{
			"ollama":   {"llama3:8b"},
			"lmstudio": {"qwen:0.5b"},
		},
		"loadedByEngine": map[string][]string{
			"ollama":   {"llama3:8b"},
			"lmstudio": {},
		},
	})
	pollForNode(t, stdin, msgs, instance, 45*time.Second, func(n availableNode) bool {
		return modelsMatch(n.Models) && modelsByEngineMatch(n.ModelsByEngine) &&
			loadedByEngineMatch(n.LoadedByEngine)
	})

	// Phase 3: delete the final models without touching mDNS. Preserve the
	// successful empty engine keys all the way through daemon -> broker so
	// consumers can distinguish "known empty" from "inventory unavailable".
	stub.set(map[string]any{
		"models": []string{},
		"modelsByEngine": map[string][]string{
			"ollama":   {},
			"lmstudio": {},
		},
		"loadedByEngine": map[string][]string{
			"ollama":   {},
			"lmstudio": {},
		},
	})
	emptyByEngine := map[string][]string{"ollama": {}, "lmstudio": {}}
	pollForNode(t, stdin, msgs, instance, 45*time.Second, func(n availableNode) bool {
		return len(n.Models) == 0 && byEngineEqual(n.ModelsByEngine, emptyByEngine) &&
			byEngineEqual(n.LoadedByEngine, emptyByEngine)
	})
}

// pollForNode drives discovery:get-nodes on a cadence until the node named
// instance appears and satisfies pred, or the timeout elapses.
func pollForNode(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, instance string, timeout time.Duration, pred func(availableNode) bool) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	id := 200
	sendReq(t, stdin, id, "discovery:get-nodes")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed unexpectedly")
			}
			if msg.Method == "" && msg.ID != nil {
				var res availableNodesResult
				if json.Unmarshal(msg.Result, &res) == nil {
					if n, found := findNode(res.Nodes, instance); found && pred(n) {
						return
					}
				}
			}
		case <-ticker.C:
			id++
			sendReq(t, stdin, id, "discovery:get-nodes")
		case <-deadline:
			t.Fatalf("timed out waiting for node %q to match predicate", instance)
		}
	}
}
