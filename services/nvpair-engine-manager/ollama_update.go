// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"nvpair-shared/engines"
)

// managedOllamaBinary is shared with the version check, so the UI offers a
// managed update only for a layout the updater can safely replace.
func managedOllamaBinary(st *engineState, binPath string) bool {
	if !managedUpdateBinary(st, binPath) {
		return false
	}
	// Ollama's models may have been relocated via its launch environment.
	// Preserve both the store used by a running daemon and the configured one
	// that would be used after restart, including a store reached via a link.
	launch, err := launchForState(st, 0, false)
	if err != nil {
		return false
	}
	env := processEnvironment(binPath, launch.Env)
	if env == nil {
		env = os.Environ()
	}
	configuredStore := strings.Trim(strings.TrimSpace(environmentValue(env, "OLLAMA_MODELS")), `"'`)
	if configuredStore == "" {
		configuredStore = filepath.Join(environmentValue(env, homeVariable()), ".ollama", "models")
	}
	return modelStoresOutsideInstall(st, ollamaBlobsDir(st), configuredStore)
}

// UpdateOllama stages a checksum-verified archive before replacing the
// original installation.
func (e *Executor) UpdateOllama(ctx context.Context) error {
	return e.updateStagedEngine(ctx, engines.NameOllama, managedOllamaBinary, e.stageOllamaUpdate)
}

func (e *Executor) stageOllamaUpdate(ctx context.Context, st *engineState, binPath, staging string) (bool, error) {
	current, err := ollamaBinaryVersion(ctx, binPath)
	if err != nil {
		return false, err
	}
	release, err := e.latestOllamaRelease(ctx)
	if err != nil {
		return false, err
	}
	order, err := compareOllamaVersions(current, release.Version)
	if err != nil || order >= 0 {
		return false, err
	}

	return true, e.stageOllamaRelease(ctx, st, release, staging, binPath)
}

func (release *ollamaRelease) archiveFor(inst *Install) (*Fetch, error) {
	if inst == nil || inst.ModeOrDefault() != "user" || inst.Fetch == nil || len(inst.Run) == 0 {
		return nil, errors.New("Ollama has no supported archive installer for this platform")
	}
	// Use the platform archive named by the installer, pinned to the release
	// we just checked. A floating vendor URL could change between requests.
	name := path.Base(inst.Fetch.URL)
	url := "https://github.com/ollama/ollama/releases/download/v" + release.Version + "/" + name
	return githubReleaseAssetFetch(release.Assets, name, url)
}

func (e *Executor) stageOllamaRelease(ctx context.Context, st *engineState, release *ollamaRelease, stage, binPath string) error {
	inst := st.bundledInstall
	fetch, err := release.archiveFor(inst)
	if err != nil {
		return err
	}
	pinned := *inst
	pinned.Fetch = fetch
	if err := e.installArchives(ctx, engines.NameOllama, &pinned, map[string]string{"install_dir": stage}); err != nil {
		return err
	}
	stagedBinary, err := stagedEngineBinary(st, stage, binPath)
	if err != nil {
		return err
	}
	version, err := ollamaBinaryVersion(ctx, stagedBinary)
	if err != nil {
		return fmt.Errorf("verify staged Ollama: %w", err)
	}
	if version != release.Version {
		return fmt.Errorf("the downloaded Ollama reports version %s, expected %s", version, release.Version)
	}
	return nil
}
