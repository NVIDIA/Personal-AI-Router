// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/engines"
)

type vllmVersions struct {
	Core  string `json:"core"`
	Metal string `json:"metal"`
}

// Read package metadata from the CLI's own Python environment. Importing
// vllm during a background check would initialize its inference dependencies.
func installedVLLMVersions(ctx context.Context, bin string, metal bool) (vllmVersions, error) {
	python, err := vllmPython(bin)
	if err != nil {
		return vllmVersions{}, err
	}
	const probe = `import json, sys
from importlib.metadata import version
print(json.dumps({"core": version("vllm"), "metal": version("vllm-metal") if sys.argv[1] == "true" else ""}))`
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", probe, strconv.FormatBool(metal))
	configureSysProcAttr(cmd)
	output, err := cmd.Output()
	if err != nil {
		return vllmVersions{}, fmt.Errorf("read installed vLLM package metadata: %w", err)
	}
	var installed vllmVersions
	if err := json.Unmarshal(output, &installed); err != nil {
		return vllmVersions{}, err
	}
	return installed, nil
}

// vLLM uses Python distribution versions, including post releases and local
// CUDA/CPU wheel labels. A wheel label is not a newer engine release.
var vllmVersionPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+){1,3})(?:(a|b|rc|\.dev|\.post)([0-9]+))?(?:\+[a-zA-Z0-9]+(?:[._-][a-zA-Z0-9]+)*)?$`)

func vllmVersionKey(version string) ([6]int, error) {
	var key [6]int
	match := vllmVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return key, errors.New("unrecognized vLLM release version")
	}
	for index, field := range strings.Split(match[1], ".") {
		n, err := strconv.Atoi(field)
		if err != nil {
			return key, err
		}
		key[index] = n
	}
	if match[2] != "" {
		key[4] = map[string]int{".dev": -4, "a": -3, "b": -2, "rc": -1, ".post": 1}[match[2]]
		var err error
		key[5], err = strconv.Atoi(match[3])
		return key, err
	}
	return key, nil
}

// Both macOS distributions are pinned to the same release. Never downgrade
// either package if someone upgraded one independently.
func (v vllmVersions) olderPackage(target string, metal bool) (string, error) {
	latest, err := vllmVersionKey(target)
	if err != nil {
		return "", err
	}
	installed := []string{v.Core}
	if metal {
		installed = []string{v.Metal, v.Core}
	}
	var older string
	for _, version := range installed {
		current, err := vllmVersionKey(version)
		if err != nil {
			return "", err
		}
		switch slices.Compare(current[:], latest[:]) {
		case 1:
			return "", nil
		case -1:
			if older == "" {
				older = version
			}
		}
	}
	return older, nil
}

// The bundled recipe passes its release as the installer's third positional
// argument. The Linux package and both macOS wheels use that same value.
func vllmRecipeVersion(inst *Install) (string, error) {
	if inst == nil || inst.Fetch == nil || inst.Fetch.SHA256 == "" || len(inst.Run) < 2 || inst.Run[len(inst.Run)-2] != "{install_dir}" {
		return "", errors.New("vLLM has no pinned installer for this platform")
	}
	target := inst.Run[len(inst.Run)-1]
	_, err := vllmVersionKey(target)
	return target, err
}

func vllmReleaseProject(metal bool) (repo, download string) {
	if metal {
		return "vllm-project/vllm-metal", "https://github.com/vllm-project/vllm-metal#installation"
	}
	return "vllm-project/vllm", "https://docs.vllm.ai/en/latest/getting_started/installation/gpu/"
}

func (e *Executor) CheckVLLMUpdate(ctx context.Context) (*EngineUpdateInfo, error) {
	return e.checkInstalledEngineUpdate(ctx, engines.NameVLLM, e.checkVLLMRelease)
}

func (e *Executor) checkVLLMRelease(ctx context.Context, st *engineState, snapshot engineUpdateSnapshot) (*EngineUpdateInfo, error) {
	if snapshot.processPath != "" && !isOurEngineImage(snapshot.processPath, snapshot.binPath) {
		return nil, nil
	}
	metal := runtime.GOOS == "darwin"
	repo, download := vllmReleaseProject(metal)
	var installed vllmVersions
	if snapshot.adopted {
		if metal {
			// The server reports its core version, not the Metal plugin's.
			return nil, nil
		}
		var reply struct {
			Version string `json:"version"`
		}
		if err := e.readRunningEngineVersion(ctx, snapshot.port, "/version", &reply); err != nil {
			return nil, err
		}
		installed.Core = reply.Version
	} else {
		var err error
		installed, err = installedVLLMVersions(ctx, snapshot.binPath, metal)
		if err != nil {
			return nil, err
		}
	}
	target, installType := "", "external"
	if !snapshot.adopted && managedVLLMBinary(st, snapshot.binPath) {
		var err error
		target, err = vllmRecipeVersion(st.bundledInstall)
		if err != nil {
			return nil, err
		}
		installType = "managed"
		download = ""
	} else {
		release, err := e.latestGitHubRelease(ctx, repo)
		if err != nil {
			return nil, err
		}
		target = strings.TrimPrefix(release.Tag, "v")
		if release.Tag != "v"+target {
			return nil, errors.New("unrecognized vLLM release tag")
		}
		if metal {
			// Upstream publishes Metal separately. Its release number need
			// not match the core in a user-provisioned environment.
			installed.Core = installed.Metal
		}
	}
	current, err := installed.olderPackage(target, metal)
	if err != nil || current == "" {
		return nil, err
	}
	return &EngineUpdateInfo{
		CurrentVersion: current, LatestVersion: target, InstallType: installType,
		ReleaseURL:  "https://github.com/" + repo + "/releases/tag/v" + target,
		DownloadURL: download,
	}, nil
}
