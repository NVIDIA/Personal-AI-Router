// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"nvpair-shared/engines"
)

func managedVLLMBinary(st *engineState, bin string) bool {
	if !managedUpdateBinary(st, bin) {
		return false
	}
	configured, err := launchForState(st, 0, false)
	if err != nil {
		return false
	}
	protected := func(args []string, env hfEnv) bool {
		stores := []string{env.home(), env.hubCache(), hfLaunchHub(args, env)}
		if model, ok := launchFlagValue(args, "--model"); ok && filepath.IsAbs(model) {
			stores = append(stores, model)
		}
		return modelStoresOutsideInstall(st, stores...)
	}
	if !protected(configured.Args, engineHFEnv(st)) {
		return false
	}
	st.mu.Lock()
	proc := st.proc
	st.mu.Unlock()
	if proc == nil {
		return true
	}
	launched := hfEnv{}
	for _, kv := range proc.cmd.Env {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			launched[key] = value
		}
	}
	return protected(proc.cmd.Args, launched)
}

func (e *Executor) UpdateVLLM(ctx context.Context) error {
	return e.withEngineUpdate(ctx, engines.NameVLLM, e.updateVLLM)
}

func (e *Executor) updateVLLM(ctx context.Context, st *engineState) error {
	bin, _, err := e.managedUpdateTarget(ctx, st, managedVLLMBinary)
	if err != nil {
		return err
	}
	guard, err := lockEngineInstall(st.installDir)
	if err != nil {
		return err
	}
	defer guard.Close()
	inst := st.bundledInstall
	target, err := vllmRecipeVersion(inst)
	if err != nil {
		return err
	}
	metal := runtime.GOOS == "darwin"
	installed, err := installedVLLMVersions(ctx, bin, metal)
	if err != nil {
		return err
	}
	old, err := installed.olderPackage(target, metal)
	if err != nil || old == "" {
		return err
	}
	// Fetch and verify the pinned bootstrap before interrupting inference.
	archive, err := e.download(ctx, engines.NameVLLM, inst.Fetch)
	if err != nil {
		return err
	}
	defer os.Remove(archive)
	active, running, err := e.managedUpdateTarget(ctx, st, managedVLLMBinary)
	if err != nil {
		return err
	}
	if bin != active {
		return errors.New("the detected vLLM installation changed during the update")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if running {
		if err := e.doStop(st, engines.NameVLLM); err != nil {
			return err
		}
	}
	restart := func(ctx context.Context) error {
		if !running || e.shuttingDown.Load() || e.stopWaiting(st) {
			return nil
		}
		return e.doStart(ctx, st, engines.NameVLLM, startOpts{RequireOwned: true})
	}
	err = rebuildVLLMInstall(st.installDir, func() error {
		e.emitInstallProgress(engines.NameVLLM, "installing", 0)
		if err := e.runInstallCommand(ctx, inst, map[string]string{"install_dir": st.installDir, "download": archive}, guard); err != nil {
			return err
		}
		versions, err := installedVLLMVersions(ctx, bin, metal)
		if err != nil {
			return err
		}
		// +cpu and +cuNNN identify platform wheel builds.
		core, _, _ := strings.Cut(versions.Core, "+")
		if core != target || metal && versions.Metal != target {
			return fmt.Errorf("installed vLLM does not match the pinned release %s", target)
		}
		return ctx.Err()
	}, func() error { return restart(ctx) })
	if err != nil {
		// An unconfirmed rollback must never launch a partially built environment.
		if original, probeErr := installedVLLMVersions(context.WithoutCancel(ctx), bin, metal); probeErr == nil && original == installed {
			return errors.Join(err, restart(context.WithoutCancel(ctx)))
		}
	}
	return err
}

const vllmPendingMarker = ".nvpair-vllm-update-in-progress"

// Python environments have absolute interpreter paths and console scripts.
// Build at the final path, keeping the entire original tree for rollback.
func rebuildVLLMInstall(dir string, build, start func() error) error {
	if !filepath.IsAbs(dir) || !isEngineUpdateBackup(dir, engines.NameVLLM) {
		return errors.New("cannot rebuild an unowned vLLM installation")
	}
	marker, err := readEngineInstallMarker(dir, engines.NameVLLM)
	if err != nil {
		return err
	}
	backup := dir + ".update-backup"
	if err := discardEngineUpdateBackup(dir, engines.NameVLLM); err != nil {
		return err
	}
	if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("vLLM update backup location %s is occupied", backup)
	}
	prepared, err := os.MkdirTemp(filepath.Dir(dir), ".vllm-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(prepared)
	if err := os.WriteFile(filepath.Join(prepared, vllmPendingMarker), marker, 0o600); err != nil {
		return err
	}
	if err := renameEngineDirectory(dir, backup); err != nil {
		return err
	}
	if err := renameEngineDirectory(prepared, dir); err != nil {
		return errors.Join(err, renameEngineDirectory(backup, dir))
	}
	err = build()
	if err == nil {
		// Only a verified complete install can lose its pending marker.
		err = os.Rename(filepath.Join(dir, vllmPendingMarker), installMarkerPath(dir))
	}
	if err == nil {
		err = start()
	}
	if err != nil {
		if moveErr := renameEngineDirectory(dir, prepared); moveErr != nil {
			return errors.Join(err, fmt.Errorf("move failed vLLM aside; previous copy at %s: %w", backup, moveErr))
		}
		return errors.Join(err, renameEngineDirectory(backup, dir))
	}
	// Rename away from the recovery path before deleting the obsolete copy.
	if err := renameEngineDirectory(backup, prepared); err != nil {
		slog.Warn("could not retire previous vLLM installation", "err", err)
	}
	return nil
}

func recoverVLLMUpdate(dir string) error {
	backup := dir + ".update-backup"
	if !filepath.IsAbs(dir) {
		return nil
	}
	if _, err := os.Lstat(dir + ".update-lock"); errors.Is(err, os.ErrNotExist) && !isEngineUpdateBackup(backup, engines.NameVLLM) {
		return nil
	}
	guard, err := lockEngineInstall(dir)
	if err != nil {
		return err
	}
	defer guard.Close()
	if !isEngineUpdateBackup(backup, engines.NameVLLM) {
		return nil
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return renameEngineDirectory(backup, dir)
	}
	if err != nil || !fi.IsDir() {
		return err
	}
	pending := filepath.Join(dir, vllmPendingMarker)
	fi, err = os.Lstat(pending)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return errors.New("unrecognized vLLM update recovery marker")
	}
	oldMarker, err := readEngineInstallMarker(backup, engines.NameVLLM)
	if err != nil {
		return err
	}
	candidate, err := os.ReadFile(pending)
	if err != nil || !bytes.Equal(candidate, oldMarker) {
		return errors.New("vLLM update recovery marker does not match its backup")
	}
	// Park the recognized partial tree before restoring the working one, so
	// another interruption can still recover by observing the missing root.
	park, err := os.MkdirTemp(filepath.Dir(dir), ".vllm-interrupted-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(park)
	if err := renameEngineDirectory(dir, filepath.Join(park, "partial")); err != nil {
		return err
	}
	return renameEngineDirectory(backup, dir)
}
