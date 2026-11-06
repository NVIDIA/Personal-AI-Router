// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withToolParserProbe replaces the probe of the installed vLLM, and empties the
// cache of earlier results, for one test.
func withToolParserProbe(t *testing.T, probe func(bin, snapshot string) (toolParserProbe, error)) {
	t.Helper()
	old := probeToolParsers
	probeToolParsers = probe
	toolParserCache.Clear()
	t.Cleanup(func() {
		probeToolParsers = old
		toolParserCache.Clear()
	})
}

func TestChooseToolParser_PrefersGeneralPurposeParsersAmongEqualMatches(t *testing.T) {
	cases := []struct {
		name  string
		probe toolParserProbe
		want  string
	}{
		{"Qwen JSON in tool_call tags", toolParserProbe{Matches: []string{"ernie45", "granite4", "hermes", "qwen3_json"}}, "hermes"},
		{"Qwen XML function tags", toolParserProbe{Matches: []string{"qwen3_coder", "qwen3_xml", "seed_oss", "step3p5"}}, "qwen3_coder"},
		{"only uncommon parsers", toolParserProbe{Matches: []string{"step3p5", "seed_oss"}}, "seed_oss"},
		{"response template wins", toolParserProbe{HF: true, Matches: []string{"hermes"}}, "hf"},
		{"nothing matched", toolParserProbe{}, ""},
	}
	for _, c := range cases {
		if got := chooseToolParser(c.probe); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDetectToolParser_ProbesEachModelOnce(t *testing.T) {
	probes := 0
	withToolParserProbe(t, func(bin, snapshot string) (toolParserProbe, error) {
		probes++
		return toolParserProbe{Matches: []string{"hermes"}}, nil
	})

	for range 3 {
		if parser, _, err := detectToolParser("vllm", "/hub/a"); err != nil || parser != "hermes" {
			t.Fatalf("got %q, %v", parser, err)
		}
	}
	if probes != 1 {
		t.Fatalf("probed %d times, want once for the same model", probes)
	}
	if _, _, err := detectToolParser("vllm", "/hub/c"); err != nil || probes != 2 {
		t.Fatalf("another model must be probed on its own: %d probes, %v", probes, err)
	}
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

	if _, _, err := detectToolParser("vllm", "/hub/a"); err == nil {
		t.Fatal("the first probe's failure was not reported")
	}
	if parser, _, err := detectToolParser("vllm", "/hub/a"); err != nil || parser != "hermes" {
		t.Fatalf("the next start must probe again: %q, %v", parser, err)
	}
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
	home, _ := os.UserHomeDir()
	hub := filepath.Join(home, ".cache", "huggingface", "hub")
	for _, pair := range strings.Split(os.Getenv("NVPAIR_LIVE_VLLM_TOOL_MODELS"), ",") {
		model, want, _ := strings.Cut(pair, "=")
		snapshot, _, _, ok := findHFSnapshot(hub, model)
		if !ok {
			t.Errorf("%s is not in %s", model, hub)
			continue
		}
		probe, err := runToolParserProbe(bin, snapshot)
		if err != nil {
			t.Errorf("%s: %v", model, err)
			continue
		}
		if got := chooseToolParser(probe); got != want {
			t.Errorf("%s: chose %q from %+v, want %q", model, got, probe, want)
		}
	}
}
