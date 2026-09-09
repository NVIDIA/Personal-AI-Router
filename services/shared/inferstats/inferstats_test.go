// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package inferstats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func headers(contentType, encoding string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", contentType)
	if encoding != "" {
		h.Set("Content-Encoding", encoding)
	}
	return h
}

// feed arms a tap and observes each chunk in order, sleeping gap between
// chunks so first-to-last timing is measurable.
func feed(t *testing.T, h http.Header, gap time.Duration, chunks ...string) *Stats {
	t.Helper()
	tap := NewTap(time.Now())
	tap.Arm(h)
	for i, c := range chunks {
		if i > 0 && gap > 0 {
			time.Sleep(gap)
		}
		tap.Observe([]byte(c))
	}
	return tap.Finish()
}

func TestFinish_NilBeforeArmOrBody(t *testing.T) {
	tap := NewTap(time.Now())
	tap.Observe([]byte("ignored: not armed"))
	if got := tap.Finish(); got != nil {
		t.Fatalf("unarmed tap produced stats %+v", got)
	}
	tap = NewTap(time.Now())
	tap.Arm(headers("application/json", ""))
	if got := tap.Finish(); got != nil {
		t.Fatalf("armed tap with no body produced stats %+v", got)
	}
}

func TestFinish_OpenAINonStreaming(t *testing.T) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46,"completion_tokens_details":{"reasoning_tokens":5}},"prompt_logprobs":null}`
	s := feed(t, headers("application/json", ""), 0, body)
	if s == nil {
		t.Fatal("no stats")
	}
	if s.PromptTokens != 12 || s.CompletionTokens != 34 || s.Estimated {
		t.Fatalf("got %+v, want prompt=12 completion=34 estimated=false", s)
	}
	if s.TokensPerSecond <= 0 {
		t.Fatalf("tokensPerSecond not derived for a non-streaming reply: %+v", s)
	}
}

func TestFinish_OpenAIStreamWithUsageChunk(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n",
		"data: [DONE]\n\n",
	}
	s := feed(t, headers("text/event-stream", ""), 2*time.Millisecond, chunks...)
	if s == nil {
		t.Fatal("no stats")
	}
	if s.PromptTokens != 7 || s.CompletionTokens != 2 || s.Estimated {
		t.Fatalf("got %+v, want engine-reported prompt=7 completion=2", s)
	}
	if s.TokensPerSecond <= 0 {
		t.Fatalf("tokensPerSecond not derived for a stream: %+v", s)
	}
}

func TestFinish_OpenAIStreamWithoutUsage_Estimates(t *testing.T) {
	var chunks []string
	for i := 0; i < 10; i++ {
		chunks = append(chunks, fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i))
	}
	chunks = append(chunks, "data: [DONE]\n\n")
	s := feed(t, headers("text/event-stream", ""), time.Millisecond, chunks...)
	if s == nil {
		t.Fatal("no stats")
	}
	if !s.Estimated || s.CompletionTokens != 10 {
		t.Fatalf("got %+v, want estimated completion=10 ([DONE] discounted)", s)
	}
	if s.PromptTokens != 0 {
		t.Fatalf("prompt tokens invented without a usage object: %+v", s)
	}
}

// A `data:` line split across two body writes at the newline boundary must
// still count once — the common coalescing pattern is a write ending in "\n"
// followed by one starting with "data:".
func TestObserve_SSEEventSplitAtLineStart(t *testing.T) {
	s := feed(t, headers("text/event-stream", ""), 0,
		"data: {\"a\":1}\n\n", "data: {\"b\":2}\n", "\ndata: {\"c\":3}\n\n", "data: [DONE]\n\n")
	if s == nil || s.CompletionTokens != 3 || !s.Estimated {
		t.Fatalf("got %+v, want estimated completion=3", s)
	}
}

func TestFinish_OllamaNativeStream(t *testing.T) {
	chunks := []string{
		`{"model":"llama3","message":{"role":"assistant","content":"Hi"},"done":false}` + "\n",
		`{"model":"llama3","message":{"role":"assistant","content":" there"},"done":false}` + "\n",
		`{"model":"llama3","message":{"role":"assistant","content":""},"done_reason":"stop","done":true,` +
			`"total_duration":1500000000,"load_duration":100000000,"prompt_eval_count":9,"prompt_eval_duration":200000000,` +
			`"eval_count":40,"eval_duration":500000000}` + "\n",
	}
	s := feed(t, headers("application/x-ndjson", ""), 0, chunks...)
	if s == nil {
		t.Fatal("no stats")
	}
	if s.PromptTokens != 9 || s.CompletionTokens != 40 || s.Estimated {
		t.Fatalf("got %+v, want prompt=9 completion=40 from the terminal line", s)
	}
	// 40 tokens over the engine's own 0.5s eval_duration.
	if s.TokensPerSecond != 80 {
		t.Fatalf("tokensPerSecond = %v, want 80 (eval_count/eval_duration)", s.TokensPerSecond)
	}
}

// The usage report is always at the end, so a long non-streaming completion
// whose head has scrolled out of the tail still yields exact counts.
func TestFinish_LongBodyBeyondTail(t *testing.T) {
	content := strings.Repeat("lorem ipsum ", 2000) // ~24 KiB, well past tailSize
	body := `{"choices":[{"message":{"content":"` + content + `"}}],"usage":{"prompt_tokens":3,"completion_tokens":5000,"total_tokens":5003}}`
	s := feed(t, headers("application/json", ""), 0, body)
	if s == nil || s.CompletionTokens != 5000 || s.PromptTokens != 3 {
		t.Fatalf("got %+v, want completion=5000 prompt=3", s)
	}
}

// Generated text that happens to contain a usage-looking key must not shadow
// the engine's real report, which always comes later in the body.
func TestFinish_ContentMentioningUsageKeyIsIgnored(t *testing.T) {
	body := `{"choices":[{"message":{"content":"the API returns \"completion_tokens\":999 in usage"}}],"usage":{"prompt_tokens":1,"completion_tokens":20,"total_tokens":21}}`
	s := feed(t, headers("application/json", ""), 0, body)
	if s == nil || s.CompletionTokens != 20 {
		t.Fatalf("got %+v, want completion=20 from the trailing usage object", s)
	}
}

func TestFinish_CompressedBodyReportsTimingOnly(t *testing.T) {
	s := feed(t, headers("text/event-stream", "gzip"), 0, "\x1f\x8b\x08 not really gzip data: data: data:")
	if s == nil {
		t.Fatal("no stats for a compressed body; timing should still be reported")
	}
	if s.CompletionTokens != 0 || s.PromptTokens != 0 || s.TokensPerSecond != 0 || s.Estimated {
		t.Fatalf("token fields derived from compressed bytes: %+v", s)
	}
}

func TestFinish_TTFTMeasuredFromRequestStart(t *testing.T) {
	tap := NewTap(time.Now().Add(-250 * time.Millisecond))
	tap.Arm(headers("application/json", ""))
	tap.Observe([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	s := tap.Finish()
	if s == nil || s.TTFTMs < 250 {
		t.Fatalf("ttftMs = %v, want >= 250 (first body byte relative to request start)", s)
	}
}

func TestStats_JSONOmitsUnmeasuredFields(t *testing.T) {
	b, err := json.Marshal(&Stats{TTFTMs: 12})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"ttftMs":12}` {
		t.Fatalf("got %s, want only the measured field", b)
	}
	b, _ = json.Marshal(&Stats{CompletionTokens: 3, TokensPerSecond: 1.5, Estimated: true})
	if !bytes.Contains(b, []byte(`"estimated":true`)) || bytes.Contains(b, []byte(`"promptTokens"`)) {
		t.Fatalf("got %s", b)
	}
}

func TestRetain_KeepsExactlyTheTail(t *testing.T) {
	tap := NewTap(time.Now())
	tap.Arm(headers("application/json", ""))
	// Many small writes, then one oversized write; the tail must end with the
	// most recent bytes in both regimes and never exceed tailSize.
	for i := 0; i < 100; i++ {
		tap.Observe(bytes.Repeat([]byte{'a'}, 100))
	}
	if len(tap.tail) != tailSize {
		t.Fatalf("tail len %d after overflow, want %d", len(tap.tail), tailSize)
	}
	big := append(bytes.Repeat([]byte{'b'}, tailSize*2), []byte("END")...)
	tap.Observe(big)
	if len(tap.tail) != tailSize || !bytes.HasSuffix(tap.tail, []byte("END")) || tap.tail[0] != 'b' {
		t.Fatalf("oversized write not retained as its last %d bytes", tailSize)
	}
}

func TestLastNumber(t *testing.T) {
	cases := []struct {
		in, key string
		want    int64
		ok      bool
	}{
		{`{"completion_tokens":34}`, `"completion_tokens":`, 34, true},
		{`{"completion_tokens": 34}`, `"completion_tokens":`, 34, true},
		{`{"completion_tokens_details":{"x":1}}`, `"completion_tokens":`, 0, false},
		{`{"prompt_eval_count":9}`, `"eval_count":`, 0, false},
		{`{"eval_count":}`, `"eval_count":`, 0, false},
		{`{"a":1}{"a":2}`, `"a":`, 2, true},
	}
	for _, c := range cases {
		got, ok := lastNumber([]byte(c.in), c.key)
		if got != c.want || ok != c.ok {
			t.Errorf("lastNumber(%q, %q) = %d,%v want %d,%v", c.in, c.key, got, ok, c.want, c.ok)
		}
	}
}
