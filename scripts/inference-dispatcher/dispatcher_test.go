// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func runAgainstServer(
	t *testing.T,
	ctx context.Context,
	args []string,
	baseURL string,
	stdout, stderr *bytes.Buffer,
) int {
	t.Helper()
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	client, err := newBackendClient(cfg)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	client.base, err = url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	if cfg.ListModels {
		models, err := client.listModels(ctx)
		if err != nil {
			t.Fatalf("list models: %v", err)
		}
		if err := json.NewEncoder(stdout).Encode(models); err != nil {
			t.Fatalf("encode models: %v", err)
		}
		return 0
	}
	return newDispatcher(cfg, client, stdout, stderr).run(ctx)
}

func TestOmittedModelSelectsAvailableGenerationModel(t *testing.T) {
	var receivedModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[
				{"name":"z-embed","capabilities":["embedding"]},
				{"name":"b-chat","capabilities":["chat"]},
				{"name":"a-completion","capabilities":["completion"]}
			]}`))
		case "/api/generate":
			var request struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			receivedModel = request.Model
			_, _ = w.Write([]byte(`{"response":"hello"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exit := runAgainstServer(
		t,
		context.Background(),
		[]string{"--prompt", "Say hello"},
		server.URL,
		&stdout,
		&stderr,
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if receivedModel != "a-completion" {
		t.Fatalf("selected model %q, want deterministic first generation model", receivedModel)
	}
	if !strings.Contains(stdout.String(), "Auto-selected available model") {
		t.Fatalf("missing auto-selection output: %s", stdout.String())
	}
}

func TestExplicitModelSkipsInventoryRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			t.Fatal("explicit model unexpectedly queried inventory")
		}
		_, _ = w.Write([]byte(`{"response":"ok"}`))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exit := runAgainstServer(
		t,
		context.Background(),
		[]string{"--model", "chosen", "--prompt", "test"},
		server.URL,
		&stdout,
		&stderr,
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
}

func TestLMStudioFallsBackToOpenAIInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			http.Error(w, "not found", http.StatusNotFound)
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"live-model"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exit := runAgainstServer(
		t,
		context.Background(),
		[]string{"--backend", "lmstudio", "--prompt", "test"},
		server.URL,
		&stdout,
		&stderr,
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "live-model") {
		t.Fatalf("selected model missing from output: %s", stdout.String())
	}
}

// A Personal AI Router proxy answers /v1/models with the whole cluster's
// inventory and forwards /api/v1/models to one node, so the aggregated list must
// win. The native list still supplies the type and capability fields the
// generation filter needs.
func TestLMStudioPrefersAggregatedInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"remote-chat"},
				{"id":"local-embed"},
				{"id":"local-chat"}
			]}`))
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"models":[
				{"key":"local-embed","type":"embeddings"},
				{"key":"local-chat","type":"llm"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exit := runAgainstServer(
		t,
		context.Background(),
		[]string{"--backend", "lmstudio", "--list-models"},
		server.URL,
		&stdout,
		&stderr,
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var models []RegisteredModel
	if err := json.Unmarshal(stdout.Bytes(), &models); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	byName := make(map[string]RegisteredModel, len(models))
	for _, model := range models {
		byName[model.Name] = model
	}
	if len(models) != 3 {
		t.Fatalf("models=%v, want the aggregated three-model inventory", models)
	}
	if _, ok := byName["remote-chat"]; !ok {
		t.Fatal("a model known only to the aggregated endpoint was dropped")
	}
	if byName["local-embed"].Type != "embeddings" {
		t.Fatalf("native type metadata was not merged: %v", byName["local-embed"])
	}
	if supportsGeneration(byName["local-embed"]) {
		t.Fatal("merged metadata did not restore the generation filter")
	}
	// No metadata arrived for the remote model, so it stays eligible rather than
	// being excluded for something the single-node endpoint could not report.
	if !supportsGeneration(byName["remote-chat"]) {
		t.Fatal("a model without native metadata was wrongly excluded")
	}
}

func TestListModelsEmitsJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exit := runAgainstServer(
		t,
		context.Background(),
		[]string{"--list-models"},
		server.URL,
		&stdout,
		&stderr,
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var models []RegisteredModel
	if err := json.Unmarshal(stdout.Bytes(), &models); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(models) != 1 || models[0].Name != "test-model" {
		t.Fatalf("models=%v", models)
	}
}

func TestInvalidConfigurationDoesNotSendRequests(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runMain(
		context.Background(),
		[]string{"--count", "0"},
		&stdout,
		&stderr,
	)
	if exit != 2 || !strings.Contains(stderr.String(), "--count") {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
}

func TestCancellationStopsInFlightRequestCleanly(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseServer := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"available"}]}`))
		case "/api/generate":
			once.Do(func() { close(requestStarted) })
			<-releaseServer
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runAgainstServer(
			t,
			ctx,
			[]string{"--fail-on-error"},
			server.URL,
			&stdout,
			&stderr,
		)
	}()

	select {
	case <-requestStarted:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("inference request did not start")
	}
	select {
	case exit := <-done:
		if exit != 0 {
			t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
		}
		close(releaseServer)
	case <-time.After(2 * time.Second):
		close(releaseServer)
		t.Fatal("dispatcher did not stop after cancellation")
	}
}

func TestHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := runMain(context.Background(), []string{"--help"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
}

func TestConnectionIsLocalOnly(t *testing.T) {
	for _, option := range []string{"--host", "--base-url"} {
		var stdout, stderr bytes.Buffer
		exit := runMain(context.Background(), []string{option, "example.test"}, &stdout, &stderr)
		if exit != 2 || !strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("%s: exit=%d stderr=%s", option, exit, stderr.String())
		}
	}
}

// The default must be the proxy facade (1234), not PAIR's managed LM Studio
// backend (1235+). Defaulting to the backend would bypass lmstudio-proxy and
// route nothing through the cluster.
func TestLMStudioDefaultPort(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := parseConfig([]string{"--backend", "lmstudio"}, &stderr)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if port := effectivePort(cfg); port != 1234 {
		t.Fatalf("LM Studio default port=%d, want 1234", port)
	}
}

func TestResponseTextNeverReachesStdout(t *testing.T) {
	const secret = "the capital of France is Paris"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"available"}]}`))
		case "/api/generate":
			_, _ = w.Write([]byte(`{"response":"` + secret + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exit := runAgainstServer(t, context.Background(), nil, server.URL, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "Paris") || strings.Contains(combined, secret) {
		t.Fatalf("response text reached the console: %s", combined)
	}
	if !strings.Contains(stdout.String(), "response=sha256:") {
		t.Fatalf("missing response digest: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("bytes=%d", len(secret))) {
		t.Fatalf("missing response byte count: %s", stdout.String())
	}
}

// A non-2xx body from an OpenAI-compatible endpoint can echo the request back,
// and result.Error reaches both the JSONL result log and the debug error log.
func TestUpstreamErrorBodyNeverReachesLogs(t *testing.T) {
	const echoed = "invalid request: summarize the quarterly revenue memo"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"available"}]}`))
		default:
			http.Error(w, `{"error":{"message":"`+echoed+`"}}`, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	resultLog := filepath.Join(t.TempDir(), "results.jsonl")
	errorLog := filepath.Join(t.TempDir(), "errors.log")
	var stdout, stderr bytes.Buffer
	runAgainstServer(
		t,
		context.Background(),
		[]string{
			"--prompt", "summarize the quarterly revenue memo",
			"--result-log", resultLog,
			"--debug-errors",
			"--debug-error-log", errorLog,
		},
		server.URL,
		&stdout,
		&stderr,
	)

	results, err := os.ReadFile(resultLog)
	if err != nil {
		t.Fatalf("read result log: %v", err)
	}
	errors, err := os.ReadFile(errorLog)
	if err != nil {
		t.Fatalf("read error log: %v", err)
	}
	for name, content := range map[string]string{
		"stdout":     stdout.String(),
		"stderr":     stderr.String(),
		"result log": string(results),
		"error log":  string(errors),
	} {
		if strings.Contains(content, "quarterly") || strings.Contains(content, echoed) {
			t.Fatalf("%s leaked the upstream error body: %s", name, content)
		}
		if name == "stdout" {
			continue
		}
		if !strings.Contains(content, "400 Bad Request") {
			t.Fatalf("%s dropped the HTTP status: %s", name, content)
		}
	}
}

func TestPromptDigestDoesNotLeakPromptText(t *testing.T) {
	const prompt = "classify this support request into one category"
	digest := promptDigest(prompt)
	if strings.Contains(digest, "classify") || strings.Contains(digest, "support") {
		t.Fatalf("digest leaked prompt text: %s", digest)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest=%q, want a sha256: prefix", digest)
	}
	if digest == promptDigest(prompt+" more") {
		t.Fatal("digest did not distinguish different prompts")
	}
	if digest != promptDigest(prompt) {
		t.Fatal("digest is not stable for the same prompt")
	}
}
