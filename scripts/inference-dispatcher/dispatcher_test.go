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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runAgainstServer(
	t *testing.T,
	ctx context.Context,
	args []string,
	baseURL string,
	stdout, stderr *bytes.Buffer,
) int {
	t.Helper()
	cfg, client := configuredTestClient(t, args, baseURL, stderr)
	if cfg.ListModels {
		models, err := client.listModels(ctx)
		require.NoError(t, err, "list models")
		require.NoError(t, json.NewEncoder(stdout).Encode(models), "encode models")
		return 0
	}
	return newDispatcher(cfg, client, stdout, stderr).run(ctx)
}

func configuredTestClient(t *testing.T, args []string, baseURL string, stderr *bytes.Buffer) (Config, *backendClient) {
	t.Helper()
	cfg, err := parseConfig(args, stderr)
	require.NoError(t, err, "parse config")
	client, err := newBackendClient(cfg)
	require.NoError(t, err, "create client")
	client.base, err = url.Parse(baseURL)
	require.NoError(t, err, "parse test server URL")
	return cfg, client
}

func TestOmittedModelSelectsAvailableGenerationModel(t *testing.T) {
	var receivedModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, err := w.Write([]byte(`{"models":[
				{"name":"z-embed","capabilities":["embedding"]},
				{"name":"b-chat","capabilities":["chat"]},
				{"name":"a-completion","capabilities":["completion"]}
			]}`))
			assert.NoError(t, err, "write engine response")
		case "/api/generate":
			var request struct {
				Model string `json:"model"`
			}
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request), "decode request") {
				return
			}
			receivedModel = request.Model
			_, err := w.Write([]byte(`{"response":"hello"}`))
			assert.NoError(t, err, "write engine response")
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
	assert.Equal(t, 0, exit, "stderr: %s", stderr.String())
	assert.Equal(t, "a-completion", receivedModel, "selected model")
	assert.Contains(t, stdout.String(), "Auto-selected available model", "missing auto-selection output")
}

func TestExplicitModelSkipsInventoryRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			assert.Fail(t, "explicit model unexpectedly queried inventory")
			return
		}
		_, err := w.Write([]byte(`{"response":"ok"}`))
		assert.NoError(t, err, "write engine response")
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
	assert.Equal(t, 0, exit, "stderr: %s", stderr.String())
}

func TestLMStudioFallsBackToOpenAIInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			http.Error(w, "not found", http.StatusNotFound)
		case "/v1/models":
			_, err := w.Write([]byte(`{"data":[{"id":"live-model"}]}`))
			assert.NoError(t, err, "write engine response")
		case "/v1/chat/completions":
			_, err := w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
			assert.NoError(t, err, "write engine response")
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
	assert.Equal(t, 0, exit, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "live-model", "selected model missing from output")
}

func TestLlamaCPPUsesOpenAIInventoryAndChat(t *testing.T) {
	type observedRequest struct {
		method       string
		path         string
		model        string
		messageCount int
	}
	observed := make(chan observedRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			observed <- observedRequest{method: r.Method, path: r.URL.Path}
			_, _ = w.Write([]byte(`{"data":[{"id":"llama-demo"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			var request struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&request), "decode chat request")
			observed <- observedRequest{
				method:       r.Method,
				path:         r.URL.Path,
				model:        request.Model,
				messageCount: len(request.Messages),
			}
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
		[]string{"--backend", "llamacpp", "--prompt", "test"},
		server.URL,
		&stdout,
		&stderr,
	)
	require.Equal(t, 0, exit, "stderr: %s", stderr.String())
	inventory := <-observed
	require.Equal(t, http.MethodGet, inventory.method)
	require.Equal(t, "/v1/models", inventory.path)
	require.Equal(t, observedRequest{http.MethodPost, "/v1/chat/completions", "llama-demo", 1}, <-observed)
}

// A Personal AI Router proxy answers /v1/models with the whole cluster's
// inventory and forwards /api/v1/models to one node, so the aggregated list must
// win. The native list still supplies the type and capability fields the
// generation filter needs.
func TestLMStudioPrefersAggregatedInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, err := w.Write([]byte(`{"data":[
				{"id":"remote-chat"},
				{"id":"local-embed"},
				{"id":"local-chat"}
			]}`))
			assert.NoError(t, err, "write engine response")
		case "/api/v1/models":
			_, err := w.Write([]byte(`{"models":[
				{"key":"local-embed","type":"embeddings"},
				{"key":"local-chat","type":"llm"}
			]}`))
			assert.NoError(t, err, "write engine response")
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
	assert.Equal(t, 0, exit, "stderr: %s", stderr.String())
	var models []RegisteredModel
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &models), "invalid JSON")
	byName := make(map[string]RegisteredModel, len(models))
	for _, model := range models {
		byName[model.Name] = model
	}
	require.Len(t, models, 3)
	require.Contains(t, byName, "remote-chat", "a model known only to the aggregated endpoint was dropped")
	assert.Equal(t, "embeddings", byName["local-embed"].Type, "native type metadata was not merged")
	assert.False(t, supportsGeneration(byName["local-embed"]), "merged metadata did not restore the generation filter")
	// No metadata arrived for the remote model, so it stays eligible rather than
	// being excluded for something the single-node endpoint could not report.
	assert.True(t, supportsGeneration(byName["remote-chat"]), "a model without native metadata was wrongly excluded")
}

func TestListModelsEmitsJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
		assert.NoError(t, err, "write engine response")
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
	assert.Equal(t, 0, exit, "stderr: %s", stderr.String())
	var models []RegisteredModel
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &models), "invalid JSON")
	require.Len(t, models, 1)
	assert.Equal(t, "test-model", models[0].Name)
}

func TestInvalidConfigurationDoesNotSendRequests(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := runMain(
		context.Background(),
		[]string{"--count", "0"},
		&stdout,
		&stderr,
	)
	assert.Equal(t, 2, exit, "stderr: %s", stderr.String())
	assert.Contains(t, stderr.String(), "--count")
}

func TestCancellationStopsInFlightRequestCleanly(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseServer := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, err := w.Write([]byte(`{"models":[{"name":"available"}]}`))
			assert.NoError(t, err, "write engine response")
		case "/api/generate":
			once.Do(func() { close(requestStarted) })
			<-releaseServer
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	cfg, client := configuredTestClient(t, []string{"--fail-on-error"}, server.URL, &stderr)
	done := make(chan int, 1)
	go func() {
		done <- newDispatcher(cfg, client, &stdout, &stderr).run(ctx)
	}()

	select {
	case <-requestStarted:
		cancel()
	case <-time.After(2 * time.Second):
		require.FailNow(t, "inference request did not start")
	}
	select {
	case exit := <-done:
		assert.Equal(t, 0, exit, "stderr: %s", stderr.String())
		close(releaseServer)
	case <-time.After(2 * time.Second):
		close(releaseServer)
		require.FailNow(t, "dispatcher did not stop after cancellation")
	}
}

func TestHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	assert.Equal(t, 0, runMain(context.Background(), []string{"--help"}, &stdout, &stderr), "stderr: %s", stderr.String())
}

func TestConnectionIsLocalOnly(t *testing.T) {
	for _, option := range []string{"--host", "--base-url"} {
		var stdout, stderr bytes.Buffer
		exit := runMain(context.Background(), []string{option, "example.test"}, &stdout, &stderr)
		assert.Equal(t, 2, exit, "%s stderr: %s", option, stderr.String())
		assert.Contains(t, stderr.String(), "flag provided but not defined", "%s", option)
	}
}

// The default must be the proxy facade (1234), not PAIR's managed LM Studio
// backend (1235+). Defaulting to the backend would bypass lmstudio-proxy and
// route nothing through the cluster.
func TestLMStudioDefaultPort(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := parseConfig([]string{"--backend", "lmstudio"}, &stderr)
	require.NoError(t, err, "parse config")
	assert.Equal(t, 1234, effectivePort(cfg), "LM Studio default port")
}

func TestLlamaCPPDefaultPort(t *testing.T) {
	var stderr bytes.Buffer
	cfg, err := parseConfig([]string{"--backend", "llamacpp"}, &stderr)
	require.NoError(t, err, "parse config")
	require.Equal(t, 8080, effectivePort(cfg))
}

func TestResponseTextNeverReachesStdout(t *testing.T) {
	const secret = "the capital of France is Paris"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, err := w.Write([]byte(`{"models":[{"name":"available"}]}`))
			assert.NoError(t, err, "write engine response")
		case "/api/generate":
			_, err := w.Write([]byte(`{"response":"` + secret + `"}`))
			assert.NoError(t, err, "write engine response")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exit := runAgainstServer(t, context.Background(), nil, server.URL, &stdout, &stderr)
	assert.Equal(t, 0, exit, "stderr: %s", stderr.String())
	combined := stdout.String() + stderr.String()
	assert.NotContains(t, combined, "Paris", "response text reached the console")
	assert.NotContains(t, combined, secret, "response text reached the console")
	assert.Contains(t, stdout.String(), "response=sha256:", "missing response digest")
	assert.Contains(t, stdout.String(), fmt.Sprintf("bytes=%d", len(secret)), "missing response byte count")
}

// A non-2xx body from an OpenAI-compatible endpoint can echo the request back,
// and result.Error reaches both the JSONL result log and the debug error log.
func TestUpstreamErrorBodyNeverReachesLogs(t *testing.T) {
	const echoed = "invalid request: summarize the quarterly revenue memo"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, err := w.Write([]byte(`{"models":[{"name":"available"}]}`))
			assert.NoError(t, err, "write engine response")
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
	require.NoError(t, err, "read result log")
	errors, err := os.ReadFile(errorLog)
	require.NoError(t, err, "read error log")
	for name, content := range map[string]string{
		"stdout":     stdout.String(),
		"stderr":     stderr.String(),
		"result log": string(results),
		"error log":  string(errors),
	} {
		assert.NotContains(t, content, "quarterly", "%s leaked the upstream error body", name)
		assert.NotContains(t, content, echoed, "%s leaked the upstream error body", name)
		if name == "stdout" {
			continue
		}
		assert.Contains(t, content, "400 Bad Request", "%s dropped the HTTP status", name)
	}
}

func TestPromptDigestDoesNotLeakPromptText(t *testing.T) {
	const prompt = "classify this support request into one category"
	digest := promptDigest(prompt)
	assert.NotContains(t, digest, "classify", "digest leaked prompt text")
	assert.NotContains(t, digest, "support", "digest leaked prompt text")
	assert.True(t, strings.HasPrefix(digest, "sha256:"), "digest")
	assert.NotEqual(t, promptDigest(prompt+" more"), digest, "digest did not distinguish different prompts")
	assert.Equal(t, promptDigest(prompt), digest, "digest is not stable for the same prompt")
}
