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
// Keep orgDir, appDir, previousDirs, preservedEntries, migrationLockName,
// lockStaleAfter, and lockPoll in sync with desktop/src/shared/constants/app.ts
// and desktop/src/electron/app-data-migration.ts; Electron migrates the same
// directories under the same lock before it writes its own state.
package appdir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// orgDir / appDir are the two-level vendor/product path segments.
	orgDir = "Nvidia Corporation"
	appDir = "NVIDIA PAIR"

	// migrationLockName sits in the org directory so Electron and every broker
	// (desktop app, nvpair TUI, headless install) serialize on one file.
	migrationLockName = ".nvpair-data-migration.lock"
	// lockStaleAfter is how long a lock may go without its holder refreshing
	// its modification time before a waiter treats the holder as crashed.
	lockStaleAfter = 2 * time.Minute
	lockPoll       = 100 * time.Millisecond
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

var errIncomplete = errors.New("app data migration could not move every entry")

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

// Migration describes what Migrate did with one previous directory.
type Migration struct {
	// Source is the previous directory that was drained.
	Source string
	// Moved counts the entries moved into Dir().
	Moved int
	// Kept lists paths left in Source because the destination already had an
	// entry of that name. They are never retried.
	Kept []string
	// Failed lists paths that could not be moved (for example, a locked file).
	// The next Migrate retries them.
	Failed []string
}

// Migrate moves data from every previous location into Dir() without
// overwriting anything already there. It returns only the directories where
// something moved or failed to move.
//
// Call it before any component reads or writes the data directory, and do not
// start one when it errors: a worker that starts first mints a fresh node
// identity and cluster state that a later migration refuses to replace. It
// waits for as long as another process holds the lock, because a live holder
// keeps the lock fresh and a crashed one lets it go stale.
func Migrate() ([]Migration, error) {
	base, err := baseDir()
	if err != nil {
		return nil, err
	}
	return migrate(base, defaultLockTiming)
}

type lockTiming struct {
	staleAfter time.Duration
	heartbeat  time.Duration
	poll       time.Duration
}

var defaultLockTiming = lockTiming{staleAfter: lockStaleAfter, heartbeat: lockStaleAfter / 4, poll: lockPoll}

func migrate(base string, timing lockTiming) ([]Migration, error) {
	orgPath := filepath.Join(base, orgDir)
	if err := os.MkdirAll(orgPath, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", orgPath, err)
	}
	lock, err := acquireLock(filepath.Join(orgPath, migrationLockName), timing)
	if err != nil {
		return nil, err
	}
	defer lock.release()

	destination := filepath.Join(orgPath, appDir)
	var migrations []Migration
	var failed []string
	for _, prev := range previousDirs {
		source := filepath.Join(base, prev[0], prev[1])
		m, err := migrateDirectory(source, destination)
		if err != nil {
			return migrations, err
		}
		if m.Moved > 0 || len(m.Failed) > 0 {
			migrations = append(migrations, m)
		}
		failed = append(failed, m.Failed...)
	}
	if len(failed) > 0 {
		return migrations, fmt.Errorf("%w: %s", errIncomplete, strings.Join(failed, ", "))
	}
	return migrations, nil
}

// migrateDirectory merges source into destination. A missing source, or one that
// is the destination itself (a case-only difference on a case-insensitive
// filesystem), yields an empty Migration.
func migrateDirectory(source, destination string) (Migration, error) {
	m := Migration{Source: source}
	srcInfo, err := os.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if dstInfo, err := os.Stat(destination); err == nil && os.SameFile(srcInfo, dstInfo) {
		return m, nil
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return m, fmt.Errorf("create %s: %w", destination, err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return m, fmt.Errorf("read %s: %w", source, err)
	}

	for _, entry := range entries {
		if isPreserved(entry.Name()) {
			continue
		}
		moveEntry(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()), &m)
	}
	// Only removes an empty directory; preserved and kept entries hold it open.
	_ = os.Remove(source)
	return m, nil
}

// moveEntry renames src onto a missing dst, or merges two directories entry by
// entry. It never replaces an existing file.
func moveEntry(src, dst string, m *Migration) {
	if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(src, dst); err != nil {
			m.Failed = append(m.Failed, src)
		} else {
			m.Moved++
		}
		return
	} else if err != nil {
		m.Failed = append(m.Failed, src)
		return
	}

	srcInfo, srcErr := os.Lstat(src)
	dstInfo, dstErr := os.Lstat(dst)
	if srcErr != nil || dstErr != nil {
		m.Failed = append(m.Failed, src)
		return
	}
	if !srcInfo.IsDir() || !dstInfo.IsDir() {
		m.Kept = append(m.Kept, src)
		return
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		m.Failed = append(m.Failed, src)
		return
	}
	for _, entry := range entries {
		moveEntry(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()), m)
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

// migrationLock is held by exactly one process. Its file holds the owner's
// token, and a goroutine refreshes its modification time so waiters can tell a
// long migration from a crashed one.
type migrationLock struct {
	path  string
	token string
	stop  chan struct{}
	done  chan struct{}
}

// acquireLock creates path exclusively, waiting while a live holder has it and
// breaking it once it goes stale. It errors rather than waits forever when a
// stale lock cannot be removed.
func acquireLock(path string, timing lockTiming) (*migrationLock, error) {
	token := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	for {
		err := createExclusive(path, token)
		if err == nil {
			lock := &migrationLock{path: path, token: token, stop: make(chan struct{}), done: make(chan struct{})}
			go lock.heartbeat(timing.heartbeat)
			return lock, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create %s: %w", path, err)
		}
		if err := breakIfStale(path, timing.staleAfter); err != nil {
			return nil, err
		}
		time.Sleep(timing.poll)
	}
}

func createExclusive(path, contents string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(contents)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// breakIfStale removes path when its holder stopped refreshing it. A separate
// breaker file serializes the removal, so two waiters that both saw the same
// stale lock cannot have the second delete the lock the first one then created.
func breakIfStale(path string, staleAfter time.Duration) error {
	if !isStale(path, staleAfter) {
		return nil
	}
	breaker := path + ".break"
	if err := createExclusive(breaker, ""); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create %s: %w", breaker, err)
		}
		// A breaker that outlived a whole stale period belongs to a waiter that
		// crashed mid-break.
		if isStale(breaker, staleAfter) {
			if err := os.Remove(breaker); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove stale %s: %w", breaker, err)
			}
		}
		return nil
	}
	defer os.Remove(breaker)
	if !isStale(path, staleAfter) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale migration lock %s: %w", path, err)
	}
	return nil
}

func isStale(path string, staleAfter time.Duration) bool {
	info, err := os.Stat(path)
	return err == nil && time.Since(info.ModTime()) > staleAfter
}

func (l *migrationLock) owned() bool {
	data, err := os.ReadFile(l.path)
	return err == nil && string(data) == l.token
}

func (l *migrationLock) heartbeat(interval time.Duration) {
	defer close(l.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			if !l.owned() {
				return
			}
			now := time.Now()
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

func (l *migrationLock) release() {
	close(l.stop)
	<-l.done
	if l.owned() {
		_ = os.Remove(l.path)
	}
}

// baseDir picks the platform base. On Windows we use %LocalAppData% rather than
// os.UserConfigDir (which returns roaming %AppData%), so this node-specific
// state stays local; without LOCALAPPDATA it takes the Local sibling of the
// roaming directory, as Electron does. Elsewhere os.UserConfigDir already
// returns the right base.
func baseDir() (string, error) {
	if runtime.GOOS != "windows" {
		return os.UserConfigDir()
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return la, nil
	}
	roaming, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return strings.Replace(roaming, "Roaming", "Local", 1), nil
}
