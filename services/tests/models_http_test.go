// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process test for model inventory from a pinned cluster peer. Scanner
// authenticates its node-info metadata, then fetches /v1/models over mTLS and
// carries the flat, per-engine, and loaded inventories through the broker.
package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grandcat/zeroconf"

	"nvpair-shared/noderec"
)

func TestDiscovery_EnrichesModelsThroughHTTP(t *testing.T) {
	const instance = "xproc-models-1"
	cluster := newInterNodeCluster(t)
	cluster.startModelsPeer(t, instance, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flat union + per-engine attribution, exactly the shape engine-manager's
		// ModelsResult serializes. The daemon must enrich both onto the node.
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
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
		}))
	}))

	stdin, msgs, _, cleanup := startBrokerWithDirs(t, t.TempDir(), cluster.nodeDir)
	t.Cleanup(cleanup)

	pollForNode(t, stdin, msgs, instance, cluster.peerUUID, 30*time.Second, func(n availableNode) bool {
		return modelsMatch(n.Models) && modelsByEngineMatch(n.ModelsByEngine) &&
			loadedByEngineMatch(n.LoadedByEngine)
	})
}

// startModelsPeer serves authenticated node metadata and a model handler using
// the same pinned identity. The one announcement remains fixed during refresh tests.
func (c *interNodeCluster) startModelsPeer(t *testing.T, instance string, models http.Handler) {
	t.Helper()
	var port int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", func(w http.ResponseWriter, r *http.Request) {
		info := noderec.NodeInfo{
			HostUUID: c.peerUUID, ClusterUUID: &c.peerUUID,
			Peer: &noderec.PeerMetadata{
				Name: instance, Addresses: []string{"127.0.0.1"},
				Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: port, noderec.ServiceEngineManager: port},
			},
		}
		assert.NoError(t, json.NewEncoder(w).Encode(info))
	})
	mux.Handle("/v1/models", models)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := c.peer.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	server.TLS = c.peer.ServerTLSConfig()
	server.StartTLS()
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err, "parse model peer URL")
	port, err = strconv.Atoi(u.Port())
	require.NoError(t, err, "model peer port")

	txt := []string{"v=1", "uuid=" + c.peerUUID, "cluster-uuid=" + c.peerUUID, "ip=127.0.0.1",
		fmt.Sprintf("ni=%d", port), fmt.Sprintf("em=%d", port)}
	advertisement, err := zeroconf.Register(instance, nodeRecordService, testDomain, port, txt, nil)
	require.NoError(t, err, "register model peer")
	t.Cleanup(advertisement.Shutdown)
	t.Logf("advertising %s with pinned node-info and models (port=%d)", instance, port)
}

func modelsMatch(models []string) bool {
	want := []string{"llama3:8b", "qwen:0.5b", "org/router-model-GGUF:Q4_K_M"}
	if len(models) != len(want) {
		return false
	}
	for i := range want {
		if models[i] != want[i] {
			return false
		}
	}
	return true
}

func modelsByEngineMatch(byEngine map[string][]string) bool {
	want := map[string][]string{
		"ollama":   {"llama3:8b"},
		"lmstudio": {"qwen:0.5b"},
		"llamacpp": {"org/router-model-GGUF:Q4_K_M"},
	}
	return byEngineEqual(byEngine, want)
}

// loadedByEngineMatch asserts the loaded set the stub served (Ollama and
// llama.cpp each have one resident model; LM Studio is running but empty)
// survives the daemon->broker projection onto the client-facing node.
func loadedByEngineMatch(loaded map[string][]string) bool {
	want := map[string][]string{
		"ollama":   {"llama3:8b"},
		"lmstudio": {},
		"llamacpp": {"org/router-model-GGUF:Q4_K_M"},
	}
	return byEngineEqual(loaded, want)
}

// byEngineEqual reports whether a per-engine map equals want: same key set, each
// with the same ordered list (an empty list and a present-but-empty key both
// count, so "running, nothing loaded" is distinguishable from a missing key).
func byEngineEqual(got, want map[string][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for engine, wantList := range want {
		gotList, ok := got[engine]
		if !ok || len(gotList) != len(wantList) {
			return false
		}
		for i := range wantList {
			if gotList[i] != wantList[i] {
				return false
			}
		}
	}
	return true
}
