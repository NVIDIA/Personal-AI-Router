// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLMSGetCandidates(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "bare owner/name falls back to Hugging Face",
			in:   "lmstudio-community/Qwen3.5-397B-A17B-MLX-4bit",
			want: []string{
				"lmstudio-community/Qwen3.5-397B-A17B-MLX-4bit",
				"https://huggingface.co/lmstudio-community/Qwen3.5-397B-A17B-MLX-4bit",
			},
		},
		{
			name: "quant qualifier is preserved",
			in:   "owner/name@q4_k_m",
			want: []string{
				"owner/name@q4_k_m",
				"https://huggingface.co/owner/name@q4_k_m",
			},
		},
		{
			name: "explicit Hugging Face URL is honored first, then Hub",
			in:   "https://huggingface.co/lmstudio-community/Foo-MLX-4bit",
			want: []string{
				"https://huggingface.co/lmstudio-community/Foo-MLX-4bit",
				"lmstudio-community/Foo-MLX-4bit",
			},
		},
		{
			name: "lmstudio.ai /models URL keeps Hub first, then Hugging Face",
			in:   "https://lmstudio.ai/models/qwen/qwen3.5-9b",
			want: []string{
				"https://lmstudio.ai/models/qwen/qwen3.5-9b",
				"https://huggingface.co/qwen/qwen3.5-9b",
			},
		},
		{
			name: "lmstudio.ai URL without /models",
			in:   "https://lmstudio.ai/qwen/qwen3.5-9b",
			want: []string{
				"https://lmstudio.ai/qwen/qwen3.5-9b",
				"https://huggingface.co/qwen/qwen3.5-9b",
			},
		},
		{
			name: "search term passes through unchanged",
			in:   "llama3.2",
			want: []string{"llama3.2"},
		},
		{
			name: "unrecognized URL is honored verbatim with no fallback",
			in:   "https://example.com/a/b",
			want: []string{"https://example.com/a/b"},
		},
		{
			name: "blank yields no candidates",
			in:   "   ",
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := lmsGetCandidates(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("lmsGetCandidates(%q)\n got: %#v\nwant: %#v", c.in, got, c.want)
			}
		})
	}
}

func TestIsLMSResolveFailure(t *testing.T) {
	yes := []string{
		`exit status 1: Error: Failed to resolve artifact "x/y": The artifact does not exist or you do not have permission to read it`,
		"this model is not supported in LM Studio",
		"no models found matching that term",
	}
	for _, s := range yes {
		if !isLMSResolveFailure(errors.New(s)) {
			t.Errorf("expected resolve failure for %q", s)
		}
	}
	no := []error{
		nil,
		errors.New("exit status 1: write error: disk full"),
		errors.New("network connection failed"),
	}
	for _, e := range no {
		if isLMSResolveFailure(e) {
			t.Errorf("did not expect resolve failure for %v", e)
		}
	}
}

func TestIsLMSTransientDownloadError(t *testing.T) {
	yes := []string{
		"exit status 1: Error: Download failed: Timed-out. Please try to resume. - You can try to resume the download within LM Studio.",
		"exit status 1: read ECONNRESET",
		"exit status 1: socket hang up",
		"exit status 1: fetch failed",
	}
	for _, s := range yes {
		if !isLMSTransientDownloadError(errors.New(s)) {
			t.Errorf("expected transient download error for %q", s)
		}
	}
	no := []error{
		nil,
		errors.New("exit status 1: write error: disk full"),
		// A resolution failure is permanent for this source (handled by the
		// candidate loop), so it must NOT be treated as a transient download.
		errors.New(`exit status 1: Failed to resolve artifact "x/y": the artifact does not exist`),
	}
	for _, e := range no {
		if isLMSTransientDownloadError(e) {
			t.Errorf("did not expect transient download error for %v", e)
		}
	}
}

// TestCmdActionLMSGetFallback drives a cmd action that opts into lms-get
// resolution against the fake engine's `resolvesim`, which fails for a
// bare Hub id and succeeds for a Hugging Face URL — proving the runner
// falls back from Hub to Hugging Face and returns the winning candidate.
func TestCmdActionLMSGetFallback(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["pull_model"] = Action{
		Cmd:             []string{fakeEngineBin, "resolvesim", "{model}"},
		ModelResolution: modelResolutionLMSGet,
	}
	ex := newTestExecutor(t, m)

	res, err := ex.Action(context.Background(), "fake", "pull_model",
		json.RawMessage(`{"model":"lmstudio-community/Foo-MLX-4bit"}`))
	if err != nil {
		t.Fatalf("expected Hugging Face fallback to succeed, got: %v", err)
	}
	if !strings.Contains(string(res), "https://huggingface.co/lmstudio-community/Foo-MLX-4bit") {
		t.Fatalf("expected the Hugging Face candidate to win, got: %s", res)
	}
}

// TestCmdActionLMSGetAllFail confirms that when every candidate fails to
// resolve, the action surfaces the (last) resolve error rather than
// silently succeeding.
func TestCmdActionLMSGetAllFail(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["pull_model"] = Action{
		Cmd:             []string{fakeEngineBin, "resolvesim", "{model}"},
		ModelResolution: modelResolutionLMSGet,
	}
	ex := newTestExecutor(t, m)

	// "nope" makes even the Hugging Face URL candidate fail in resolvesim.
	_, err := ex.Action(context.Background(), "fake", "pull_model",
		json.RawMessage(`{"model":"owner/nope"}`))
	if err == nil {
		t.Fatal("expected an error when all candidates fail to resolve")
	}
	if !strings.Contains(err.Error(), "action command failed") {
		t.Fatalf("unexpected error shape: %v", err)
	}
}

// TestCmdActionLMSGetResumesTransientDownload proves a transient `lms get`
// download failure (a stalled/timed-out transfer) is retried in place — the
// download resumes — rather than surfaced to the caller. The fake fails
// twice then succeeds; the action should succeed within the resume budget.
func TestCmdActionLMSGetResumesTransientDownload(t *testing.T) {
	defer setResumeBudget(t, 3, time.Millisecond)()

	counter := filepath.Join(t.TempDir(), "n")
	m := testEngineManifest(fakeEngineBin)
	m.Actions["pull_model"] = Action{
		Cmd:             []string{fakeEngineBin, "downloadsim", counter, "2", "{model}"},
		ModelResolution: modelResolutionLMSGet,
	}
	ex := newTestExecutor(t, m)

	if _, err := ex.Action(context.Background(), "fake", "pull_model",
		json.RawMessage(`{"model":"owner/name"}`)); err != nil {
		t.Fatalf("expected resume to succeed after transient failures, got: %v", err)
	}
	if got := readCount(t, counter); got != 3 {
		t.Fatalf("expected 3 in-place attempts (2 fail + 1 success), got %d", got)
	}
}

// TestCmdActionLMSGetResumeExhausted confirms that when the transient failure
// never clears, the action gives up after the resume budget and surfaces the
// error — and does NOT fall through to the next source (a download stall on
// the resolved artifact shouldn't switch sources).
func TestCmdActionLMSGetResumeExhausted(t *testing.T) {
	defer setResumeBudget(t, 3, time.Millisecond)()

	counter := filepath.Join(t.TempDir(), "n")
	m := testEngineManifest(fakeEngineBin)
	m.Actions["pull_model"] = Action{
		Cmd:             []string{fakeEngineBin, "downloadsim", counter, "99", "{model}"},
		ModelResolution: modelResolutionLMSGet,
	}
	ex := newTestExecutor(t, m)

	_, err := ex.Action(context.Background(), "fake", "pull_model",
		json.RawMessage(`{"model":"owner/name"}`))
	if err == nil {
		t.Fatal("expected an error when the download never recovers")
	}
	if !strings.Contains(err.Error(), "Timed-out") {
		t.Fatalf("expected the LM Studio timeout to surface, got: %v", err)
	}
	if got := readCount(t, counter); got != 3 {
		t.Fatalf("expected exactly 3 attempts (resume budget, no source fallthrough), got %d", got)
	}
}

func setResumeBudget(t *testing.T, attempts int, backoff time.Duration) func() {
	t.Helper()
	oa, ob := lmsGetResumeAttempts, lmsGetResumeBackoff
	lmsGetResumeAttempts, lmsGetResumeBackoff = attempts, backoff
	return func() { lmsGetResumeAttempts, lmsGetResumeBackoff = oa, ob }
}

func readCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse counter %q: %v", b, err)
	}
	return n
}

func TestModelResolutionValidation(t *testing.T) {
	cases := []struct {
		name    string
		action  Action
		wantErr bool
	}{
		{"valid lms-get", Action{Cmd: []string{"{cli}", "get", "{model}", "--yes"}, ModelResolution: "lms-get"}, false},
		{"unknown strategy", Action{Cmd: []string{"{cli}", "get", "{model}"}, ModelResolution: "bogus"}, true},
		{"http cannot resolve models", Action{HTTP: &ActionHTTP{Method: "POST", Path: "/x"}, ModelResolution: "lms-get"}, true},
		{"cmd must template {model}", Action{Cmd: []string{"{cli}", "ls"}, ModelResolution: "lms-get"}, true},
		{"plain cmd still valid", Action{Cmd: []string{"{cli}", "ls"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.action.validate("pull_model")
			if c.wantErr && err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}
