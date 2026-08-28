// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process test for the models-http path: a node advertises an
// engine-manager (em) service on its _nvpair-node record, and the promoted
// discovery daemon enriches the node's model list by fetching /v1/models over
// plain HTTP from that em port — replacing the retired models= TXT key. We stand
// in for engine-manager with a tiny HTTP server, advertise em=<port> + ip so the
// daemon dials it over loopback, and assert the enriched list surfaces on the
// broker's discovery:get-nodes response.
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

	"github.com/grandcat/zeroconf"
)

func TestModelsHTTPEnrichment(t *testing.T) {
	// Stand-in engine-manager HTTP surface serving the model list.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		// Flat union + per-engine attribution, exactly the shape engine-manager's
		// ModelsResult serializes. The daemon must enrich both onto the node.
		_ = json.NewEncoder(w).Encode(map[string]any{
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
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	emPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("stub port: %v", err)
	}

	const instance = "xproc-models-1"
	// Advertise a _nvpair-node with em=<port> and ip=127.0.0.1 so the daemon
	// fetches the model list from the stub over loopback (ip= wins over the mDNS
	// address in the daemon's enrichment target selection).
	txt := []string{"v=1", "uuid=" + instance + "-uuid", "ip=127.0.0.1", fmt.Sprintf("em=%d", emPort)}
	zsrv, err := zeroconf.Register(instance, nodeRecordService, testDomain, emPort, txt, nil)
	if err != nil {
		t.Fatalf("register %s: %v", instance, err)
	}
	t.Cleanup(zsrv.Shutdown)
	t.Logf("advertising %s @ %s (em=%d)", instance, nodeRecordService, emPort)

	stdin, msgs, cleanup := startBroker(t)
	t.Cleanup(cleanup)

	// Poll get-nodes until the node appears carrying the enriched model list.
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	id := 100
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
					if n, found := findNode(res.Nodes, instance); found &&
						modelsMatch(n.Models) && modelsByEngineMatch(n.ModelsByEngine) &&
						loadedByEngineMatch(n.LoadedByEngine) {
						return // enriched flat + per-engine + loaded lists surfaced
					}
				}
			}
		case <-ticker.C:
			id++
			sendReq(t, stdin, id, "discovery:get-nodes")
		case <-deadline:
			t.Fatalf("timed out waiting for %q with an enriched model list", instance)
		}
	}
}

func findNode(nodes []availableNode, id string) (availableNode, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return availableNode{}, false
}

func modelsMatch(models []string) bool {
	want := []string{"llama3:8b", "qwen:0.5b"}
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
	}
	return byEngineEqual(byEngine, want)
}

// loadedByEngineMatch asserts the loaded set the stub served (ollama has one
// resident model; lmstudio is running but empty) survives the daemon->broker
// projection onto the client-facing node.
func loadedByEngineMatch(loaded map[string][]string) bool {
	want := map[string][]string{
		"ollama":   {"llama3:8b"},
		"lmstudio": {},
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
