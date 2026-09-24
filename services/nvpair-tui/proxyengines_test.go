// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestDefaultProxyEngineCSVUsesSharedDefaults(t *testing.T) {
	if got, want := defaultProxyEngineCSV(), "ollama,lmstudio"; got != want {
		t.Fatalf("default proxy engines = %q, want %q", got, want)
	}
}

func TestParseProxyEnginesPreservesExplicitSelection(t *testing.T) {
	got, err := parseProxyEngines("llamacpp, ollama, llamacpp")
	if err != nil {
		t.Fatalf("parse explicit engines: %v", err)
	}
	if len(got) != 2 || got[0].Name != "llamacpp" || got[1].Name != "ollama" {
		t.Fatalf("parsed engines = %v, want [llamacpp ollama]", got)
	}
}

func TestParseProxyEnginesAllowsNoFacades(t *testing.T) {
	got, err := parseProxyEngines(" , ")
	if err != nil {
		t.Fatalf("parse empty selection: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("parsed engines = %v, want none", got)
	}
}

func TestParseProxyEnginesRejectsUnknownEngine(t *testing.T) {
	_, err := parseProxyEngines("llamacpp,vllm")
	if err == nil {
		t.Fatal("unknown engine was accepted")
	}
	if !strings.Contains(err.Error(), `unknown engine "vllm"`) {
		t.Fatalf("error = %q, want unknown-engine detail", err)
	}
}
