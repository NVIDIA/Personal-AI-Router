// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package inferstats derives per-request inference statistics — token counts,
// decode throughput, and time to first token — from an inference response as
// the proxy streams it to the client. Both inference proxies front engines that
// speak the OpenAI-compatible API (and Ollama's native one), so keeping one
// implementation is what stops the two from drifting apart.
//
// The design goal is negligible overhead on the hot path. A Tap never buffers
// or parses the whole body: per body write it records a timestamp, counts
// stream events, and keeps only the last few kilobytes. Every engine puts its
// usage report at the END of the response — the OpenAI `usage` object trails
// `choices` in a non-streaming reply and rides the final chunk of a stream
// (`stream_options.include_usage`), and Ollama's native `eval_count` /
// `eval_duration` sit on the terminal `"done":true` line — so that tail is all
// Finish needs. Token counts are pulled from the tail with a byte search, not a
// JSON decode, so a truncated leading object (a long non-streaming completion)
// is not a problem.
//
// Prompts, messages, and response text are never retained beyond the rolling
// tail, which is discarded at Finish; only numbers leave this package.
package inferstats

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stats is the additive `stats` object carried on a workload's terminal event.
// Every field is optional: a reader must treat an absent field as "not
// measured", never as zero. A field the engine did not report is omitted rather
// than invented.
type Stats struct {
	// PromptTokens is the prompt size as reported by the engine.
	PromptTokens int64 `json:"promptTokens,omitempty"`
	// CompletionTokens is the number of tokens generated. Reported by the
	// engine when its response carried a usage object; otherwise counted from
	// stream chunks, in which case Estimated is set.
	CompletionTokens int64 `json:"completionTokens,omitempty"`
	// TokensPerSecond is decode throughput: CompletionTokens over generation
	// time. Generation time is the engine's own measurement when it reports one
	// (Ollama's eval_duration), else first-to-last body byte for a stream, else
	// the whole request for a non-streaming reply. Rounded to one decimal.
	TokensPerSecond float64 `json:"tokensPerSecond,omitempty"`
	// TTFTMs is time to first token: request start to the first response body
	// byte. Headers are deliberately not the boundary — a streaming engine sends
	// them before prefill finishes.
	TTFTMs int64 `json:"ttftMs,omitempty"`
	// Estimated marks CompletionTokens (and so TokensPerSecond) as counted from
	// stream chunks rather than reported by the engine. A chunk usually carries
	// one token, but an engine may batch several, so treat the count as
	// approximate.
	Estimated bool `json:"estimated,omitempty"`
}

// tailSize bounds the bytes retained from the end of the body. An OpenAI usage
// chunk with its details objects plus the `[DONE]` sentinel is a few hundred
// bytes; Ollama's terminal line likewise. 4 KiB leaves ample slack for trailing
// engine-specific fields (vLLM's `prompt_logprobs`, LM Studio's `stats`).
const tailSize = 4096

// Tap observes one response body. It is safe for concurrent use: the reverse
// proxy's copy goroutine calls Observe while a disconnect watcher may call
// Finish. The mutex is uncontended in practice, so it costs a few nanoseconds
// per body write.
type Tap struct {
	mu    sync.Mutex
	start time.Time
	armed bool
	// opaque is set when the body is content-encoded (a client that asked for
	// gzip and an engine that obliged). Only timing is usable then; counting
	// or searching compressed bytes would be noise.
	opaque bool
	// sse is set for a text/event-stream body, where a stream event is a
	// `data:` line; otherwise (Ollama's NDJSON) an event is a line.
	sse         bool
	firstAt     time.Time
	lastAt      time.Time
	events      int64
	atLineStart bool
	tail        []byte
}

// NewTap returns a Tap for a request that began at start. It observes nothing
// until Arm is called.
func NewTap(start time.Time) *Tap {
	return &Tap{start: start, atLineStart: true}
}

// Arm starts observation once the proxy has committed to an upstream response,
// using the response headers to decide how to read the body. Call it only for a
// successful inference response — an error body carries no tokens.
func (t *Tap) Arm(h http.Header) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armed = true
	enc := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding")))
	t.opaque = enc != "" && enc != "identity"
	t.sse = strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
	if !t.opaque {
		t.tail = make([]byte, 0, tailSize)
	}
}

// Observe records one body write. It must be cheap: it runs on the reverse
// proxy's copy goroutine between the upstream read and the client write.
func (t *Tap) Observe(p []byte) {
	if len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.armed {
		return
	}
	now := time.Now()
	if t.firstAt.IsZero() {
		t.firstAt = now
	}
	t.lastAt = now
	if t.opaque {
		return
	}
	if t.sse {
		if t.atLineStart && bytes.HasPrefix(p, []byte("data:")) {
			t.events++
		}
		t.events += int64(bytes.Count(p, []byte("\ndata:")))
	} else {
		t.events += int64(bytes.Count(p, []byte{'\n'}))
	}
	t.atLineStart = p[len(p)-1] == '\n'
	t.retain(p)
}

// retain appends p to the rolling tail, keeping only the last tailSize bytes.
// The buffer is fixed-capacity, so this never allocates after Arm.
func (t *Tap) retain(p []byte) {
	if len(p) >= tailSize {
		t.tail = t.tail[:tailSize]
		copy(t.tail, p[len(p)-tailSize:])
		return
	}
	if overflow := len(t.tail) + len(p) - tailSize; overflow > 0 {
		copy(t.tail, t.tail[overflow:])
		t.tail = t.tail[:len(t.tail)-overflow]
	}
	t.tail = append(t.tail, p...)
}

// Finish derives the statistics and releases the tail. It returns nil when the
// Tap was never armed or no body byte was observed, so a caller can attach the
// result directly to an omitempty field.
func (t *Tap) Finish() *Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.armed || t.firstAt.IsZero() {
		return nil
	}
	s := &Stats{TTFTMs: t.firstAt.Sub(t.start).Milliseconds()}
	tail := t.tail
	t.tail = nil

	var genDur time.Duration
	switch {
	case t.opaque:
		// Compressed body: nothing to count. Timing alone is still worth
		// reporting.
	default:
		if n, ok := lastNumber(tail, `"completion_tokens":`); ok {
			// OpenAI-compatible usage object (non-streaming reply, or the
			// final chunk when the client asked for stream_options.include_usage).
			s.CompletionTokens = n
			if p, ok := lastNumber(tail, `"prompt_tokens":`); ok {
				s.PromptTokens = p
			}
		} else if n, ok := lastNumber(tail, `"eval_count":`); ok {
			// Ollama native terminal line. eval_duration is the engine's own
			// decode-time measurement in nanoseconds — more precise than our
			// wall clock, so prefer it.
			s.CompletionTokens = n
			if p, ok := lastNumber(tail, `"prompt_eval_count":`); ok {
				s.PromptTokens = p
			}
			if d, ok := lastNumber(tail, `"eval_duration":`); ok && d > 0 {
				genDur = time.Duration(d)
			}
		} else if t.events > 0 {
			// No usage report: approximate from stream events, discounting the
			// SSE `[DONE]` sentinel, which carries no token.
			n := t.events
			if t.sse && bytes.Contains(tail, []byte("[DONE]")) {
				n--
			}
			if n > 0 {
				s.CompletionTokens = n
				s.Estimated = true
			}
		}
	}

	if s.CompletionTokens > 0 {
		if genDur == 0 {
			if t.events > 1 {
				// Streamed: the first byte marks the end of prefill, so
				// first-to-last is decode time.
				genDur = t.lastAt.Sub(t.firstAt)
			} else {
				// Non-streaming: the whole reply arrived at once, so the only
				// honest denominator is the full request.
				genDur = t.lastAt.Sub(t.start)
			}
		}
		if genDur > 0 {
			tps := float64(s.CompletionTokens) / genDur.Seconds()
			s.TokensPerSecond = math.Round(tps*10) / 10
		}
	}
	return s
}

// lastNumber finds the LAST occurrence of key (a quoted JSON key including its
// trailing colon, e.g. `"completion_tokens":`) and parses the non-negative
// integer that follows it. The last occurrence is the right one: every engine
// emits its usage report after the generated content, so a model that happens
// to write the same key in its output cannot shadow it. Including the closing
// quote and colon in the key keeps `"completion_tokens":` from matching inside
// `"completion_tokens_details":`, and the opening quote keeps `"eval_count":`
// from matching inside `"prompt_eval_count":`.
func lastNumber(b []byte, key string) (int64, bool) {
	i := bytes.LastIndex(b, []byte(key))
	if i < 0 {
		return 0, false
	}
	j := i + len(key)
	for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
		j++
	}
	k := j
	for k < len(b) && b[k] >= '0' && b[k] <= '9' {
		k++
	}
	if k == j || k-j > 18 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(b[j:k]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
