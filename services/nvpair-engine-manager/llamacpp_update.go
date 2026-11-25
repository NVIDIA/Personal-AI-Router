// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/engines"
)

const llamaReleasesURL = "https://github.com/ggml-org/llama.cpp/releases"

// The stable release contains a short text file pointing to the numbered build
// that actually publishes the binaries, including the CUDA runtime archives.
const latestLlamaBuildURL = llamaReleasesURL + "/latest/download/nightly-tag.txt"

type llamaBuild int

func (b llamaBuild) tag() string { return "b" + strconv.Itoa(int(b)) }

func parseLlamaBuild(tag string) (llamaBuild, error) {
	build, err := strconv.Atoi(strings.TrimPrefix(tag, "b"))
	if err != nil || build <= 0 || llamaBuild(build).tag() != tag {
		return 0, errors.New("unrecognized llama.cpp build number")
	}
	return llamaBuild(build), nil
}

var llamaVersionLine = regexp.MustCompile(`(?m)^version: (?:[^\s]+ \(build ([0-9]+), commit [^()\s]+\)|([0-9]+) \([^()\s]+\))\r?$`)

func llamaBinaryBuild(ctx context.Context, binPath string) (llamaBuild, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "--version")
	if runtime.GOOS == "linux" {
		// The Linux release ships libllama and its CUDA libraries beside the
		// executable. A staged probe must load that copy of the libraries.
		libraries := filepath.Dir(binPath)
		if inherited := os.Getenv("LD_LIBRARY_PATH"); inherited != "" {
			libraries += string(os.PathListSeparator) + inherited
		}
		cmd.Env = processEnvironment(binPath, map[string]string{"LD_LIBRARY_PATH": libraries})
	}
	configureSysProcAttr(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("read installed llama.cpp build: %w", err)
	}
	match := llamaVersionLine.FindSubmatch(out)
	if match == nil {
		return 0, errors.New("llama-server --version did not report a build number")
	}
	return parseLlamaBuild("b" + string(match[1]) + string(match[2]))
}

func (e *Executor) latestLlamaBuild(ctx context.Context) (llamaBuild, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestLlamaBuildURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("check llama.cpp stable release: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65))
	if err != nil {
		return 0, err
	}
	if len(data) > 64 {
		return 0, errors.New("invalid llama.cpp stable release pointer")
	}
	return parseLlamaBuild(strings.TrimSpace(string(data)))
}

func managedLlamaBinary(st *engineState, binPath string) bool {
	if !managedUpdateBinary(st, binPath) {
		return false
	}
	launch, err := launchForState(st, 0, false)
	if err != nil {
		return false
	}
	env := processEnvironment(binPath, launch.Env)
	if env == nil {
		env = os.Environ()
	}
	st.mu.Lock()
	proc := st.proc
	st.mu.Unlock()
	if proc != nil && !llamaModelsOutsideInstall(st, proc.cmd.Args, proc.cmd.Env) {
		return false
	}
	return llamaModelsOutsideInstall(st, launch.Args, env)
}

func llamaModelsOutsideInstall(st *engineState, args, env []string) bool {
	stores := []string{environmentValue(env, "LLAMA_CACHE"), environmentValue(env, "LLAMA_ARG_MODELS_DIR"),
		environmentValue(env, "LLAMA_ARG_MODEL"), environmentValue(env, "LLAMA_ARG_MODELS_PRESET")}
	for _, flag := range []string{"--models-dir", "--model", "-m", "--models-preset"} {
		if path, ok := launchFlagValue(args, flag); ok {
			stores = append(stores, path)
		}
	}
	return modelStoresOutsideInstall(st, stores...)
}

func (e *Executor) CheckLlamaCppUpdate(ctx context.Context) (*EngineUpdateInfo, error) {
	return e.checkInstalledEngineUpdate(ctx, engines.NameLlamaCPP, func(ctx context.Context, st *engineState, s engineUpdateSnapshot) (*EngineUpdateInfo, error) {
		if s.processPath != "" && !isOurEngineImage(s.processPath, s.binPath) {
			return nil, nil
		}
		var current llamaBuild
		var err error
		if s.running {
			var props struct {
				BuildInfo string `json:"build_info"`
			}
			if err := e.readRunningEngineVersion(ctx, s.port, "/props", &props); err != nil {
				return nil, err
			}
			tag, _, _ := strings.Cut(props.BuildInfo, "-")
			current, err = parseLlamaBuild(tag)
		} else {
			current, err = llamaBinaryBuild(ctx, s.binPath)
		}
		if err != nil {
			return nil, err
		}
		var installer *Install
		ownership := "external"
		if !s.adopted && managedLlamaBinary(st, s.binPath) {
			installer, ownership = st.bundledInstall, "managed"
		}
		target, _, err := e.llamaUpdateRelease(ctx, current, installer)
		if err != nil || target <= current {
			return nil, err
		}
		url := llamaReleasesURL + "/tag/" + target.tag()
		info := &EngineUpdateInfo{CurrentVersion: current.tag(), LatestVersion: target.tag(), ReleaseURL: url, InstallType: ownership}
		if installer == nil {
			info.DownloadURL = url
		}
		return info, nil
	})
}

// A managed update uses exactly the OS, architecture, CUDA version and archive
// format in the bundled recipe. When a vendor changes variants, the bundled
// checksummed build remains available to older installations.
func (e *Executor) llamaUpdateRelease(ctx context.Context, current llamaBuild, inst *Install) (llamaBuild, *Install, error) {
	latest, err := e.latestLlamaBuild(ctx)
	if err != nil || inst == nil {
		return latest, nil, err
	}
	pin, err := bundledLlamaBuild(inst)
	if err != nil || latest <= pin || current >= latest {
		return pin, inst, err
	}
	release, err := e.githubRelease(ctx, "ggml-org/llama.cpp", "tags/"+latest.tag())
	if err != nil {
		return 0, nil, err
	}
	// Resolve all required assets before offering an update or downloading one.
	archive := func(oldURL string) (*Fetch, error) {
		name := strings.TrimPrefix(oldURL, llamaReleasesURL+"/download/"+pin.tag()+"/")
		name = strings.ReplaceAll(name, "-"+pin.tag()+"-", "-"+latest.tag()+"-")
		url := llamaReleasesURL + "/download/" + latest.tag() + "/" + name
		return githubReleaseAssetFetch(release.Assets, name, url)
	}
	candidate := *inst
	if inst.Fetch != nil {
		candidate.Fetch, err = archive(inst.Fetch.URL)
		if err != nil {
			return pin, inst, nil
		}
	}
	candidate.Artifacts = slices.Clone(inst.Artifacts)
	for i, artifact := range inst.Artifacts {
		fetch, err := archive(artifact.URL)
		if err != nil {
			return pin, inst, nil
		}
		candidate.Artifacts[i].URL, candidate.Artifacts[i].SHA256 = fetch.URL, fetch.SHA256
	}
	return latest, &candidate, nil
}

func bundledLlamaBuild(inst *Install) (llamaBuild, error) {
	var build llamaBuild
	check := func(url, checksum string) bool {
		tail, ok := strings.CutPrefix(url, llamaReleasesURL+"/download/")
		tag, name, hasFile := strings.Cut(tail, "/")
		version, err := parseLlamaBuild(tag)
		if !ok || !hasFile || name == "" || strings.Contains(name, "/") || !sha256Re.MatchString(checksum) || err != nil || (build != 0 && build != version) {
			return false
		}
		build = version
		return true
	}
	invalid := errors.New("the bundled llama.cpp installer must pin every archive to one checksummed build")
	if inst.ModeOrDefault() != "user" || len(inst.Run) == 0 || len(inst.Script) != 0 {
		return 0, invalid
	}
	if inst.Fetch != nil && !check(inst.Fetch.URL, inst.Fetch.SHA256) {
		return 0, invalid
	}
	for _, artifact := range inst.Artifacts {
		if !check(artifact.URL, artifact.SHA256) {
			return 0, invalid
		}
	}
	if build == 0 {
		return 0, invalid
	}
	return build, nil
}

func (e *Executor) UpdateLlamaCpp(ctx context.Context) error {
	return e.updateStagedEngine(ctx, engines.NameLlamaCPP, managedLlamaBinary, func(ctx context.Context, st *engineState, binPath, staging string) (bool, error) {
		current, err := llamaBinaryBuild(ctx, binPath)
		if err != nil {
			return false, err
		}
		target, inst, err := e.llamaUpdateRelease(ctx, current, st.bundledInstall)
		if err != nil || target <= current {
			return false, err
		}
		if err := e.installArchives(ctx, engines.NameLlamaCPP, inst, map[string]string{"install_dir": staging}); err != nil {
			return false, err
		}
		candidate, err := stagedEngineBinary(st, staging, binPath)
		if err != nil {
			return false, err
		}
		version, err := llamaBinaryBuild(ctx, candidate)
		if err != nil {
			return false, err
		}
		if version != target {
			return false, fmt.Errorf("the downloaded llama.cpp reports build %s, expected %s", version.tag(), target.tag())
		}
		return true, nil
	})
}
