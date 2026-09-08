// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"nvpair-shared/noderec"
)

// sglangModel is the model id a real SGLang reports. Its --served-model-name
// defaults to --model-path verbatim, so an id can be a bare local directory —
// leading slash and all — rather than the Hugging Face "org/name" shape vLLM
// installs usually show. Nothing here may assume either.
const sglangModel = "/models/my-model"

// TestSGLangIsAFrontedEngine proves the engine table carries SGLang on both of
// its halves: the engine-manager name that gates node/add-manual and tags
// workloads, and the discovery key the proxy subscribes to. A name accepted
// without its key would create entries no peer could ever be discovered for.
func TestSGLangIsAFrontedEngine(t *testing.T) {
	if !isOpenAIEngine("sglang") {
		t.Error("sglang must be recognized as an engine this proxy fronts")
	}
	if !slices.Contains(subscribedServices(), noderec.ServiceSGLang) {
		t.Errorf("subscribed services = %v, want sg among them", subscribedServices())
	}
	if got := engineNames(); len(got) != 3 || got[2] != "sglang" {
		t.Errorf("engineNames = %v, want sglang last in resolution order", got)
	}
}

// TestSubscribedNodeSGLangOnly proves a peer that runs only SGLang is routable
// through this proxy, and that its inventory is read off the sglang attribution
// rather than any other engine's.
func TestSubscribedNodeSGLangOnly(t *testing.T) {
	n, ok := subscribedToNode(dirNode("sgpeer", 1234, map[string][]string{
		"sglang": {sglangModel},
		"ollama": {"llama3.2:1b"},
	}, noderec.ServiceSGLang))
	if !ok {
		t.Fatal("SGLang-only peer was dropped")
	}
	if n.Port != 1234 {
		t.Errorf("port = %d, want the OpenAI proxy port 1234", n.Port)
	}
	if len(n.Models) != 1 || n.Models[0] != sglangModel {
		t.Errorf("models = %v, want only the SGLang inventory", n.Models)
	}
	if got := engineForModel(n.ModelsByEngine, sglangModel); got != "sglang" {
		t.Errorf("owner of %q = %q, want sglang", sglangModel, got)
	}
}

// TestSubscribedNodeCarriesAllThreeOpenAIEngines proves a peer advertising lm,
// vl and sg still projects to one routable node — all three name that peer's
// single proxy port — whose Models is the union and whose attribution stays per
// engine.
func TestSubscribedNodeCarriesAllThreeOpenAIEngines(t *testing.T) {
	n, ok := subscribedToNode(dirNode("peer", 1234, map[string][]string{
		"lmstudio": {"qwen2.5-7b"},
		"vllm":     {"Qwen/Qwen3-8B"},
		"sglang":   {sglangModel},
	}, noderec.ServiceLMStudio, noderec.ServiceVLLM, noderec.ServiceSGLang))
	if !ok {
		t.Fatal("peer advertising lm, vl and sg was dropped")
	}
	if n.Port != 1234 {
		t.Errorf("port = %d, want the one shared proxy port 1234", n.Port)
	}
	want := []string{sglangModel, "Qwen/Qwen3-8B", "qwen2.5-7b"}
	if strings.Join(n.Models, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want the sorted three-engine union %v", n.Models, want)
	}
	for model, engine := range map[string]string{
		"qwen2.5-7b":    "lmstudio",
		"Qwen/Qwen3-8B": "vllm",
		sglangModel:     "sglang",
	} {
		if got := engineForModel(n.ModelsByEngine, model); got != engine {
			t.Errorf("owner of %q = %q, want %q", model, got, engine)
		}
	}
}

// TestUnionModelsAcrossThreeEngines proves the aggregated inventory covers every
// fronted engine and de-duplicates a model more than one of them serves. A
// missing engine here is a node that silently advertises less than it can run.
func TestUnionModelsAcrossThreeEngines(t *testing.T) {
	got := unionModels(map[string][]string{
		"lmstudio": {"shared-model", "qwen2.5-7b"},
		"vllm":     {"shared-model", "Qwen/Qwen3-8B"},
		"sglang":   {"shared-model", sglangModel},
		"ollama":   {"llama3.2:1b"},
	})
	want := []string{sglangModel, "Qwen/Qwen3-8B", "qwen2.5-7b", "shared-model"}
	if !slices.Equal(got, want) {
		t.Errorf("unionModels = %v, want %v (Ollama is not ours)", got, want)
	}
}

// TestEngineTieBreakOrderWithThreeEngines proves a model id every OpenAI engine
// on a node claims resolves by openaiEngines order — lmstudio, then vllm, then
// sglang — every time, rather than by map iteration.
func TestEngineTieBreakOrderWithThreeEngines(t *testing.T) {
	byEngine := map[string][]string{
		"lmstudio": {"shared-model"},
		"vllm":     {"shared-model"},
		"sglang":   {"shared-model"},
	}
	for range 50 {
		if got := engineForModel(byEngine, "shared-model"); got != "lmstudio" {
			t.Fatalf("tie-break = %q, want the first listed engine lmstudio", got)
		}
	}
	// With LM Studio out of the picture the next listed engine wins, which is
	// what makes the order a rank rather than just a default.
	delete(byEngine, "lmstudio")
	if got := engineForModel(byEngine, "shared-model"); got != "vllm" {
		t.Errorf("tie-break without lmstudio = %q, want vllm", got)
	}
	delete(byEngine, "vllm")
	if got := engineForModel(byEngine, "shared-model"); got != "sglang" {
		t.Errorf("tie-break without lmstudio or vllm = %q, want sglang", got)
	}
}

// TestLocalBackendsHoldAllThreeEngines proves one node can serve LM Studio,
// vLLM and SGLang at once and that clearing one leaves the other two routable.
// The backends are keyed by engine precisely so an engine going down is not a
// node going down.
func TestLocalBackendsHoldAllThreeEngines(t *testing.T) {
	p := testProxy(NewDiscovery(), 1235)
	p.setLocalBackend(localBackend{Engine: "lmstudio", Port: 1234, Healthy: true})
	p.setLocalBackend(localBackend{Engine: "vllm", Port: 8000, Healthy: true})
	p.setLocalBackend(localBackend{Engine: "sglang", Port: 30000, Healthy: true})
	if got := len(p.localBackends()); got != 3 {
		t.Fatalf("healthy backends = %d, want all three engines", got)
	}
	target, ok := p.localBackendTarget("sglang")
	if !ok {
		t.Fatal("the SGLang backend must be a target")
	}
	if !strings.HasSuffix(target.Host, ":30000") {
		t.Errorf("sglang target = %q, want SGLang's own port 30000", target.Host)
	}

	p.setLocalBackend(localBackend{Engine: "sglang", Port: 30000, Healthy: false})
	if _, ok := p.localBackendTarget("sglang"); ok {
		t.Error("an unhealthy SGLang backend must not be a target")
	}
	for _, engine := range []string{"lmstudio", "vllm"} {
		if _, ok := p.localBackendTarget(engine); !ok {
			t.Errorf("clearing SGLang must not disturb %s", engine)
		}
	}
}

// TestAddManualAcceptsSGLang drives node/add-manual over the RPC surface with
// engine "sglang", which is the wire change a broker depends on: the engine
// field is validated against the fronted set, so an engine the table doesn't
// carry is refused at the boundary. It also proves the entry is keyed by
// (engine, node), so SGLang on 30000 sits alongside vLLM on 8000 for one host
// instead of replacing it.
func TestAddManualAcceptsSGLang(t *testing.T) {
	rw := &captureRW{}
	disc := NewDiscovery()
	p := NewProxy(NewCodec(rw), disc, 1235)

	addManual(t, p, 1, map[string]any{
		"id": "host", "engine": "vllm", "host": "192.0.2.30", "port": 8000,
		"addresses": []string{"192.0.2.30"}, "models": []string{"Qwen/Qwen3-8B"},
	})
	addManual(t, p, 2, map[string]any{
		"id": "host", "engine": "sglang", "host": "192.0.2.30", "port": 30000,
		"addresses": []string{"192.0.2.30"}, "models": []string{sglangModel},
	})

	for _, resp := range rw.replies(t) {
		if resp.Error != nil {
			t.Fatalf("node/add-manual rejected: %+v", resp.Error)
		}
	}
	nodes := disc.Nodes()
	if len(nodes) != 2 {
		t.Fatalf("nodes = %+v, want one entry per engine on the same host", nodes)
	}
	var sg *Node
	for i := range nodes {
		if nodes[i].Engine == "sglang" {
			sg = &nodes[i]
		}
	}
	if sg == nil {
		t.Fatalf("no sglang entry: %+v", nodes)
	}
	if sg.Port != 30000 {
		t.Errorf("sglang port = %d, want SGLang's own 30000", sg.Port)
	}
	// A manual node's whole inventory belongs to the engine it was added for, so
	// the model id — a bare path here — must resolve back to sglang and to no
	// other engine.
	if got := engineForModel(sg.ModelsByEngine, sglangModel); got != "sglang" {
		t.Errorf("manual SGLang model owner = %q, want sglang", got)
	}
}

// TestAddManualRejectsAnUnfrontedEngine is the other half of the boundary
// check: a near-miss on an engine name must be refused rather than creating an
// entry nothing can route to, and the rejection must name the engines that are
// accepted so the caller can see sglang is among them.
func TestAddManualRejectsAnUnfrontedEngine(t *testing.T) {
	rw := &captureRW{}
	disc := NewDiscovery()
	p := NewProxy(NewCodec(rw), disc, 1235)

	addManual(t, p, 1, map[string]any{
		"id": "host", "engine": "sglang-typo", "host": "192.0.2.30", "port": 30000,
		"addresses": []string{"192.0.2.30"},
	})

	replies := rw.replies(t)
	if len(replies) != 1 || replies[0].Error == nil {
		t.Fatalf("an unfronted engine was accepted: %+v", replies)
	}
	if !strings.Contains(replies[0].Error.Message, "sglang") {
		t.Errorf("rejection %q should name the engines that are accepted", replies[0].Error.Message)
	}
	if len(disc.Nodes()) != 0 {
		t.Errorf("a rejected add left an entry: %+v", disc.Nodes())
	}
}

func addManual(t *testing.T, p *Proxy, id int, params map[string]any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	rawID := json.RawMessage(strconv.Itoa(id))
	p.handleMessage(&Message{JSONRPC: "2.0", ID: &rawID, Method: "node/add-manual", Params: raw})
}

// captureRW is a codec transport that records everything the proxy writes and
// reads EOF, so a request can be handed to handleMessage in-process and its
// reply read back.
type captureRW struct{ out bytes.Buffer }

func (c *captureRW) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *captureRW) Write(p []byte) (int, error) { return c.out.Write(p) }

func (c *captureRW) replies(t *testing.T) []Message {
	t.Helper()
	var out []Message
	for _, line := range strings.Split(strings.TrimSpace(c.out.String()), "\n") {
		if line == "" {
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("unparseable frame %q: %v", line, err)
		}
		if msg.IsResponse() {
			out = append(out, msg)
		}
	}
	return out
}
