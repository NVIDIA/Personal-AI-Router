// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// statsFacade builds a single-engine test proxy routing "llama" to upstream and
// returns its facade with the recorder its workload events land in.
func statsFacade(t *testing.T, engine, upstreamURL string) (*facade, *recRW) {
	t.Helper()
	profile, ok := profileFor(engine)
	if !ok {
		t.Fatalf("%s profile missing", engine)
	}
	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstreamURL, "llama"))
	p := newTestProxy(profile, NewCodec(rec), disc, profile.FacadePort)
	return p.soleFacade(), rec
}

// recordedWorkload decodes the workloadInfo of the last frame rec captured for
// method, failing the test when none was emitted.
func recordedWorkload(t *testing.T, rec *recRW, method string) Workload {
	t.Helper()
	rec.mu.Lock()
	raw := append([]byte(nil), rec.b...)
	rec.mu.Unlock()
	var found []byte
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if bytes.Contains(line, []byte(`"method":"`+method+`"`)) {
			found = line
		}
	}
	if found == nil {
		t.Fatalf("no %s frame recorded; frames:\n%s", method, raw)
	}
	var frame struct {
		Params struct {
			WorkloadInfo Workload `json:"workloadInfo"`
		} `json:"params"`
	}
	if err := json.Unmarshal(found, &frame); err != nil {
		t.Fatalf("decode %s frame: %v", method, err)
	}
	return frame.Params.WorkloadInfo
}

// streamChunks serves body chunks as a flushed stream with a small gap between
// them, the way an engine streams tokens.
func streamChunks(contentType string, chunks ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			io.WriteString(w, c)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
}

// TestHandleHTTP_CompletedCarriesStats: a streamed OpenAI-compatible response
// whose final chunk reports usage must surface engine-exact token counts and a
// throughput figure on workload:completed — and nothing on workload:started,
// which fires before a byte has flowed. Every facade shares the handler, so
// each engine is checked.
func TestHandleHTTP_CompletedCarriesStats(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio", "llamacpp"} {
		t.Run(engine, func(t *testing.T) {
			upstream := httptest.NewServer(streamChunks("text/event-stream",
				"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n",
				"data: [DONE]\n\n",
			))
			defer upstream.Close()
			f, rec := statsFacade(t, engine, upstream.URL)

			rr := httptest.NewRecorder()
			f.handleHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"llama","stream":true}`)))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "[DONE]") {
				t.Fatalf("client did not receive the full stream:\n%s", rr.Body.String())
			}

			if started := recordedWorkload(t, rec, "workload:started"); started.Stats != nil {
				t.Fatalf("workload:started carried stats %+v before any byte flowed", started.Stats)
			}
			done := recordedWorkload(t, rec, "workload:completed")
			if done.Stats == nil {
				t.Fatal("workload:completed carried no stats")
			}
			s := done.Stats
			if s.PromptTokens != 5 || s.CompletionTokens != 2 || s.Estimated {
				t.Fatalf("stats = %+v, want engine-reported prompt=5 completion=2", s)
			}
			if s.TokensPerSecond <= 0 {
				t.Fatalf("stats = %+v, want a positive tokensPerSecond", s)
			}
		})
	}
}

// TestHandleHTTP_OllamaNativeStreamStats: Ollama's own API reports counts on
// the terminal "done":true line, so an /api/chat stream yields engine-exact
// counts with throughput taken from the engine's eval_duration.
func TestHandleHTTP_OllamaNativeStreamStats(t *testing.T) {
	upstream := httptest.NewServer(streamChunks("application/x-ndjson",
		`{"model":"llama","message":{"role":"assistant","content":"Hi"},"done":false}`+"\n",
		`{"model":"llama","message":{"role":"assistant","content":" there"},"done":false}`+"\n",
		`{"model":"llama","message":{"role":"assistant","content":""},"done_reason":"stop","done":true,"total_duration":900000000,"prompt_eval_count":9,"eval_count":40,"eval_duration":500000000}`+"\n",
	))
	defer upstream.Close()
	f, rec := statsFacade(t, "ollama", upstream.URL)

	f.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"llama"}`)))

	s := recordedWorkload(t, rec, "workload:completed").Stats
	if s == nil || s.Estimated || s.PromptTokens != 9 || s.CompletionTokens != 40 || s.TokensPerSecond != 80 {
		t.Fatalf("stats = %+v, want prompt=9 completion=40 tokensPerSecond=80 from the done line", s)
	}
}

// TestHandleHTTP_AnthropicMessagesStats: the Anthropic Messages route reports
// usage as input_tokens / output_tokens on its final message_delta, and its
// stream carries non-token events (message_start, ping, content_block_*), so a
// chunk count would overstate it. Both facades that serve /v1/messages are
// checked.
func TestHandleHTTP_AnthropicMessagesStats(t *testing.T) {
	for _, engine := range []string{"ollama", "lmstudio"} {
		t.Run(engine, func(t *testing.T) {
			upstream := httptest.NewServer(streamChunks("text/event-stream",
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n",
				"event: ping\ndata: {\"type\":\"ping\"}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hi\"}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			))
			defer upstream.Close()
			f, rec := statsFacade(t, engine, upstream.URL)

			f.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"llama","stream":true}`)))

			s := recordedWorkload(t, rec, "workload:completed").Stats
			if s == nil || s.Estimated || s.CompletionTokens != 15 || s.PromptTokens != 25 {
				t.Fatalf("stats = %+v, want engine-reported completion=15 prompt=25", s)
			}
		})
	}
}

// TestHandleHTTP_StreamWithoutUsageEstimates: an engine that streams without a
// usage report still yields a token count, counted from chunks and flagged as
// an estimate.
func TestHandleHTTP_StreamWithoutUsageEstimates(t *testing.T) {
	upstream := httptest.NewServer(streamChunks("text/event-stream",
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"c\"}}]}\n\n",
		"data: [DONE]\n\n",
	))
	defer upstream.Close()
	f, rec := statsFacade(t, "lmstudio", upstream.URL)

	f.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","stream":true}`)))

	s := recordedWorkload(t, rec, "workload:completed").Stats
	if s == nil || !s.Estimated || s.CompletionTokens != 3 || s.PromptTokens != 0 {
		t.Fatalf("stats = %+v, want estimated completion=3 and no prompt count", s)
	}
}

// TestHandleHTTP_ErrorBodyNotMeasured: an upstream error body is never mined for
// tokens, even when it happens to contain usage-looking fields.
func TestHandleHTTP_ErrorBodyNotMeasured(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"boom","usage":{"prompt_tokens":1,"completion_tokens":99}}`)
	}))
	defer upstream.Close()
	f, rec := statsFacade(t, "ollama", upstream.URL)

	f.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama"}`)))

	if failed := recordedWorkload(t, rec, "workload:errored"); failed.Stats != nil {
		t.Fatalf("workload:errored carried stats %+v from an error body", failed.Stats)
	}
}
