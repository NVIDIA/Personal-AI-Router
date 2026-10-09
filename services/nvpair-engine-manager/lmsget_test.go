// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLMSGetCandidates(t *testing.T) {
	test := func(name, input string, want []string) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, lmsGetCandidates(input))
		})
	}
	test("bare owner/name falls back to Hugging Face", "lmstudio-community/Qwen3.5-397B-A17B-MLX-4bit", []string{
		"lmstudio-community/Qwen3.5-397B-A17B-MLX-4bit",
		"https://huggingface.co/lmstudio-community/Qwen3.5-397B-A17B-MLX-4bit",
	})
	test("quant qualifier is preserved", "owner/name@q4_k_m", []string{"owner/name@q4_k_m", "https://huggingface.co/owner/name@q4_k_m"})
	test("explicit Hugging Face URL is honored first, then Hub", "https://huggingface.co/lmstudio-community/Foo-MLX-4bit", []string{
		"https://huggingface.co/lmstudio-community/Foo-MLX-4bit",
		"lmstudio-community/Foo-MLX-4bit",
	})
	test("lmstudio.ai /models URL keeps Hub first, then Hugging Face", "https://lmstudio.ai/models/qwen/qwen3.5-9b", []string{
		"https://lmstudio.ai/models/qwen/qwen3.5-9b",
		"https://huggingface.co/qwen/qwen3.5-9b",
	})
	test("lmstudio.ai URL without /models", "https://lmstudio.ai/qwen/qwen3.5-9b", []string{"https://lmstudio.ai/qwen/qwen3.5-9b", "https://huggingface.co/qwen/qwen3.5-9b"})
	test("search term passes through unchanged", "llama3.2", []string{"llama3.2"})
	test("unrecognized URL is honored verbatim with no fallback", "https://example.com/a/b", []string{"https://example.com/a/b"})
	test("blank yields no candidates", "   ", nil)
}

func TestIsLMSResolveFailure(t *testing.T) {
	const missingArtifact = `exit status 1: Error: Failed to resolve artifact "x/y": The artifact does not exist or you do not have permission to read it`
	test := func(name string, err error, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, isLMSResolveFailure(err))
		})
	}
	test("missing artifact", errors.New(missingArtifact), true)
	test("unsupported model", errors.New("this model is not supported in LM Studio"), true)
	test("no matching models", errors.New("no models found matching that term"), true)
	test("no error", nil, false)
	test("disk full", errors.New("exit status 1: write error: disk full"), false)
	test("network failure", errors.New("network connection failed"), false)
}

func TestIsLMSTransientDownloadError(t *testing.T) {
	const downloadTimeout = "exit status 1: Error: Download failed: Timed-out. Please try to resume. - You can try to resume the download within LM Studio."
	test := func(name string, err error, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, isLMSTransientDownloadError(err))
		})
	}
	test("download timeout", errors.New(downloadTimeout), true)
	test("connection reset", errors.New("exit status 1: read ECONNRESET"), true)
	test("socket hang up", errors.New("exit status 1: socket hang up"), true)
	test("fetch failed", errors.New("exit status 1: fetch failed"), true)
	test("no error", nil, false)
	test("disk full", errors.New("exit status 1: write error: disk full"), false)
	// A resolution failure is permanent for this source (handled by the
	// candidate loop), so it must NOT be treated as a transient download.
	test("permanent resolution failure", errors.New(`exit status 1: Failed to resolve artifact "x/y": the artifact does not exist`), false)
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
	require.NoError(t, err, "expected Hugging Face fallback to succeed")
	require.Contains(t, string(res), "https://huggingface.co/lmstudio-community/Foo-MLX-4bit", "expected the Hugging Face candidate to win (%v)", res)
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
	require.ErrorContains(t, err, "action command failed", "expected an error when all candidates fail to resolve")
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

	_, err := ex.Action(context.Background(), "fake", "pull_model",
		json.RawMessage(`{"model":"owner/name"}`))
	require.NoError(t, err, "expected resume to succeed after transient failures")
	require.Equal(t, 3, readCount(t, counter), "expected 3 in-place attempts (2 fail + 1 success)")
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
	require.ErrorContains(t, err, "Timed-out", "expected the LM Studio timeout to surface when the download never recovers")
	require.Equal(t, 3, readCount(t, counter), "expected exactly 3 attempts (resume budget, no source fallthrough)")
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
	require.NoError(t, err, "read counter")
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	require.NoError(t, err, "parse counter (%v, %v)", b, err)
	return n
}

func TestModelResolutionValidation(t *testing.T) {
	test := func(name string, action Action, wantErr bool) {
		t.Run(name, func(t *testing.T) {
			err := action.validate("pull_model")
			if wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	test("valid lms-get", Action{Cmd: []string{"{cli}", "get", "{model}", "--yes"}, ModelResolution: "lms-get"}, false)
	test("unknown strategy", Action{Cmd: []string{"{cli}", "get", "{model}"}, ModelResolution: "bogus"}, true)
	test("http cannot resolve models", Action{HTTP: &ActionHTTP{Method: "POST", Path: "/x"}, ModelResolution: "lms-get"}, true)
	test("cmd must template {model}", Action{Cmd: []string{"{cli}", "ls"}, ModelResolution: "lms-get"}, true)
	test("plain cmd still valid", Action{Cmd: []string{"{cli}", "ls"}}, false)
}
