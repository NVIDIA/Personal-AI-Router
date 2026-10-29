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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
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

func (s *swapModels) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		body := s.body
		s.mu.Unlock()
		assert.NoError(t, json.NewEncoder(w).Encode(body))
	}
}

func TestModelsPeriodicRefresh_ConvergesWithoutMDNSChange(t *testing.T) {
	// Start with an empty inventory (engine present but serving no models yet).
	stub := &swapModels{}
	emptyByEngine := map[string][]string{"ollama": {}, "lmstudio": {}, "llamacpp": {}}
	stub.set(map[string]any{
		"models":         []string{},
		"modelsByEngine": emptyByEngine,
		"loadedByEngine": emptyByEngine,
	})

	const instance = "xproc-models-refresh-1"
	cluster := newInterNodeCluster(t)
	cluster.startModelsPeer(t, instance, stub.handler(t))

	stdin, msgs, _, cleanup := startBrokerWithDirs(t, t.TempDir(), cluster.nodeDir)
	t.Cleanup(cleanup)

	// Require a successful empty inventory read, not just an unverified row
	// whose model fields have never been populated.
	pollForNode(t, stdin, msgs, cluster.peerUUID, 30*time.Second, func(n availableNode) bool {
		return n.ID == instance && len(n.Models) == 0 && byEngineEqual(n.ModelsByEngine, emptyByEngine) &&
			byEngineEqual(n.LoadedByEngine, emptyByEngine)
	})
	t.Log("node discovered with an empty inventory; now serving models without an mDNS change")

	// Phase 2: the endpoint starts serving models. No mDNS record changes, so
	// only the periodic refresh loop can converge the directory. The deadline
	// comfortably exceeds the refresh interval so at least one sweep runs.
	stub.set(map[string]any{
		"models": []string{"llama3:8b", "qwen:0.5b", "org/router-model-GGUF:Q4_K_M"},
		"modelsByEngine": map[string][]string{
			"ollama":   {"llama3:8b"},
			"lmstudio": {"qwen:0.5b"},
			"llamacpp": {"org/router-model-GGUF:Q4_K_M"},
		},
		"loadedByEngine": map[string][]string{
			"ollama":   {"llama3:8b"},
			"lmstudio": {},
			"llamacpp": {"org/router-model-GGUF:Q4_K_M"},
		},
	})
	pollForNode(t, stdin, msgs, cluster.peerUUID, 45*time.Second, func(n availableNode) bool {
		return n.ID == instance && modelsMatch(n.Models) && modelsByEngineMatch(n.ModelsByEngine) &&
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
			"llamacpp": {},
		},
		"loadedByEngine": map[string][]string{
			"ollama":   {},
			"lmstudio": {},
			"llamacpp": {},
		},
	})
	pollForNode(t, stdin, msgs, cluster.peerUUID, 45*time.Second, func(n availableNode) bool {
		return n.ID == instance && len(n.Models) == 0 && byEngineEqual(n.ModelsByEngine, emptyByEngine) &&
			byEngineEqual(n.LoadedByEngine, emptyByEngine)
	})
}

// pollForNode waits for an authenticated peer and, when supplied, an additional
// case-specific condition. Unverified discoveries cannot satisfy readiness.
func pollForNode(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, hostUUID string, timeout time.Duration, pred func(availableNode) bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	id := 200
	sendReq(t, stdin, id, "discovery:get-nodes")
	for {
		select {
		case msg, ok := <-msgs:
			require.True(t, ok, "broker stream closed unexpectedly")
			if msg.Method == "" && msg.ID != nil {
				require.Nil(t, msg.Error, "discovery:get-nodes")
				var res availableNodesResult
				require.NoError(t, json.Unmarshal(msg.Result, &res), "discovery:get-nodes")
				for _, node := range res.Nodes {
					if node.HostUUID == hostUUID &&
						node.IdentityVerification == noderec.IdentityPinned && node.Trusted &&
						node.DiscoveryRecordID != "" && (pred == nil || pred(node)) {
						return
					}
				}

			}
		case <-ticker.C:
			id++
			sendReq(t, stdin, id, "discovery:get-nodes")
		case <-deadline.C:
			require.FailNow(t, fmt.Sprintf("timed out waiting for authenticated node %q to match predicate", hostUUID))
		}
	}
}
