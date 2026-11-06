// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"
)

// toolParserProbeScript asks the installed vLLM which of its tool-call parsers
// read a model's tool-call format. See the script for how.
//
//go:embed toolparser_probe.py
var toolParserProbeScript string

// toolParserProbeTimeout bounds one probe. It imports transformers and vLLM's
// parsers, which takes a few seconds once their files are in the page cache.
const toolParserProbeTimeout = 2 * time.Minute

// toolParserProbe is what the probe found for one model.
type toolParserProbe struct {
	// HF is true when the tokenizer ships response_template metadata, which
	// vLLM's hf parser reads.
	HF bool `json:"hf"`
	// Matches are the parsers that extracted the model's example tool call.
	Matches []string `json:"matches"`
}

// probeToolParsers runs the probe with the Python next to the vLLM binary. It
// is a variable so tests can supply results without a vLLM install.
var probeToolParsers = runToolParserProbe

// toolParserCache keeps each probe's result for the life of the service, keyed
// by vLLM binary and snapshot directory, so a model is probed once rather than
// on every start, wake and switch. A different vLLM or model revision is a
// different key.
var toolParserCache sync.Map

// preferredToolParsers breaks ties when several parsers read a format equally
// well: Qwen's <tool_call> JSON is read alike by hermes, ernie45, granite4 and
// qwen3_json, and its XML form by qwen3_coder, qwen3_xml, seed_oss and step3p5.
// These are vLLM's general-purpose parsers for the common formats, tried in
// this order; any other match is taken in name order.
var preferredToolParsers = []string{"hermes", "qwen3_coder", "llama3_json", "mistral", "pythonic"}

// chooseToolParser picks the parser to launch with, or "" when none fits.
func chooseToolParser(p toolParserProbe) string {
	if p.HF {
		return "hf"
	}
	for _, name := range preferredToolParsers {
		if slices.Contains(p.Matches, name) {
			return name
		}
	}
	if len(p.Matches) == 0 {
		return ""
	}
	return slices.Min(p.Matches)
}

// vllmPython is the interpreter of the environment vLLM is installed in.
func vllmPython(bin string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	name := "python"
	if runtime.GOOS == "windows" {
		name = "python.exe"
	}
	python := filepath.Join(filepath.Dir(bin), name)
	if _, err := os.Stat(python); err != nil {
		return "", fmt.Errorf("no Python next to %s", bin)
	}
	return python, nil
}

func runToolParserProbe(bin, snapshot string) (toolParserProbe, error) {
	python, err := vllmPython(bin)
	if err != nil {
		return toolParserProbe{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolParserProbeTimeout)
	defer cancel()
	// -I keeps the working directory and user site off the import path.
	cmd := exec.CommandContext(ctx, python, "-I", "-c", toolParserProbeScript, snapshot)
	cmd.Dir = os.TempDir()
	out, err := cmd.Output()
	if err != nil {
		return toolParserProbe{}, fmt.Errorf("tool-call parser probe: %w", err)
	}
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	var probe toolParserProbe
	if err := json.Unmarshal(lines[len(lines)-1], &probe); err != nil {
		return toolParserProbe{}, errors.New("tool-call parser probe printed no result")
	}
	return probe, nil
}

// detectToolParser returns the parser for the model in snapshot, probing it the
// first time. A failed probe is not cached, so the next start tries again.
func detectToolParser(bin, snapshot string) (string, []string, error) {
	key := bin + "\x00" + snapshot
	if cached, ok := toolParserCache.Load(key); ok {
		probe := cached.(toolParserProbe)
		return chooseToolParser(probe), probe.Matches, nil
	}
	probe, err := probeToolParsers(bin, snapshot)
	if err != nil {
		return "", nil, err
	}
	toolParserCache.Store(key, probe)
	return chooseToolParser(probe), probe.Matches, nil
}
