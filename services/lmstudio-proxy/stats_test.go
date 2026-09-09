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

// recordedWorkload decodes the workloadInfo of the last frame rec captured for
// method, failing the test when none was emitted.
func recordedWorkload(t *testing.T, rec *prRec, method string) Workload {
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

// TestHandleHTTP_CompletedCarriesStats: a streamed inference response whose
// final chunk reports usage must surface engine-exact token counts and a
// throughput figure on workload:completed — and nothing on workload:started,
// which fires before a byte has flowed.
func TestHandleHTTP_CompletedCarriesStats(t *testing.T) {
	upstream := httptest.NewServer(streamChunks("text/event-stream",
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n",
		"data: [DONE]\n\n",
	))
	defer upstream.Close()

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	rr := httptest.NewRecorder()
	p.handleHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
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

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	p.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
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

	rec := &prRec{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "node-a", upstream.URL, "llama"))
	p := NewProxy(NewCodec(rec), disc, 1235)

	p.handleHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama"}`)))

	if failed := recordedWorkload(t, rec, "workload:errored"); failed.Stats != nil {
		t.Fatalf("workload:errored carried stats %+v from an error body", failed.Stats)
	}
}
