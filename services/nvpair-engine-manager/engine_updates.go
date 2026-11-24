// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type EngineUpdateInfo struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	ReleaseURL     string `json:"releaseUrl"`
	DownloadURL    string `json:"downloadUrl,omitempty"`
	InstallType    string `json:"installType"`
}

// The vendor recipe determines the exact release URL and platform archive.
// Accept the published digest only for that same asset.
func githubReleaseAssetFetch(assets []githubReleaseAsset, name, url string) (*Fetch, error) {
	for _, asset := range assets {
		if asset.Name != name || asset.BrowserDownloadURL != url {
			continue
		}
		digest, ok := strings.CutPrefix(asset.Digest, "sha256:")
		if ok && sha256Re.MatchString(digest) {
			return &Fetch{URL: url, SHA256: digest}, nil
		}
		break
	}
	return nil, fmt.Errorf("no checksum-verified release asset for %s", name)
}

// An update may replace only a PAIR-owned binary installed using the bundled
// recipe. A custom installer or install directory may contain unrelated files.
func managedUpdateBinary(st *engineState, binPath string) bool {
	if !filepath.IsAbs(st.installDir) || st.bundledInstall == nil || !reflect.DeepEqual(st.plat.Install, st.bundledInstall) ||
		!isManagedInstallPath(binPath, st.installDir) {
		return false
	}
	if _, err := readEngineInstallMarker(st.installDir, st.manifest.Engine); err != nil {
		return false
	}
	resolvedDir, err := filepath.EvalSymlinks(st.installDir)
	if err != nil {
		return false
	}
	resolvedBin, err := filepath.EvalSymlinks(binPath)
	return err == nil && isManagedInstallPath(resolvedBin, resolvedDir) && modelStoresOutsideInstall(st, st.modelsDir)
}

// Protect configured and running model stores, including stores reached via
// filesystem links, before an update removes the previous engine tree.
func modelStoresOutsideInstall(st *engineState, stores ...string) bool {
	dirInfo, err := os.Lstat(st.installDir)
	if err != nil || !dirInfo.IsDir() {
		return false
	}
	for _, store := range stores {
		if store == "" {
			continue
		}
		if !filepath.IsAbs(store) {
			return false
		}
		location := locateStore(store)
		if location.is(dirInfo) || location.under(dirInfo) || location.contains(st.installDir) {
			return false
		}
	}
	return true
}

// The brief gap between the two directory renames can span a shutdown.
// Restore the previous managed tree before detection or auto-start sees it
// as missing. A promoted candidate already contains a verified executable.
func recoverStagedEngineInstall(dir, engine string) error {
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backup := dir + ".update-backup"
	if !isEngineUpdateBackup(backup, engine) {
		return nil
	}
	guard, err := lockEngineInstall(dir)
	if err != nil {
		return err
	}
	defer guard.Close()
	// Another manager may have finished the swap before we acquired the lock.
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return renameEngineDirectory(backup, dir)
}

// Windows can briefly keep an executable's directory locked after the
// version probe or server process exits. Bound the wait even during rollback.
func renameEngineDirectory(from, to string) error {
	const (
		accessDenied     = syscall.Errno(5)
		sharingViolation = syscall.Errno(32)
		lockViolation    = syscall.Errno(33)
	)
	for attempt := 1; ; attempt++ {
		err := os.Rename(from, to)
		if err == nil || runtime.GOOS != "windows" || attempt == 9 {
			return err
		}
		var errno syscall.Errno
		if !errors.As(err, &errno) || (errno != accessDenied && errno != sharingViolation && errno != lockViolation) {
			return err
		}
		time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
	}
}

func isEngineUpdateBackup(backup, engine string) bool {
	info, err := os.Lstat(backup)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = readEngineInstallMarker(backup, engine)
	return err == nil
}

func readEngineInstallMarker(dir, engine string) ([]byte, error) {
	p := installMarkerPath(dir)
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, fmt.Errorf("no valid PAIR install marker for %s", engine)
	}
	data, err := os.ReadFile(p)
	var marker installMarker
	if err != nil || json.Unmarshal(data, &marker) != nil || marker.Engine != engine {
		return nil, fmt.Errorf("no valid PAIR install marker for %s", engine)
	}
	return data, nil
}

func discardEngineUpdateBackup(dir, engine string) error {
	backup := dir + ".update-backup"
	if filepath.IsAbs(dir) && isEngineUpdateBackup(backup, engine) {
		return os.RemoveAll(backup)
	}
	return nil
}

// withEngineUpdate serializes provider work with engine lifecycle operations.
// An explicit Stop or shutdown can interrupt long transfers and installations.
func (e *Executor) withEngineUpdate(ctx context.Context, engine string, op func(context.Context, *engineState) error) (result error) {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.lockOperation()
	defer st.unlockOperation()
	// Stop and prepare-shutdown cancel the transfer before waiting on opMu.
	// doStart registers its own cancellation callback during restart.
	ctx, cancel := context.WithCancel(ctx)
	st.mu.Lock()
	st.startCancel = cancel
	st.mu.Unlock()
	defer func() {
		cancel()
		st.mu.Lock()
		st.startCancel = nil
		st.mu.Unlock()
	}()
	defer func() {
		if result != nil {
			e.emitInstallFrame(engine, "failed", 0, result.Error())
			return
		}
		e.reporter.clear("engine-manager:update-failed:" + engine)
		e.emitInstallProgress(engine, "done", 100)
		e.emitState(engine)
	}()
	if e.shuttingDown.Load() || e.stopWaiting(st) {
		return context.Canceled
	}
	return op(ctx, st)
}

// The provider stages and verifies its release while the old engine runs.
// Returning false means the installed copy is already current.
func (e *Executor) updateStagedEngine(ctx context.Context, engine string, eligible func(*engineState, string) bool, stage func(context.Context, *engineState, string, string) (bool, error)) error {
	return e.withEngineUpdate(ctx, engine, func(ctx context.Context, st *engineState) error {
		if st.bundledInstall == nil || !installedByPAIR(st.installDir) {
			return fmt.Errorf("updates require a PAIR-managed %s installation", st.manifest.DisplayName)
		}
		guard, err := lockEngineInstall(st.installDir)
		if err != nil {
			return err
		}
		defer guard.Close()
		binPath, _, err := e.managedUpdateTarget(ctx, st, eligible)
		if err != nil {
			return err
		}
		staging, err := os.MkdirTemp(filepath.Dir(st.installDir), "."+engine+"-update-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(staging)
		ready, err := stage(ctx, st, binPath, staging)
		if err != nil || !ready {
			return err
		}
		target, running, err := e.managedUpdateTarget(ctx, st, eligible)
		if err != nil {
			return err
		}
		if target != binPath {
			return fmt.Errorf("the active %s installation changed during the download", st.manifest.DisplayName)
		}
		marker, err := readEngineInstallMarker(st.installDir, engine)
		if err != nil {
			return err
		}
		if err := os.WriteFile(installMarkerPath(staging), marker, 0o600); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.shuttingDown.Load() || e.stopWaiting(st) {
			return context.Canceled
		}
		return e.replaceEngineInstall(ctx, st, staging, running)
	})
}

func (e *Executor) managedUpdateTarget(ctx context.Context, st *engineState, eligible func(*engineState, string) bool) (string, bool, error) {
	found, binary := detectPath(st)
	if !found || !eligible(st, binary) {
		return "", false, fmt.Errorf("updates require a PAIR-managed %s installation", st.manifest.DisplayName)
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	presence := e.reconcilePresence(ctx, st.manifest.Engine, st, true, port, false)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.adopted || presence.Occupied && (st.proc == nil || !isOurEngineImage(st.proc.cmd.Path, binary)) {
		return "", false, fmt.Errorf("%s is running under external management", st.manifest.DisplayName)
	}
	return binary, st.running, nil
}

func stagedEngineBinary(st *engineState, staging, binPath string) (string, error) {
	rel, err := filepath.Rel(st.installDir, binPath)
	if err != nil {
		return "", err
	}
	stagedBinary := filepath.Join(staging, rel)
	resolved, err := filepath.EvalSymlinks(stagedBinary)
	resolvedStage, stageErr := filepath.EvalSymlinks(staging)
	if err != nil || stageErr != nil || !isManagedInstallPath(resolved, resolvedStage) {
		return "", fmt.Errorf("the %s archive does not contain its expected executable", st.manifest.DisplayName)
	}
	return stagedBinary, nil
}

func (e *Executor) replaceEngineInstall(ctx context.Context, st *engineState, staging string, running bool) error {
	engine, name := st.manifest.Engine, st.manifest.DisplayName
	dir := st.installDir
	backup := dir + ".update-backup"
	// A completed swap can leave a verified previous copy if PAIR exits
	// before cleanup. Never remove an unrelated directory at this path.
	if _, err := os.Lstat(backup); err == nil {
		if !isEngineUpdateBackup(backup, engine) {
			return fmt.Errorf("%s update backup location %q is occupied", name, backup)
		}
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove previous %s backup: %w", name, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if running {
		if err := e.doStop(st, engine); err != nil {
			return err
		}
	}
	restoreRunning := func(failure error) error {
		if running && !e.shuttingDown.Load() && !e.stopWaiting(st) {
			if restartErr := e.doStart(context.WithoutCancel(ctx), st, engine, startOpts{RequireOwned: true}); restartErr != nil {
				return errors.Join(failure, fmt.Errorf("restart previous %s: %w", name, restartErr))
			}
		}
		return failure
	}
	if err := renameEngineDirectory(dir, backup); err != nil {
		return restoreRunning(fmt.Errorf("back up installed %s: %w", name, err))
	}
	if err := renameEngineDirectory(staging, dir); err != nil {
		if rollbackErr := renameEngineDirectory(backup, dir); rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("restore %s from %s: %w", name, backup, rollbackErr))
		}
		return restoreRunning(err)
	}
	if running {
		if err := e.doStart(ctx, st, engine, startOpts{RequireOwned: true}); err != nil {
			// doStart terminates a failed candidate before it returns. Move
			// it back into staging so restoring the old copy needs no delete.
			if moveErr := renameEngineDirectory(dir, staging); moveErr != nil {
				return errors.Join(err, fmt.Errorf("move failed %s aside (previous copy at %s): %w", name, backup, moveErr))
			}
			if rollbackErr := renameEngineDirectory(backup, dir); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("restore %s from %s: %w", name, backup, rollbackErr))
			}
			return restoreRunning(err)
		}
	}
	// Retire the recovery name before cleanup. A partially removed backup
	// must never be mistaken for a recoverable installation.
	if err := renameEngineDirectory(backup, staging); err != nil {
		slog.Warn("could not retire previous engine after update", "engine", engine, "err", err)
	}
	return nil
}
