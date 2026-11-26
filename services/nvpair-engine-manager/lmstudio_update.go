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
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const lmStudioDaemonGuide = "https://lmstudio.ai/docs/cli/daemon/daemon-update"

// LM Studio publishes numeric builds separately from its three-part version.
var lmStudioVersionRE = regexp.MustCompile(`^[0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9}(-[0-9]{1,9})?$`)
var lmStudioHostRE = regexp.MustCompile(`^(llmster|LM Studio|Bionic) v([^ ]+) is running \(PID: [0-9]+\)$`)

type lmStudioTarget struct {
	version string
	daemon  bool
	path    string // Versioned llmster executable selected by the vendor.
	running bool   // Host process, including when its HTTP server is off.
}

func lmStudioHome(cli string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Join(home, ".lmstudio-home-pointer"))
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Dir(filepath.Dir(cli)), nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	root := strings.TrimSpace(string(data))
	if err != nil || len(data) > 4096 || !filepath.IsAbs(root) {
		return "", errors.New("invalid LM Studio home pointer")
	}
	return filepath.Clean(root), nil
}

// Read the location used by "lms daemon up". Older version directories can
// remain installed, so picking the greatest directory can select the wrong copy.
func lmStudioDaemonInstall(root string) (lmStudioTarget, error) {
	path, err := lmStudioInstallPath(root, "llmster-install-location.json")
	if path == "" || err != nil {
		return lmStudioTarget{}, err
	}
	version := filepath.Base(filepath.Dir(path))
	name := "llmster"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	expected := filepath.Join(root, "llmster", version, name)
	if !lmStudioVersionRE.MatchString(version) || path != expected {
		return lmStudioTarget{}, errors.New("unrecognized llmster installation location")
	}
	return lmStudioTarget{version: version, daemon: true, path: expected}, nil
}

// The human-readable status includes the host's product and version. Its JSON
// output omits the version, and "lms --version" reports the separate CLI.
func (e *Executor) lmStudioTarget(ctx context.Context, cli string) (lmStudioTarget, error) {
	if cli == "" {
		return lmStudioTarget{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lmstudioHostStatusTimeout)
	defer cancel()
	output, err := e.runCommandOutput(ctx, []string{cli, "daemon", "status"})
	if err != nil {
		return lmStudioTarget{}, fmt.Errorf("read LM Studio host: %w", err)
	}
	root, err := lmStudioHome(cli)
	if err != nil {
		return lmStudioTarget{}, err
	}
	status := strings.TrimSpace(output)
	var liveVersion string
	if status != "LM Studio is not running" {
		match := lmStudioHostRE.FindStringSubmatch(status)
		if match == nil {
			return lmStudioTarget{}, errors.New("unrecognized LM Studio host status")
		}
		liveVersion = strings.Replace(match[2], "+", "-", 1)
		if !lmStudioVersionRE.MatchString(liveVersion) {
			return lmStudioTarget{}, errors.New("unrecognized LM Studio host version")
		}
		switch match[1] {
		case "Bionic":
			return lmStudioTarget{}, nil
		case "LM Studio":
			version, err := lmStudioAppVersion(root, liveVersion)
			return lmStudioTarget{version: version, running: true}, err
		}
	}
	target, err := lmStudioDaemonInstall(root)
	if liveVersion != "" {
		// A desktop and headless install can share lms. A running daemon
		// whose version disagrees with the local pointer is external.
		if err != nil || (liveVersion != target.version && liveVersion != strings.SplitN(target.version, "-", 2)[0]) {
			return lmStudioTarget{version: liveVersion, daemon: true, running: true}, nil
		}
		target.running = true
	}
	if err != nil || target.daemon {
		return target, err
	}
	// When stopped, lms starts the selected llmster first, then the app.
	version, err := lmStudioAppVersion(root, "")
	return lmStudioTarget{version: version}, err
}

func lmStudioNewer(latest, current string) bool {
	// A live host sometimes reports only its core version. A same-core build
	// update can only be established when the installed build is known.
	if !strings.Contains(current, "-") {
		latest, _, _ = strings.Cut(latest, "-")
	}
	return compareVersionParts(versionParts(latest), versionParts(current)) > 0
}

func (e *Executor) latestLMStudio(ctx context.Context, target lmStudioTarget) (string, error) {
	osName := runtime.GOOS
	if osName == "windows" {
		osName = "win32"
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86"
	}
	product := ""
	if target.daemon {
		product = "daemon/"
	}
	current, _, _ := strings.Cut(target.version, "-")
	endpoint := fmt.Sprintf("https://versions-prod.lmstudio.ai/update/%s%s/%s/%s", product, osName, arch, current)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	var release struct {
		OS, Arch, Version, Build string
	}
	if err := readEngineVersionJSON(e.client, req, &release); err != nil {
		return "", fmt.Errorf("check LM Studio release: %w", err)
	}
	version := release.Version + "-" + release.Build
	if release.OS != osName || release.Arch != arch || !lmStudioVersionRE.MatchString(version) {
		return "", errors.New("unrecognized LM Studio release")
	}
	return version, nil
}

func managedLMStudio(st *engineState, cli string, target lmStudioTarget, running, adopted bool) bool {
	if !target.daemon || target.path == "" || adopted || target.running != running {
		return false
	}
	st.mu.Lock()
	bundled := st.plat.InstallDir == "" && st.bundledInstall != nil && reflect.DeepEqual(st.plat.Install, st.bundledInstall)
	configuredCLI := expandPath(st.plat.Runtime.CLI)
	st.mu.Unlock()
	if !bundled || configuredCLI != cli {
		return false
	}
	if _, err := readEngineInstallMarker(st.installDir, "lmstudio"); err != nil {
		return false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	root := filepath.Join(home, ".lmstudio")
	defaultCLI := filepath.Join(root, "bin", "lms"+filepath.Ext(target.path))
	defaultDaemon := filepath.Join(root, "llmster", target.version, filepath.Base(target.path))
	if cli != defaultCLI || target.path != defaultDaemon || os.Getenv("LMS_API_SERVER_INFO_PATH") != "" {
		return false
	}
	// Upgrades execute a local binary; a redirected version directory must
	// never confer the PAIR install marker on another installation.
	resolved, err := filepath.EvalSymlinks(target.path)
	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	resolvedCLI, cliErr := filepath.EvalSymlinks(cli)
	return err == nil && rootErr == nil && cliErr == nil && isManagedInstallPath(resolvedCLI, resolvedRoot) &&
		resolved == filepath.Join(resolvedRoot, "llmster", target.version, filepath.Base(target.path))
}

func (e *Executor) CheckLMStudioUpdate(ctx context.Context) (*EngineUpdateInfo, error) {
	return e.checkInstalledEngineUpdate(ctx, "lmstudio", func(ctx context.Context, st *engineState, snap engineUpdateSnapshot) (*EngineUpdateInfo, error) {
		target, err := e.lmStudioTarget(ctx, snap.binPath)
		if err != nil || target.version == "" {
			return nil, err
		}
		latest, err := e.latestLMStudio(ctx, target)
		if err != nil || !lmStudioNewer(latest, target.version) {
			return nil, err
		}
		info := &EngineUpdateInfo{
			CurrentVersion: target.version,
			LatestVersion:  latest,
			InstallType:    "external",
			ReleaseURL:     "https://lmstudio.ai/changelog/lmstudio",
			DownloadURL:    "https://lmstudio.ai/download#lm-studio-download-heading",
		}
		if target.daemon {
			info.ReleaseURL, info.DownloadURL = lmStudioDaemonGuide, lmStudioDaemonGuide
		}
		if managedLMStudio(st, snap.binPath, target, snap.running, snap.adopted) {
			info.InstallType, info.DownloadURL = "managed", ""
		}
		return info, nil
	})
}

func (e *Executor) UpdateLMStudio(ctx context.Context) error {
	return e.withEngineUpdate(ctx, "lmstudio", e.updateLMStudio)
}

func (e *Executor) updateLMStudio(ctx context.Context, st *engineState) (result error) {
	found, cli := detectPath(st)
	target, err := e.lmStudioTarget(ctx, cli)
	if err != nil {
		return err
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	presence := e.reconcilePresence(ctx, "lmstudio", st, found, port, false)
	st.mu.Lock()
	running, adopted := st.running, st.adopted
	st.mu.Unlock()
	if !found || (presence.Occupied && !running) || !managedLMStudio(st, cli, target, running, adopted) {
		return errors.New("updates require a PAIR-managed llmster installation")
	}
	guard, err := lockEngineInstall(st.installDir)
	if err != nil {
		return err
	}
	defer guard.Close()
	latest, err := e.latestLMStudio(ctx, target)
	if err != nil || !lmStudioNewer(latest, target.version) {
		return err
	}
	if runtime.GOOS == "linux" {
		// Match the prerequisite check lms performs before delegating to llmster.
		libs, err := e.runCommandOutput(ctx, []string{"ldconfig", "-p"})
		if err != nil || !strings.Contains(libs, "libatomic.so.1") {
			return errors.New("llmster updates on Linux require ldconfig and libatomic.so.1")
		}
	}
	if current, err := e.lmStudioTarget(ctx, cli); err != nil || current != target || !managedLMStudio(st, cli, current, running, adopted) {
		return errors.New("the LM Studio installation changed during the update check")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if running {
		if err := e.doStop(st, "lmstudio"); err != nil {
			return err
		}
		defer func() {
			if e.shuttingDown.Load() || e.stopWaiting(st) {
				return
			}
			restartCtx := context.WithoutCancel(ctx)
			current, err := e.lmStudioTarget(restartCtx, cli)
			if err != nil || !managedLMStudio(st, cli, current, false, false) {
				result = errors.Join(result, errors.New("cannot restart llmster because a stopped PAIR installation is no longer selected"))
				return
			}
			result = errors.Join(result, e.doStart(restartCtx, st, "lmstudio", startOpts{RequireOwned: true}))
		}()
		stopped, err := e.lmStudioTarget(ctx, cli)
		if err != nil || stopped.running || stopped.path != target.path {
			return errors.New("llmster must be stopped before updating")
		}
	}
	e.emitInstallProgress("lmstudio", "installing", 0)
	// lms daemon update delegates to this native updater. Direct execution
	// lets PAIR await the versioned, checksum-verified install on Windows
	// without its interactive cmd.exe window. The only Windows prompt with
	// the daemon stopped is an acknowledgement before the updater exits.
	updateCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(updateCtx, target.path, "upgrade", "--channel=stable")
	cmd.Dir = filepath.Dir(target.path)
	cmd.Stdin = strings.NewReader("\n")
	configureSysProcAttr(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("update llmster: %w", err)
	}
	updated, err := lmStudioDaemonInstall(filepath.Dir(filepath.Dir(cli)))
	if err != nil || updated.version == "" || compareVersionParts(versionParts(updated.version), versionParts(latest)) < 0 {
		return fmt.Errorf("llmster did not install version %s", latest)
	}
	return nil
}
