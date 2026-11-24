// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// {cli} names the engine's own command line tool, and a manifest can only write
// that path as a guess: where the tool ends up depends on how the engine reached
// the machine. MTPLX's manifest names ~/.mtplx/bin/mtplx, which is where the
// vendor's own installer puts it, while a Homebrew install puts it in Homebrew's
// bin directory and a downloaded app keeps it inside the app bundle. So the
// placeholder follows what the last detect actually found, and the manifest's
// literal is the fallback for a manifest that declares no detect paths and so has
// nothing to have found.
//
// These tests stand the tool up as a shell script that answers with the file that
// ran, so a failure names the path that was chosen rather than only saying that
// something went wrong.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeEngineCLI puts an executable stand-in engine tool at dir/name. It answers
// with the label it was given and the arguments it received, which is how the
// test below reads which command ran.
func writeEngineCLI(t *testing.T, dir, name, label string) string {
	t.Helper()
	skipWithoutShell(t)
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	body := "#!/bin/sh\nprintf '{\"ran\":\"" + label + "\",\"args\":\"%s\"}' \"$*\"\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

// cliActionManifest describes an engine whose only action is a CLI call, with the
// tool found at `detected` (empty for none) and the manifest naming `literal`.
func cliActionManifest(engine, detected, literal string) *Manifest {
	platform := Platform{
		Runtime: Runtime{CLI: literal},
	}
	if detected != "" {
		platform.Detect = []string{detected}
	}
	return &Manifest{
		Engine:          engine,
		DisplayName:     "CLI Fixture",
		ManifestVersion: 1,
		Platforms:       map[string]Platform{hostKey(): platform},
		Actions:         map[string]Action{"probe": {Cmd: []string{"{cli}", "models", "--json"}}},
	}
}

// TestAction_CliPlaceholderRunsTheCommandThatWasFound is the Homebrew case: the
// engine is installed, its command is in the package manager's bin directory, and
// the manifest's literal names a place that was never filled.
func TestAction_CliPlaceholderRunsTheCommandThatWasFound(t *testing.T) {
	root := t.TempDir()
	detected := writeEngineCLI(t, filepath.Join(root, "homebrew", "bin"), "mtplx", "found")
	literal := filepath.Join(root, "mtplx-home", "bin", "mtplx")

	executor := newTestExecutor(t, cliActionManifest("cli-found", detected, literal))
	installed, err := executor.Detect("cli-found")
	require.NoError(t, err)
	require.True(t, installed, "the fixture engine should read as installed")

	result, err := executor.Action(context.Background(), "cli-found", "probe", nil)

	require.NoError(t, err, "the action had a working engine command to run")
	assert.Contains(t, string(result), `"ran":"found"`, "the action ran the wrong command: %s", result)
	assert.Contains(t, string(result), "models --json", "the action lost its own arguments: %s", result)
}

// TestAction_CliPlaceholderFallsBackToTheManifestLiteral is the LM Studio case,
// which declares no detect paths at all: with nothing observed, the path the
// manifest names is the only one there can be, and the engine's actions have to
// still run.
func TestAction_CliPlaceholderFallsBackToTheManifestLiteral(t *testing.T) {
	root := t.TempDir()
	literal := writeEngineCLI(t, filepath.Join(root, "bin"), "lms", "literal")

	executor := newTestExecutor(t, cliActionManifest("cli-literal", "", literal))

	result, err := executor.Action(context.Background(), "cli-literal", "probe", nil)

	require.NoError(t, err, "a manifest that names its CLI can still run it")
	assert.Contains(t, string(result), `"ran":"literal"`, "the action ran the wrong command: %s", result)
}

// TestAction_CliPlaceholderCannotBeSuppliedByTheCaller keeps the new resolution
// on the same side of the line as the old one: {cli} is runner-owned, so a params
// key of that name is dropped rather than becoming argv[0].
func TestAction_CliPlaceholderCannotBeSuppliedByTheCaller(t *testing.T) {
	root := t.TempDir()
	detected := writeEngineCLI(t, filepath.Join(root, "bin"), "mtplx", "found")

	executor := newTestExecutor(t, cliActionManifest("cli-caller", detected, ""))
	installed, err := executor.Detect("cli-caller")
	require.NoError(t, err)
	require.True(t, installed)

	result, err := executor.Action(context.Background(), "cli-caller", "probe",
		json.RawMessage(`{"cli":"/bin/sh","model":"qwen"}`))

	require.NoError(t, err)
	assert.Contains(t, string(result), `"ran":"found"`, "a caller param reached argv[0]: %s", result)
	assert.NotContains(t, string(result), "literal", "a caller param replaced the engine command: %s", result)
}
