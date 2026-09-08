// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// dirNode builds a relay DirectoryNode advertising the given engine services on
// one shared proxy port, with per-engine model attribution.
func dirNode(id string, port int, byEngine map[string][]string, services ...noderec.ServiceKey) noderec.DirectoryNode {
	svc := make(map[noderec.ServiceKey]noderec.ServiceStatus, len(services))
	for _, s := range services {
		svc[s] = noderec.ServiceStatus{Port: port}
	}
	return noderec.DirectoryNode{
		Name:           id,
		HostUUID:       id,
		IP:             "192.0.2.10",
		IPs:            []string{"192.0.2.10"},
		Services:       svc,
		ModelsByEngine: byEngine,
	}
}

// TestSubscribedNodeCarriesEveryOpenAIEngine proves one peer advertising both
// lm and vl projects to a single routable node — they name the same proxy port
// — whose inventory is the union and whose attribution is kept per engine.
func TestSubscribedNodeCarriesEveryOpenAIEngine(t *testing.T) {
	n, ok := subscribedToNode(dirNode("peer", 1234, map[string][]string{
		"lmstudio": {"qwen2.5-7b"},
		"vllm":     {"Qwen/Qwen3-8B"},
		"ollama":   {"llama3.2:1b"},
	}, noderec.ServiceLMStudio, noderec.ServiceVLLM))
	if !ok {
		t.Fatal("peer advertising lm and vl was dropped")
	}
	if n.Port != 1234 {
		t.Errorf("port = %d, want the shared proxy port 1234", n.Port)
	}
	want := []string{"Qwen/Qwen3-8B", "qwen2.5-7b"}
	if strings.Join(n.Models, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want the sorted OpenAI union %v (Ollama-only models are not ours)", n.Models, want)
	}
	if got := engineForModel(n.ModelsByEngine, "Qwen/Qwen3-8B"); got != "vllm" {
		t.Errorf("owner of Qwen/Qwen3-8B = %q, want vllm", got)
	}
	if got := engineForModel(n.ModelsByEngine, "qwen2.5-7b"); got != "lmstudio" {
		t.Errorf("owner of qwen2.5-7b = %q, want lmstudio", got)
	}
	if got := engineForModel(n.ModelsByEngine, "llama3.2:1b"); got != "" {
		t.Errorf("an Ollama-only model must not resolve to an OpenAI engine, got %q", got)
	}
}

// TestSubscribedNodeVLLMOnly proves a node that runs only vLLM is routable
// through this proxy, which is the whole point of the second service key.
func TestSubscribedNodeVLLMOnly(t *testing.T) {
	n, ok := subscribedToNode(dirNode("vpeer", 1234, map[string][]string{
		"vllm": {"Qwen/Qwen3-8B"},
	}, noderec.ServiceVLLM))
	if !ok {
		t.Fatal("vLLM-only peer was dropped")
	}
	if len(n.Models) != 1 || n.Models[0] != "Qwen/Qwen3-8B" {
		t.Errorf("models = %v", n.Models)
	}
}

// TestSubscribedNodeWithNoOpenAIEngineIsDropped proves a node running only
// Ollama is not a candidate here, however many models it advertises.
func TestSubscribedNodeWithNoOpenAIEngineIsDropped(t *testing.T) {
	if _, ok := subscribedToNode(dirNode("opeer", 11434, map[string][]string{
		"ollama": {"llama3.2:1b"},
	}, noderec.ServiceOllama)); ok {
		t.Fatal("an Ollama-only node must not be an OpenAI routing candidate")
	}
}

// TestEngineForModelTieBreakIsDeterministic proves a model id served by both
// local engines resolves the same way every time, following openaiEngines order
// rather than map iteration order.
func TestEngineForModelTieBreakIsDeterministic(t *testing.T) {
	byEngine := map[string][]string{
		"lmstudio": {"Qwen/Qwen3-8B"},
		"vllm":     {"Qwen/Qwen3-8B"},
	}
	for range 50 {
		if got := engineForModel(byEngine, "Qwen/Qwen3-8B"); got != "lmstudio" {
			t.Fatalf("tie-break = %q, want the first listed engine lmstudio", got)
		}
	}
}

// TestMixedEngineCandidatesRouteToTheModelOwner proves eligibility and workload
// attribution both follow the per-engine inventory: a request for a vLLM-only
// model reaches the vLLM node and is tagged vllm, and vice versa.
func TestMixedEngineCandidatesRouteToTheModelOwner(t *testing.T) {
	lmNode := nodeForModel(t, "lm-node", "http://192.0.2.11:1234", "qwen2.5-7b")
	vlNode := nodeForModel(t, "vl-node", "http://192.0.2.12:8000", "Qwen/Qwen3-8B")
	vlNode.Engine = "vllm"
	vlNode.ModelsByEngine = map[string][]string{"vllm": {"Qwen/Qwen3-8B"}}

	disc := NewDiscovery()
	disc.AddManual(lmNode)
	disc.AddManual(vlNode)
	p := testProxy(disc, 1235)

	for _, tc := range []struct{ model, wantID, wantEngine string }{
		{"qwen2.5-7b", "lm-node", "lmstudio"},
		{"Qwen/Qwen3-8B", "vl-node", "vllm"},
	} {
		cands := p.resolveCandidates(tc.model)
		if len(cands) != 1 {
			t.Fatalf("%s: %d candidates, want only the owner", tc.model, len(cands))
		}
		if cands[0].id != tc.wantID {
			t.Errorf("%s routed to %q, want %q", tc.model, cands[0].id, tc.wantID)
		}
		if got := candidateEngine(cands[0]); got != tc.wantEngine {
			t.Errorf("%s tagged %q, want %q", tc.model, got, tc.wantEngine)
		}
	}
}

// TestManualNodeIsKeyedPerEngine proves one host added for both engines keeps
// two entries — LM Studio and vLLM sit on different ports, so collapsing them
// would lose one — and that removing one leaves the other routable.
func TestManualNodeIsKeyedPerEngine(t *testing.T) {
	lm := nodeForModel(t, "host", "http://192.0.2.20:1234", "qwen2.5-7b")
	vl := nodeForModel(t, "host", "http://192.0.2.20:8000", "Qwen/Qwen3-8B")
	vl.Engine = "vllm"
	vl.ModelsByEngine = map[string][]string{"vllm": {"Qwen/Qwen3-8B"}}

	disc := NewDiscovery()
	if !disc.AddManual(lm) || !disc.AddManual(vl) {
		t.Fatal("both engine entries should be new")
	}
	if got := len(disc.Nodes()); got != 2 {
		t.Fatalf("nodes = %d, want one entry per engine", got)
	}
	if !disc.RemoveManual("lmstudio", "host") {
		t.Fatal("removing the LM Studio entry should report removed")
	}
	if !disc.IsManual("host") {
		t.Error("the node is still manual while its vLLM entry remains")
	}
	nodes := disc.Nodes()
	if len(nodes) != 1 || nodes[0].Port != 8000 {
		t.Fatalf("surviving node = %+v, want the vLLM entry on 8000", nodes)
	}
	if !disc.RemoveManual("vllm", "host") {
		t.Fatal("removing the vLLM entry should report removed")
	}
	if disc.IsManual("host") {
		t.Error("the node should be gone once its last engine entry is removed")
	}
}

// TestRouteKeyIsEngineQualifiedForManualNodes proves the reachability cache
// cannot carry one engine's confirmed address over to the other engine on the
// same host, while attribution stays keyed by the bare node id.
func TestRouteKeyIsEngineQualifiedForManualNodes(t *testing.T) {
	lm := Node{ID: "host", Engine: "lmstudio"}
	vl := Node{ID: "host", Engine: "vllm"}
	if lm.routeKey() == vl.routeKey() {
		t.Error("two engines on one host must not share a reachability key")
	}
	peer := Node{ID: "host"}
	if peer.routeKey() != "host" {
		t.Errorf("a relay peer's route key = %q, want the bare node id", peer.routeKey())
	}
}

// TestIngressForwardsToTheEngineThatOwnsTheModel proves a peer's inference
// request lands on whichever local OpenAI engine actually serves the model,
// read from the engines themselves rather than from discovery.
func TestIngressForwardsToTheEngineThatOwnsTheModel(t *testing.T) {
	var lmHits, vlHits int
	lm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen2.5-7b"}]}`))
			return
		}
		lmHits++
		_, _ = w.Write([]byte(`{"ok":"lm"}`))
	}))
	defer lm.Close()
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"Qwen/Qwen3-8B"}]}`))
			return
		}
		vlHits++
		_, _ = w.Write([]byte(`{"ok":"vl"}`))
	}))
	defer vl.Close()

	p := testProxy(NewDiscovery(), 1235)
	setBackendFromURL(t, p, "lmstudio", lm.URL)
	setBackendFromURL(t, p, "vllm", vl.URL)

	backends := p.localBackends()
	if len(backends) != 2 {
		t.Fatalf("localBackends = %d, want both engines", len(backends))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"Qwen/Qwen3-8B"}`))
	target, engine, ok := p.ingressTarget(req, backends)
	if !ok || engine != "vllm" {
		t.Fatalf("ingress engine = %q ok=%v, want vllm", engine, ok)
	}
	p.reverseProxyToLocal(httptest.NewRecorder(), req, target)
	if vlHits != 1 || lmHits != 0 {
		t.Errorf("hits lm=%d vl=%d, want the request on vLLM only", lmHits, vlHits)
	}

	// The body must survive the peek that found the model.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen2.5-7b"}`))
	target2, engine2, ok := p.ingressTarget(req2, backends)
	if !ok || engine2 != "lmstudio" {
		t.Fatalf("ingress engine = %q ok=%v, want lmstudio", engine2, ok)
	}
	var seen string
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		seen = string(buf[:n])
	}))
	defer echo.Close()
	_ = target2
	setBackendFromURL(t, p, "lmstudio", echo.URL)
	p.reverseProxyToLocal(httptest.NewRecorder(), req2, mustURL(t, echo.URL))
	if !strings.Contains(seen, "qwen2.5-7b") {
		t.Errorf("forwarded body = %q, want the original request body", seen)
	}
}

// TestIngressModelListMergesEveryLocalEngine proves a peer aggregating this
// node's inventory sees both engines. Forwarding to one would hide the other's
// models and the peer would never route work this node could have run.
func TestIngressModelListMergesEveryLocalEngine(t *testing.T) {
	lm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen2.5-7b"}]}`))
	}))
	defer lm.Close()
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"Qwen/Qwen3-8B"}]}`))
	}))
	defer vl.Close()

	p := testProxy(NewDiscovery(), 1235)
	setBackendFromURL(t, p, "lmstudio", lm.URL)
	setBackendFromURL(t, p, "vllm", vl.URL)

	rec := httptest.NewRecorder()
	p.serveLocalModelList(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil), p.localBackends())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range envelope.Data {
		got[d.ID] = true
	}
	if !got["qwen2.5-7b"] || !got["Qwen/Qwen3-8B"] {
		t.Errorf("aggregated ids = %v, want both engines' models", got)
	}
}

// TestUnhealthyBackendDoesNotDisturbTheOther is the regression guard for the
// backends map: clearing one engine must leave the other routable.
func TestUnhealthyBackendDoesNotDisturbTheOther(t *testing.T) {
	p := testProxy(NewDiscovery(), 1235)
	p.setLocalBackend(localBackend{Engine: "lmstudio", Port: 1234, Healthy: true})
	p.setLocalBackend(localBackend{Engine: "vllm", Port: 8000, Healthy: true})

	p.setLocalBackend(localBackend{Engine: "vllm", Port: 8000, Healthy: false})
	if _, ok := p.localBackendTarget("vllm"); ok {
		t.Error("an unhealthy vLLM backend must not be a target")
	}
	if _, ok := p.localBackendTarget("lmstudio"); !ok {
		t.Error("clearing vLLM must not disturb LM Studio")
	}
	if got := len(p.localBackends()); got != 1 {
		t.Errorf("healthy backends = %d, want 1", got)
	}
	if _, ok := p.localBackendTarget("ollama"); ok {
		t.Error("an engine this proxy never fronts must have no target")
	}
}

// TestOpenAIEnginesCoverEveryServiceKey guards the two halves of the engine
// table against drifting apart: every engine has a discovery key and a name,
// and the subscription list is exactly those keys.
func TestOpenAIEnginesCoverEveryServiceKey(t *testing.T) {
	if len(subscribedServices()) != len(openaiEngines) {
		t.Fatalf("subscribed services = %v, want one per engine", subscribedServices())
	}
	for _, e := range openaiEngines {
		if e.Service == "" || e.Name == "" {
			t.Errorf("incomplete engine binding: %+v", e)
		}
		if !isOpenAIEngine(e.Name) {
			t.Errorf("%q is in the table but not recognized", e.Name)
		}
	}
	if isOpenAIEngine("ollama") {
		t.Error("ollama is fronted by its own proxy, not this one")
	}
}

// setBackendFromURL registers an httptest server as one engine's local backend.
func setBackendFromURL(t *testing.T, p *Proxy, engine, serverURL string) {
	t.Helper()
	u := mustURL(t, serverURL)
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	p.setLocalBackend(localBackend{Engine: engine, Host: host, Port: port, Healthy: true})
	// Each registration changes what the engines serve, so drop the memoized
	// attribution rather than waiting out its TTL.
	p.localModels.mu.Lock()
	p.localModels.byEngine = nil
	p.localModels.fetched = time.Time{}
	p.localModels.mu.Unlock()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
