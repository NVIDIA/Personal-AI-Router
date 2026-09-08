// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sglangModelPath is the model id a real SGLang reports: --served-model-name
// defaults to --model-path verbatim, so a container started with
// `--model-path /models/my-model` advertises exactly that. Nothing in the
// probe may assume the Hugging Face "org/name" shape.
const sglangModelPath = "/models/my-model"

// sglangStub serves the routes probeSGLang requires. modelPath == "" omits the
// model_path field, standing in for an OpenAI-compatible server that is not
// SGLang. servesVersion additionally answers vLLM's GET /version, which a real
// SGLang does not — that combination exists only to exercise probeVLLM's guard.
func sglangStub(t *testing.T, modelPath string, models []string, servesVersion bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/get_model_info":
			if modelPath == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_path":     modelPath,
				"tokenizer_path": modelPath,
				"is_generation":  true,
			})
		case "/version":
			if !servesVersion {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.5.0"})
		case "/v1/models":
			data := make([]map[string]any, 0, len(models))
			for _, id := range models {
				data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "sglang", "root": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeSGLangReportsModels covers the SGLang probe against a real HTTP
// server: a node answering both /get_model_info and /v1/models reports up with
// its model ids, path-shaped id and all.
func TestProbeSGLangReportsModels(t *testing.T) {
	m, _, _ := newTestManager()
	m.client = http.DefaultClient
	srv := sglangStub(t, sglangModelPath, []string{sglangModelPath}, false)
	host, port := stubHostPort(t, srv)

	up, models := m.probeSGLang(host, port)
	if !up {
		t.Fatal("expected sglang up")
	}
	if len(models) != 1 || models[0] != sglangModelPath {
		t.Fatalf("models = %#v, want the served model path carried verbatim", models)
	}
}

// TestProbeSGLangRejectsAnOpenAIServerThatIsNotSGLang is the disambiguator
// guard: LM Studio and vLLM serve the same /v1/models but neither serves
// /get_model_info, so an OpenAI server without it must never be reported as
// SGLang.
func TestProbeSGLangRejectsAnOpenAIServerThatIsNotSGLang(t *testing.T) {
	m, _, _ := newTestManager()
	m.client = http.DefaultClient
	srv := sglangStub(t, "", []string{"qwen2.5-7b"}, false)
	host, port := stubHostPort(t, srv)

	if up, models := m.probeSGLang(host, port); up || models != nil {
		t.Fatalf("an OpenAI server without /get_model_info was reported as SGLang: up=%v models=%#v", up, models)
	}
}

// TestProbeSGLangAbsentNodeIsDown covers the unreachable case.
func TestProbeSGLangAbsentNodeIsDown(t *testing.T) {
	m, _, _ := newTestManager()
	if up, models := m.probeSGLang("absent.local", defaultSGLangPort); up || models != nil {
		t.Fatalf("expected absent sglang down, got up=%v models=%#v", up, models)
	}
}

// TestVLLMAndSGLangProbesDoNotClaimEachOther pins both directions of the
// disambiguation. A vLLM (serves /version, 404 on /get_model_info) is vLLM and
// not SGLang. A server that answers /get_model_info is SGLang and must not be
// claimed by probeVLLM even when it also answers /version — that combination is
// hypothetical (a real SGLang answers no /version, so it is already rejected on
// that leg alone) and the case exists to document the guard: /get_model_info is
// SGLang's own route, so a server answering it is SGLang whatever else it
// serves.
func TestVLLMAndSGLangProbesDoNotClaimEachOther(t *testing.T) {
	m, _, _ := newTestManager()
	m.client = http.DefaultClient

	t.Run("vllm stub", func(t *testing.T) {
		// vllmStub serves /version and 404s everything else, /get_model_info
		// included — exactly what a real vLLM does.
		host, port := stubHostPort(t, vllmStub(t, "0.11.0", []string{"Qwen/Qwen3-8B"}))
		up, models := m.probeVLLM(host, port)
		if !up || len(models) != 1 || models[0] != "Qwen/Qwen3-8B" {
			t.Errorf("probeVLLM on a vLLM = up:%v models:%#v, want it accepted", up, models)
		}
		if up, models := m.probeSGLang(host, port); up || models != nil {
			t.Errorf("a vLLM was reported as SGLang: up=%v models=%#v", up, models)
		}
	})

	t.Run("server answering get_model_info", func(t *testing.T) {
		host, port := stubHostPort(t, sglangStub(t, sglangModelPath, []string{sglangModelPath}, true))
		if up, models := m.probeVLLM(host, port); up || models != nil {
			t.Errorf("a server answering /get_model_info was reported as vLLM: up=%v models=%#v", up, models)
		}
		up, models := m.probeSGLang(host, port)
		if !up || len(models) != 1 || models[0] != sglangModelPath {
			t.Errorf("probeSGLang = up:%v models:%#v, want it accepted", up, models)
		}
	})

	t.Run("real sglang serves no version", func(t *testing.T) {
		// The shape the live engine actually has: /get_model_info answers,
		// /version 404s. probeVLLM rejects it on the /version leg before the
		// guard is even reached, which is why the guard is defense in depth.
		host, port := stubHostPort(t, sglangStub(t, sglangModelPath, []string{sglangModelPath}, false))
		if up, _ := m.probeVLLM(host, port); up {
			t.Error("an SGLang was reported as vLLM")
		}
		if up, _ := m.probeSGLang(host, port); !up {
			t.Error("expected the SGLang to be accepted by its own probe")
		}
	})
}

// TestSGLangCountsAsReachable proves a node that runs only SGLang is not treated
// as unreachable, which would otherwise raise a probe-failed error for a healthy
// node and drop it from routing. It asserts against the manager's own
// reachability decision rather than a re-derived expression, so a leg missing
// from probeNode cannot pass here.
func TestSGLangCountsAsReachable(t *testing.T) {
	m, rw, _ := newTestManager()
	m.client = http.DefaultClient
	srv := sglangStub(t, sglangModelPath, []string{sglangModelPath}, false)
	host, port := stubHostPort(t, srv)
	dead := closedPort(t)

	// Only the SGLang leg has anything to reach: every other service is pointed
	// at a closed port, so the node's reachability rests on SGLang alone. Pointing
	// them at the stub instead would not do — it answers /v1/models, which is all
	// LM Studio's deliberately lenient probe asks for.
	status := m.addNode(ManualEntry{Address: host, Ports: &ManualPorts{
		NodeInfo: dead, Cluster: dead, Ollama: dead, LMStudio: dead, VLLM: dead, SGLang: port,
	}})
	got := decodeParams[ManualNodeStatus](t, readCaptureUntil(t, rw, methodIs("node/discovered")))

	if !got.SGLangUp {
		t.Fatalf("sglang leg did not come up: %+v", got)
	}
	if got.OllamaUp || got.LMStudioUp || got.VLLMUp || got.NodeInfoUp {
		t.Fatalf("a leg pointed at a closed port reported up: %+v", got)
	}
	if len(got.SGLangModels) != 1 || got.SGLangModels[0] != sglangModelPath {
		t.Errorf("sglang models = %v, want the served model path", got.SGLangModels)
	}
	if got.SGLangPort != port {
		t.Errorf("sglang port = %d, want the override %d", got.SGLangPort, port)
	}

	// The manager's own reachability decision, not one re-derived here: a node
	// running nothing but SGLang must not accumulate probe failures, which would
	// raise a probe-failed error for a healthy node and drop it from routing.
	m.mu.RLock()
	fails := m.nodes[status.ID].consecutiveFails
	m.mu.RUnlock()
	if fails != 0 {
		t.Errorf("consecutiveFails = %d, want 0: an SGLang-only node is reachable", fails)
	}
}

// closedPort returns a port on loopback that nothing is listening on, so a probe
// against it fails immediately with a connection refusal rather than waiting out
// a timeout.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}
