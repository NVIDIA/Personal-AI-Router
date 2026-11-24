// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
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
