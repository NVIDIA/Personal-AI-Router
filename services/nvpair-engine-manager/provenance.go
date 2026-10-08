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

// uninstallManagedTimeout bounds the whole --uninstall-managed pass: the
// commands each uninstall runs, plus the check between engines that stops the
// sweep once it expires. A vendor's stop command can hang and an uninstaller
// cannot wait forever, but an individual file walk is not interruptible, so the
// budget has to be comfortably above one engine's worst case rather than tight.
const uninstallManagedTimeout = 15 * time.Minute

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

// ManagedUninstall is one engine's outcome from UninstallManaged. Reported per
// engine rather than as a single error, because the three callers need to tell
// the three cases apart: an engine removed, an engine that was never PAIR's to
// remove, and one that failed and therefore still has files on disk.
type ManagedUninstall struct {
	Engine  string `json:"engine"`
	Removed bool   `json:"removed"`
	// Error is empty for both a removal and a skip. A skipped engine is the
	// expected outcome for an install the user made themselves, so it is not a
	// failure and must not be reported as one.
	Error string `json:"error,omitempty"`
}

// UninstallManaged removes every engine this service installed, and only those.
//
// This backs the platform uninstallers' "also remove my data" and both clients'
// data reset. Without it that promise is false: deleting the app data root
// happens to remove the Ollama and llama.cpp install directories, but never
// reaches an engine a vendor script placed in the user's home.
//
// Selection is by install marker, so an engine the user installed themselves is
// never a candidate. That matters beyond safety: the alternative — offering
// every detected engine and letting Uninstall decline the rest — raises a
// service error per decline, and those sync to cluster peers, so resetting a
// machine with the user's own LM Studio would plant an error on its neighbours.
//
// Each removal goes through Uninstall, so it gets the whole safety path: the
// ownership checks, the stop, the retries a vendor daemon needs when it holds
// files open after being told to stop, model-store preservation, and detection
// as the verdict. A client must not reimplement any of that.
func (e *Executor) UninstallManaged(ctx context.Context) []ManagedUninstall {
	if e.baseDir == "" {
		slog.Warn("no app data directory; cannot identify PAIR-installed engines")
		return nil
	}
	names := e.reg.Names()
	results := make([]ManagedUninstall, 0, len(names))
	for _, engine := range names {
		// Checked per engine, because the budget below bounds the commands an
		// uninstall runs but not the file walks between them.
		if err := ctx.Err(); err != nil {
			slog.Warn("stopped removing engines before finishing", "err", err, "remaining", len(names)-len(results))
			return results
		}
		installDir := filepath.Join(e.baseDir, engine)
		if !installedByPAIR(installDir) {
			slog.Info("leaving engine in place; PAIR did not install it", "engine", engine)
			results = append(results, ManagedUninstall{Engine: engine})
			continue
		}
		if err := e.Uninstall(ctx, engine); err != nil {
			slog.Error("could not remove a PAIR-installed engine", "engine", engine, "err", err)
			// The marker stays. It is the only record that this engine is ours,
			// so dropping it here would make the engine unremovable forever.
			results = append(results, ManagedUninstall{Engine: engine, Error: err.Error()})
			continue
		}
		slog.Info("removed PAIR-installed engine", "engine", engine)
		results = append(results, ManagedUninstall{Engine: engine, Removed: true})
	}
	return results
}
