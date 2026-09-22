// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

func llamaExecutable() string {
	if runtime.GOOS == "windows" {
		return "llama.exe"
	}
	return "llama"
}

var llamaPinnedVersion = regexp.MustCompile(`(?m)^(?:b10826-[a-zA-Z0-9]+|version: .*\(build 10826, commit [a-zA-Z0-9]+\))`)

func llamaInstallSupport(goos, arch string) (bool, string) {
	if arch != "amd64" && arch != "arm64" {
		return false, "The official llama app has no build for this CPU architecture."
	}
	if goos != "windows" && goos != "linux" && goos != "darwin" {
		return false, "The official llama app installer is unavailable on this operating system."
	}
	if goos == "darwin" && arch == "amd64" {
		return true, "The official Intel Mac llama app uses CPU inference; Radeon acceleration is not provided by this recipe."
	}
	if goos == "windows" && arch == "arm64" {
		return true, "Native hardware inventory selects CPU for non-NVIDIA ARM; NVIDIA ARM remains CUDA-required, including when its driver needs repair."
	}
	return true, "The vendor installer selects an available accelerator or CPU build; acceleration is verified after installation."
}

// The vendor installer owns a fixed home-relative staging directory. Give it
// a fresh, private home and request download-only; never let it replace a user
// binary, mutate PATH, or update the live runtime in place.
func llamaInstallerEnv(st *engineState, stage string) (map[string]string, error) {
	env, err := childEnv(st)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA"} {
		env[key] = stage
	}
	env["SKIP_INSTALL"] = "1"
	env["LLAMA_VERSION"] = "b10826"
	env["LLAMA_BUCKET"] = "ggml-org/install.sh"
	return env, nil
}

// installLlamaApp stages, verifies and promotes a fresh managed runtime. Install
// returns before reaching here when a managed runtime is already detected or an
// external identified listener is adopted, so no runtime is running and the
// runtime slot is empty (or holds an incomplete image) when promotion happens.
func (e *Executor) installLlamaApp(ctx context.Context, st *engineState) (err error) {
	const engine = "llamacpp"
	defer func() {
		if err != nil {
			e.reportInstallFailed(engine, err)
		}
	}()
	if ok, reason := llamaInstallSupport(runtime.GOOS, runtime.GOARCH); !ok {
		return errors.New(reason)
	}
	if st.plat.Install == nil || (len(st.plat.Install.Archives) == 0 && (st.plat.Install.Fetch == nil || st.plat.Install.Fetch.SHA256 == "")) {
		return errors.New("llama app requires a pinned official installer or archive recipe")
	}
	if err = os.MkdirAll(st.installDir, 0o700); err != nil {
		return err
	}
	if err = validateLlamaOwnedPaths(st.installDir); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(st.installDir, ".llama-install-")
	if err != nil {
		return err
	}
	// Preserve failed stages for diagnosis. A successful stage contains no models.
	defer func() {
		if err == nil {
			_ = safeRemoveUnderRoot(st.installDir, stage)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	env, err := llamaInstallerEnv(st, stage)
	if err != nil {
		return err
	}
	candidate := filepath.Join(stage, ".llama-app")
	if runtime.GOOS == "windows" {
		candidate = filepath.Join(stage, "llama-app")
	}
	var provenance map[string]any
	if st.plat.Install.UpstreamFirst {
		candidate, provenance, err = e.prepareLlamaWindowsARM(ctx, st, stage)
		if err != nil {
			return err
		}
	} else if len(st.plat.Install.Archives) > 0 {
		if err = e.stageLlamaArchives(ctx, st, candidate); err != nil {
			return fmt.Errorf("official llama archives: %w", err)
		}
	} else {
		script, downloadErr := e.download(ctx, engine, st.plat.Install.Fetch)
		if downloadErr != nil {
			return downloadErr
		}
		defer os.Remove(script)
		e.emitInstallProgress(engine, "installing", -1)
		if err = e.runCommand(ctx, llamaInstallerArgs(runtime.GOOS, script), env); err != nil {
			return fmt.Errorf("official llama installer: %w", err)
		}
	}
	e.emitInstallProgress(engine, "installing", -1)
	bin := filepath.Join(candidate, llamaExecutable())
	if provenance == nil {
		version, err := e.runCommandOutput(ctx, []string{bin, "version"}, env)
		if err != nil {
			return fmt.Errorf("validate downloaded llama: %w", err)
		}
		if !llamaPinnedVersion.MatchString(version) {
			return fmt.Errorf("official llama version did not match pinned build b10826")
		}
		licenses, err := e.runCommandOutput(ctx, []string{bin, "licenses"}, env)
		if err != nil {
			return fmt.Errorf("read llama third-party licenses: %w", err)
		}
		if err = os.WriteFile(filepath.Join(candidate, "THIRD-PARTY-LICENSES.txt"), []byte(licenses), 0o600); err != nil {
			return err
		}
		f, err := os.Open(bin)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		provenance = map[string]any{"version": strings.TrimSpace(version), "binary_sha256": hex.EncodeToString(h.Sum(nil)), "platform": runtime.GOOS + "/" + runtime.GOARCH}
		if len(st.plat.Install.Archives) > 0 {
			provenance["archives"] = st.plat.Install.Archives
			provenance["recipe_sha256"] = llamaArchiveRecipeHash(st.plat.Install.Archives)
		} else {
			provenance["installer_sha256"] = st.plat.Install.Fetch.SHA256
			provenance["installer_url"] = st.plat.Install.Fetch.URL
		}
	}
	receipt, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(candidate, "pair-install.json"), receipt, 0o600); err != nil {
		return err
	}
	// A cancellation (Stop, shutdown, or the caller's context) that arrives
	// after the candidate is complete must still leave the runtime slot alone.
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = promoteLlamaRuntime(st.installDir, candidate); err != nil {
		return err
	}
	// A promoted candidate the manifest cannot detect is not an install; the
	// generic path proves the same thing with waitDetect.
	if installed, _ := e.Detect(engine); !installed {
		return fmt.Errorf("engine %q was not detected after install", engine)
	}
	e.reporter.clear(installFailedID(engine))
	e.emitInstallProgress(engine, "done", 100)
	e.emitState(engine)
	return nil
}

// llamaArchiveRecipeHash identifies the exact pinned archive set that produced a
// runtime, so the receipt distinguishes recipes that share a build number but
// differ in a companion bundle (for example the CUDA runtime).
func llamaArchiveRecipeHash(archives []Fetch) string {
	h := sha256.New()
	for _, archive := range archives {
		fmt.Fprintf(h, "%q %q\n", archive.URL, archive.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Called after acquiring the official installer, whose checksum every caller
// has verified by this point: pinned recipes against their manifest entry, and
// Windows ARM64 latest against the commit pin in llamaupstream.go.
// The Windows policy override lasts for this installer process only.
func llamaInstallerArgs(goos, script string) []string {
	if goos == "windows" {
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}
	}
	return []string{"sh", script}
}

// Recover the gap between the two promotion renames after process termination.
// Called once when creating the engine state, before any operation can start.
func recoverLlamaRuntime(root string) error {
	current, previous := filepath.Join(root, "runtime"), filepath.Join(root, "previous")
	if _, err := os.Lstat(current); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(previous); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := validateLlamaOwnedPaths(root); err != nil {
		return err
	}
	return os.Rename(previous, current)
}

func promoteLlamaRuntime(root, candidate string) error {
	if err := validateLlamaOwnedPaths(root); err != nil {
		return err
	}
	if !isManagedInstallPath(candidate, root) {
		return errors.New("candidate is outside managed install")
	}
	if info, err := os.Stat(candidate); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New("candidate runtime is not a directory")
	}
	current, previous := filepath.Join(root, "runtime"), filepath.Join(root, "previous")
	if _, err := os.Lstat(previous); err == nil {
		if err := safeRemoveUnderRoot(root, previous); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	hadCurrent := false
	if _, err := os.Lstat(current); err == nil {
		if err := os.Rename(current, previous); err != nil {
			return err
		}
		hadCurrent = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(candidate, current); err != nil {
		if hadCurrent {
			return errors.Join(err, os.Rename(previous, current))
		}
		return err
	}
	return nil
}

func removeLlamaRuntime(st *engineState) error {
	if err := validateLlamaOwnedPaths(st.installDir); err != nil {
		return err
	}
	// Models and diagnostic stages are deliberately retained. Only the two
	// runtime slots are PAIR's executable installation.
	for _, name := range []string{"runtime", "previous"} {
		target := filepath.Join(st.installDir, name)
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := safeRemoveUnderRoot(st.installDir, target); err != nil {
			return err
		}
	}
	st.mu.Lock()
	st.binPath = ""
	st.mu.Unlock()
	return nil
}

func validateLlamaOwnedPaths(root string) error {
	for _, name := range []string{"", "runtime", "previous", "models"} {
		p, err := filepath.Abs(filepath.Join(root, name))
		if err != nil {
			return err
		}
		if err := validateLlamaPath(p); err != nil {
			return err
		}
	}
	return nil
}

func llamaPrerequisite() string {
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("powershell.exe"); err != nil {
			return "PowerShell is required by the official llama installer."
		}
	} else {
		for _, tool := range []string{"sh", "curl"} {
			if _, err := exec.LookPath(tool); err != nil {
				return tool + " is required by the official llama installer."
			}
		}
	}
	return ""
}
