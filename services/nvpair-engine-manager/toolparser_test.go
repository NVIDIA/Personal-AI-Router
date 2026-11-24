// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withToolParserProbe replaces the probe of the installed vLLM, and empties the
// cache of earlier results, for one test.
func withToolParserProbe(t *testing.T, probe func(bin, snapshot string) (toolParserProbe, error)) {
	t.Helper()
	old := probeToolParsers
	probeToolParsers = func(_ context.Context, bin, snapshot, _ string) (toolParserProbe, error) {
		return probe(bin, snapshot)
	}
	toolParserCache.Clear()
	t.Cleanup(func() {
		probeToolParsers = old
		toolParserCache.Clear()
	})
}

func TestChooseToolParser_PrefersGeneralPurposeParsersAmongEqualMatches(t *testing.T) {
	test := func(name string, probe toolParserProbe, want string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, chooseToolParser(probe))
		})
	}
	test("Qwen JSON in tool_call tags", toolParserProbe{Matches: []string{"ernie45", "granite4", "hermes", "qwen3_json"}}, "hermes")
	test("Qwen XML function tags", toolParserProbe{Matches: []string{"qwen3_coder", "qwen3_xml", "seed_oss", "step3p5"}}, "qwen3_coder")
	test("only uncommon parsers", toolParserProbe{Matches: []string{"step3p5", "seed_oss"}}, "seed_oss")
	test("response template wins", toolParserProbe{HF: true, Matches: []string{"hermes"}}, "hf")
	test("nothing matched", toolParserProbe{}, "")
}

func TestDetectToolParser_ProbesEachModelOnce(t *testing.T) {
	probes := 0
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		probes++
		return toolParserProbe{Matches: []string{"hermes"}}, nil
	})

	for range 3 {
		parser, _, err := detectToolParser(t.Context(), "vllm", "/hub/a", "")
		require.NoError(t, err)
		require.Equal(t, "hermes", parser)
	}
	require.Equal(t, 1, probes, "the same model is probed once")
	_, _, err := detectToolParser(t.Context(), "vllm", "/hub/c", "")
	require.NoError(t, err)
	assert.Equal(t, 2, probes, "another model must be probed on its own")
}

// Editing a --chat-template file and restarting must probe the new template,
// not reuse what the old one matched.
func TestDetectToolParser_ProbesAgainWhenTheTemplateFileChanges(t *testing.T) {
	probes := 0
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		probes++
		return toolParserProbe{Matches: []string{"hermes"}}, nil
	})
	template := filepath.Join(t.TempDir(), "chat.jinja")
	require.NoError(t, os.WriteFile(template, []byte("{{ messages }}"), 0o644))

	for range 2 {
		_, _, err := detectToolParser(t.Context(), "vllm", "/hub/a", template)
		require.NoError(t, err)
	}
	require.Equal(t, 1, probes, "an unchanged template is probed once")

	require.NoError(t, os.WriteFile(template, []byte("{{ messages }}{{ tools }}"), 0o644))
	_, _, err := detectToolParser(t.Context(), "vllm", "/hub/a", template)
	require.NoError(t, err)
	assert.Equal(t, 2, probes, "the edited template is probed again")
}

func TestDetectToolParser_RetriesAFailedProbe(t *testing.T) {
	probes := 0
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		probes++
		if probes == 1 {
			return toolParserProbe{}, errors.New("timed out")
		}
		return toolParserProbe{Matches: []string{"hermes"}}, nil
	})

	_, _, err := detectToolParser(t.Context(), "vllm", "/hub/a", "")
	require.Error(t, err, "the first probe's failure was not reported")

	parser, _, err := detectToolParser(t.Context(), "vllm", "/hub/a", "")
	require.NoError(t, err, "the next start must probe again")
	assert.Equal(t, "hermes", parser)
}

// The probe run against the real vLLM install and the models in the local
// Hugging Face cache. NVPAIR_LIVE_VLLM_BIN names the vllm binary, and
// NVPAIR_LIVE_VLLM_TOOL_MODELS lists model=parser pairs to check, with an empty
// parser for a model no parser reads, for example
// "Qwen/Qwen3-8B=hermes,nvidia/NVIDIA-Nemotron-Nano-9B-v2=".
func TestToolParserProbe_FindsTheParserWithTheInstalledVLLM(t *testing.T) {
	bin := os.Getenv("NVPAIR_LIVE_VLLM_BIN")
	if bin == "" {
		t.Skip("set NVPAIR_LIVE_VLLM_BIN and NVPAIR_LIVE_VLLM_TOOL_MODELS to probe real models with the installed vLLM")
	}
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	hub := filepath.Join(home, ".cache", "huggingface", "hub")
	for _, pair := range strings.Split(os.Getenv("NVPAIR_LIVE_VLLM_TOOL_MODELS"), ",") {
		model, want, _ := strings.Cut(pair, "=")
		t.Run(model, func(t *testing.T) {
			found, ok := findLaunchModel([]string{"--model", model}, hub, nil)
			require.True(t, ok, "%s is not in %s", model, hub)

			probe, err := runToolParserProbe(t.Context(), bin, found.snapshot, "")

			require.NoError(t, err)
			assert.Equal(t, want, chooseToolParser(probe), "chosen from %+v", probe)
		})
	}
}

// The probe runs under the start's context, so cancelling the start (Stop, a
// caller giving up, shutdown) stops the probe instead of leaving it to run out
// its own timeout.
func TestDetectToolParser_StopsTheProbeWhenTheStartIsCancelled(t *testing.T) {
	old := probeToolParsers
	t.Cleanup(func() {
		probeToolParsers = old
		toolParserCache.Clear()
	})
	toolParserCache.Clear()
	probeToolParsers = func(ctx context.Context, bin, snapshot, template string) (toolParserProbe, error) {
		select {
		case <-ctx.Done():
			return toolParserProbe{}, ctx.Err()
		case <-time.After(5 * time.Second):
			return toolParserProbe{}, errors.New("the probe ran on after its start was cancelled")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := detectToolParser(ctx, "vllm", "/hub/cancelled", "")

	require.ErrorIs(t, err, context.Canceled)
}
