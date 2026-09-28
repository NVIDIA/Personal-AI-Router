// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package appdir resolves the single per-user data directory for NVIDIA PAIR,
// so every component agrees on one location.
//
// Layout: <base>/Nvidia Corporation/NVIDIA PAIR, where <base> is:
//   - Windows: %LocalAppData% (deliberately non-roaming — this is
//     machine-specific state that must not sync across machines).
//   - Linux:   $XDG_CONFIG_HOME or ~/.config.
//   - macOS:   ~/Library/Application Support.
//
// Keep orgDir, appDir, previousDirs, preservedEntries, and migrationLockName in
// sync with desktop/src/shared/constants/app.ts; Electron migrates the same
// directories under the same lock before it reads its own state.
package appdir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// orgDir / appDir are the two-level vendor/product path segments.
	orgDir = "Nvidia Corporation"
	appDir = "NVIDIA PAIR"

	// migrationLockName sits in the org directory so Electron and every broker
	// (desktop app, nvpair TUI, headless install) serialize on one file.
	migrationLockName = ".nvpair-data-migration.lock"
	lockStaleAfter    = 2 * time.Minute
	lockWait          = 10 * time.Second
	lockPoll          = 100 * time.Millisecond
)

// previousDirs are earlier data locations, newest first. Migrate drains them in
// order, so where two hold the same entry the newer copy wins.
var previousDirs = [][2]string{
	{"Nvidia Corporation", "Personal AI Router"},
	{"NVIDIA Corporation", "PAIR"},
}

// preservedEntries stay in a previous directory. `bin` holds the Windows `nvpair`
// launcher, whose absolute path is on the user's PATH; moving it would break
// `nvpair` in terminals that are already open.
var preservedEntries = []string{"bin"}

// ErrMigrationBusy reports that another process held the migration lock for
// longer than Migrate was willing to wait.
var ErrMigrationBusy = errors.New("app data migration lock is held by another process")

// Dir returns the per-user data directory, creating no files. It errors only
// when the platform base directory can't be determined.
func Dir() (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, orgDir, appDir), nil
}

// Path joins elems onto Dir().
func Path(elems ...string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{d}, elems...)...), nil
}

// Migration describes what Migrate did with each previous directory it found.
type Migration struct {
	// Source is the previous directory that existed.
	Source string
	// Kept lists paths left in Source because the destination already had an
	// entry of that name or the move failed (for example, a locked file).
	Kept []string
}

// Migrate moves data from every previous location into Dir() without
// overwriting anything already there. Run it once per process, before any
// component reads or writes the data directory: a worker that starts first
// would mint a fresh node identity and cluster state that the migration then
// refuses to replace.
func Migrate() ([]Migration, error) {
	base, err := baseDir()
	if err != nil {
		return nil, err
	}
	return migrate(base, lockWait)
}

func migrate(base string, wait time.Duration) ([]Migration, error) {
	orgPath := filepath.Join(base, orgDir)
	if err := os.MkdirAll(orgPath, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", orgPath, err)
	}
	release, err := acquireLock(filepath.Join(orgPath, migrationLockName), wait)
	if err != nil {
		return nil, err
	}
	defer release()

	destination := filepath.Join(orgPath, appDir)
	var migrations []Migration
	for _, prev := range previousDirs {
		source := filepath.Join(base, prev[0], prev[1])
		m, ok, err := migrateDirectory(source, destination)
		if err != nil {
			return migrations, err
		}
		if ok {
			migrations = append(migrations, m)
		}
	}
	return migrations, nil
}

// migrateDirectory merges source into destination. It reports ok=false when
// source does not exist or is the destination itself (a case-only difference on
// a case-insensitive filesystem).
func migrateDirectory(source, destination string) (Migration, bool, error) {
	srcInfo, err := os.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return Migration{}, false, nil
	}
	if err != nil {
		return Migration{}, false, err
	}
	if dstInfo, err := os.Stat(destination); err == nil && os.SameFile(srcInfo, dstInfo) {
		return Migration{}, false, nil
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return Migration{}, false, fmt.Errorf("create %s: %w", destination, err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return Migration{}, false, fmt.Errorf("read %s: %w", source, err)
	}

	m := Migration{Source: source}
	for _, entry := range entries {
		if isPreserved(entry.Name()) {
			continue
		}
		moveEntry(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()), &m.Kept)
	}
	// Only removes an empty directory; preserved and kept entries hold it open.
	_ = os.Remove(source)
	return m, true, nil
}

// moveEntry renames src onto a missing dst, or merges two directories entry by
// entry. It never replaces an existing file.
func moveEntry(src, dst string, kept *[]string) {
	if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(src, dst); err != nil {
			*kept = append(*kept, src)
		}
		return
	} else if err != nil {
		*kept = append(*kept, src)
		return
	}

	srcInfo, srcErr := os.Lstat(src)
	dstInfo, dstErr := os.Lstat(dst)
	if srcErr != nil || dstErr != nil || !srcInfo.IsDir() || !dstInfo.IsDir() {
		*kept = append(*kept, src)
		return
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		*kept = append(*kept, src)
		return
	}
	for _, entry := range entries {
		moveEntry(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), kept)
	}
	_ = os.Remove(src)
}

func isPreserved(name string) bool {
	for _, p := range preservedEntries {
		if name == p {
			return true
		}
	}
	return false
}

// acquireLock creates path exclusively, waiting up to wait for a live holder and
// breaking a lock older than lockStaleAfter, which a crashed holder leaves behind.
func acquireLock(path string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, ErrMigrationBusy
		}
		time.Sleep(lockPoll)
	}
}

// baseDir picks the platform base. On Windows we use %LocalAppData% rather than
// os.UserConfigDir (which returns roaming %AppData%), so this node-specific
// state stays local. Elsewhere os.UserConfigDir already returns the right base.
func baseDir() (string, error) {
	if runtime.GOOS == "windows" {
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return la, nil
		}
	}
	return os.UserConfigDir()
}
