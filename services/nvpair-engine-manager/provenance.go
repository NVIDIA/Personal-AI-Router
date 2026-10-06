// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// installMarkerName records that this service, rather than the user, performed
// an engine's install.
//
// Ownership is otherwise unknowable for an engine whose vendor script chooses
// its own destination. LM Studio installs into ~/.lmstudio no matter who starts
// it, so "the binary sits under our install directory" — the test that answers
// this for Ollama and llama.cpp — is never true for it, and PAIR cannot
// distinguish an install it performed from one the user already had. Uninstall
// has to know: removing an engine the user installed themselves, with the model
// library they built up in it, is not PAIR's call to make.
//
// The marker lives in the per-engine install directory, which is created for
// every install regardless of where the engine itself lands.
const installMarkerName = "installed-by-pair.json"

type installMarker struct {
	Engine      string `json:"engine"`
	InstalledAt string `json:"installed_at"`
}

// uninstallManagedTimeout bounds the whole --uninstall-managed pass. Removing a
// model-adjacent tree walks a lot of files, and a vendor's stop command can
// hang, but an uninstaller cannot wait forever for either.
const uninstallManagedTimeout = 3 * time.Minute

func installMarkerPath(installDir string) string {
	if installDir == "" {
		return ""
	}
	return filepath.Join(installDir, installMarkerName)
}

// writeInstallMarker claims an install. Called only after an install this
// service actually performed and detected — never on the adoption paths, which
// return before any install work happens.
func writeInstallMarker(installDir, engine string) error {
	path := installMarkerPath(installDir)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(path, installMarker{
		Engine:      engine,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// clearInstallMarker drops the claim after an uninstall. An engine whose
// install directory is itself removed loses the marker with it; one installed
// by a vendor script elsewhere does not, and a stale claim would let PAIR
// remove a copy the user later installed themselves.
func clearInstallMarker(installDir string) {
	if path := installMarkerPath(installDir); path != "" {
		_ = os.Remove(path)
	}
}

func installedByPAIR(installDir string) bool {
	path := installMarkerPath(installDir)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// uninstallManagedEngines removes every engine this service installed, for the
// platform uninstallers' "also remove my data" path. Without it that prompt is
// a false promise: it deletes the app data root, which happens to contain the
// Ollama and llama.cpp install directories, but never reaches an engine a
// vendor script placed in the user's home.
//
// Deliberately narrow. It runs the manifest's uninstall steps and nothing else
// — no port probes, no desired-state writes, no notifications — because by the
// time an uninstaller calls this, the engine processes are already killed and
// there is no broker to talk to. Model stores are preserved, the same as an
// interactive uninstall: removing PAIR does not delete the user's weights.
//
// Every failure is logged and skipped. An uninstaller must finish.
func uninstallManagedEngines(ctx context.Context, reg *Registry, installBase string) {
	if installBase == "" {
		slog.Warn("no app data directory; cannot identify PAIR-installed engines")
		return
	}
	for _, engine := range reg.Names() {
		installDir := filepath.Join(installBase, engine)
		if !installedByPAIR(installDir) {
			slog.Info("leaving engine in place; PAIR did not install it", "engine", engine)
			continue
		}
		manifest, ok := reg.Get(engine)
		if !ok {
			continue
		}
		platform, ok := manifest.HostPlatform()
		if !ok {
			continue
		}
		un := platform.Uninstall
		if un == nil {
			slog.Info("engine has no uninstall for this platform", "engine", engine)
			continue
		}
		modelsDir := ""
		if platform.ModelsDir != "" {
			modelsDir = expandPath(platform.ModelsDir)
		}
		vars := map[string]string{"install_dir": installDir, "models_dir": modelsDir}
		if argv, err := resolveArgs(un.Run, vars); err != nil {
			slog.Warn("cannot resolve uninstall command", "engine", engine, "err", err)
		} else if len(argv) > 0 {
			for i := range argv {
				argv[i] = expandPath(argv[i])
			}
			if err := runManifestCommand(ctx, argv, nil); err != nil {
				slog.Warn("uninstall command failed; continuing", "engine", engine, "err", err)
			}
		}
		targets, err := resolveArgs(un.Remove, vars)
		if err != nil {
			slog.Warn("cannot resolve uninstall removals", "engine", engine, "err", err)
			continue
		}
		for _, target := range targets {
			if err := removeTreePreserving(target, modelsDir); err != nil {
				slog.Warn("could not remove engine files; continuing", "engine", engine, "path", target, "err", err)
				continue
			}
			slog.Info("removed PAIR-installed engine files", "engine", engine, "path", target)
		}
		clearInstallMarker(installDir)
	}
}
