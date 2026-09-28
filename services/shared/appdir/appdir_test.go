// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package appdir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist (stat err: %v)", path, err)
	}
}

func current(base string, elems ...string) string {
	return filepath.Join(append([]string{base, orgDir, appDir}, elems...)...)
}

func personalAIRouter(base string, elems ...string) string {
	return filepath.Join(append([]string{base, "Nvidia Corporation", "Personal AI Router"}, elems...)...)
}

func pair(base string, elems ...string) string {
	return filepath.Join(append([]string{base, "NVIDIA Corporation", "PAIR"}, elems...)...)
}

var fastLockTiming = lockTiming{staleAfter: 400 * time.Millisecond, heartbeat: 50 * time.Millisecond, poll: 10 * time.Millisecond}

func lockPath(base string) string {
	return filepath.Join(base, orgDir, migrationLockName)
}

func age(t *testing.T, path string, by time.Duration) {
	t.Helper()
	stale := time.Now().Add(-by)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

// migrateWithin runs migrate and fails the test instead of hanging when it does
// not return in time.
func migrateWithin(t *testing.T, base string, timeout time.Duration) ([]Migration, error) {
	t.Helper()
	type result struct {
		migrations []Migration
		err        error
	}
	done := make(chan result, 1)
	go func() {
		migrations, err := migrate(base, fastLockTiming)
		done <- result{migrations, err}
	}()
	select {
	case r := <-done:
		return r.migrations, r.err
	case <-time.After(timeout):
		t.Fatalf("migrate did not return within %s", timeout)
		return nil, nil
	}
}

func mustMigrate(t *testing.T, base string) []Migration {
	t.Helper()
	migrations, err := migrateWithin(t, base, 5*time.Second)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return migrations
}

func TestMigrateWithNoPreviousDirectory(t *testing.T) {
	base := t.TempDir()

	if migrations := mustMigrate(t, base); len(migrations) != 0 {
		t.Fatalf("migrations = %+v, want none", migrations)
	}
	assertMissing(t, current(base))
}

func TestMigrateMovesPersonalAIRouterData(t *testing.T) {
	base := t.TempDir()
	writeFile(t, personalAIRouter(base, "node-id.json"), "old-node")
	writeFile(t, personalAIRouter(base, "cluster", "node.key"), "old-key")
	writeFile(t, personalAIRouter(base, "engine-bin", "ollama", "ollama.exe"), "engine")

	migrations := mustMigrate(t, base)

	if len(migrations) != 1 || migrations[0].Moved != 3 || len(migrations[0].Kept) != 0 {
		t.Fatalf("migrations = %+v, want one that moved three entries and kept none", migrations)
	}
	if got := readFile(t, current(base, "node-id.json")); got != "old-node" {
		t.Fatalf("node-id.json = %q, want old-node", got)
	}
	if got := readFile(t, current(base, "cluster", "node.key")); got != "old-key" {
		t.Fatalf("cluster/node.key = %q, want old-key", got)
	}
	if got := readFile(t, current(base, "engine-bin", "ollama", "ollama.exe")); got != "engine" {
		t.Fatalf("engine binary = %q, want engine", got)
	}
	assertMissing(t, personalAIRouter(base))
}

func TestMigrateMovesOldestPAIRData(t *testing.T) {
	base := t.TempDir()
	writeFile(t, pair(base, "settings.json"), "pair-settings")

	mustMigrate(t, base)

	if got := readFile(t, current(base, "settings.json")); got != "pair-settings" {
		t.Fatalf("settings.json = %q, want pair-settings", got)
	}
}

func TestMigratePrefersNewerPreviousDirectory(t *testing.T) {
	base := t.TempDir()
	writeFile(t, personalAIRouter(base, "node-id.json"), "newer")
	writeFile(t, pair(base, "node-id.json"), "older")
	writeFile(t, pair(base, "only-in-pair.json"), "pair-only")

	mustMigrate(t, base)

	if got := readFile(t, current(base, "node-id.json")); got != "newer" {
		t.Fatalf("node-id.json = %q, want newer", got)
	}
	if got := readFile(t, current(base, "only-in-pair.json")); got != "pair-only" {
		t.Fatalf("only-in-pair.json = %q, want pair-only", got)
	}
	if got := readFile(t, pair(base, "node-id.json")); got != "older" {
		t.Fatalf("older conflicting copy = %q, want it kept in place", got)
	}
}

func TestMigrateNeverOverwritesDestination(t *testing.T) {
	base := t.TempDir()
	writeFile(t, current(base, "cluster", "node.key"), "current-key")
	writeFile(t, personalAIRouter(base, "cluster", "node.key"), "old-key")
	writeFile(t, personalAIRouter(base, "cluster", "trusted", "peer.crt"), "peer")

	migrations := mustMigrate(t, base)

	if got := readFile(t, current(base, "cluster", "node.key")); got != "current-key" {
		t.Fatalf("cluster/node.key = %q, want current-key", got)
	}
	if got := readFile(t, current(base, "cluster", "trusted", "peer.crt")); got != "peer" {
		t.Fatalf("trusted/peer.crt = %q, want merged peer", got)
	}
	want := personalAIRouter(base, "cluster", "node.key")
	if len(migrations) != 1 || len(migrations[0].Kept) != 1 || migrations[0].Kept[0] != want {
		t.Fatalf("migrations = %+v, want only %s kept", migrations, want)
	}
	if got := readFile(t, want); got != "old-key" {
		t.Fatalf("kept source = %q, want old-key", got)
	}
}

func TestMigrateLeavesLauncherBin(t *testing.T) {
	base := t.TempDir()
	writeFile(t, personalAIRouter(base, "bin", "nvpair.cmd"), "launcher")
	writeFile(t, personalAIRouter(base, "settings.json"), "settings")

	mustMigrate(t, base)

	if got := readFile(t, personalAIRouter(base, "bin", "nvpair.cmd")); got != "launcher" {
		t.Fatalf("launcher = %q, want it left in the previous directory", got)
	}
	assertMissing(t, current(base, "bin"))
	if got := readFile(t, current(base, "settings.json")); got != "settings" {
		t.Fatalf("settings.json = %q, want settings", got)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	base := t.TempDir()
	writeFile(t, personalAIRouter(base, "settings.json"), "settings")

	mustMigrate(t, base)
	if migrations := mustMigrate(t, base); len(migrations) != 0 {
		t.Fatalf("second run migrations = %+v, want none", migrations)
	}
	if got := readFile(t, current(base, "settings.json")); got != "settings" {
		t.Fatalf("settings.json = %q, want settings", got)
	}
}

func TestMigrateDoesNotReportLeftoverLauncher(t *testing.T) {
	base := t.TempDir()
	writeFile(t, personalAIRouter(base, "bin", "nvpair.cmd"), "launcher")

	if migrations := mustMigrate(t, base); len(migrations) != 0 {
		t.Fatalf("migrations = %+v, want none for a directory holding only bin", migrations)
	}
}

func TestMigrateReportsEntriesItCouldNotMove(t *testing.T) {
	base := t.TempDir()
	// cluster is a file-versus-directory conflict, so it is kept. The read-only
	// source makes renaming settings.json out of it fail, standing in for a
	// locked file.
	writeFile(t, current(base, "cluster"), "not a directory")
	writeFile(t, personalAIRouter(base, "cluster", "node.key"), "old-key")
	writeFile(t, personalAIRouter(base, "settings.json"), "settings")
	if err := os.Chmod(personalAIRouter(base), 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(personalAIRouter(base), 0o755) })

	migrations, err := migrateWithin(t, base, 5*time.Second)

	if len(migrations) != 1 || len(migrations[0].Kept) != 1 {
		t.Fatalf("migrations = %+v, want the cluster conflict kept", migrations)
	}
	if len(migrations[0].Failed) == 0 {
		// Windows ignores the read-only bit on directories, and root ignores it
		// everywhere, so there the rename succeeds and nothing fails.
		if err != nil {
			t.Fatalf("migrate err = %v, want nil when nothing failed", err)
		}
		return
	}
	if !errors.Is(err, errIncomplete) {
		t.Fatalf("migrate err = %v, want errIncomplete", err)
	}
}

func TestMigrateReleasesLock(t *testing.T) {
	base := t.TempDir()

	mustMigrate(t, base)

	assertMissing(t, lockPath(base))
}

func TestMigrateWaitsForLiveLock(t *testing.T) {
	base := t.TempDir()
	writeFile(t, personalAIRouter(base, "settings.json"), "settings")
	writeFile(t, lockPath(base), "other-holder")

	done := make(chan error, 1)
	go func() {
		_, err := migrate(base, fastLockTiming)
		done <- err
	}()
	for deadline := time.Now().Add(fastLockTiming.staleAfter / 2); time.Now().Before(deadline); {
		select {
		case err := <-done:
			t.Fatalf("migrate returned %v while another process held a fresh lock", err)
		case <-time.After(fastLockTiming.poll):
		}
		now := time.Now()
		if err := os.Chtimes(lockPath(base), now, now); err != nil {
			t.Fatalf("refresh lock: %v", err)
		}
	}
	if got := readFile(t, personalAIRouter(base, "settings.json")); got != "settings" {
		t.Fatalf("source settings.json = %q, want untouched while the lock is held", got)
	}
	if err := os.Remove(lockPath(base)); err != nil {
		t.Fatalf("release other holder's lock: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("migrate did not proceed after the lock was released")
	}
	if got := readFile(t, current(base, "settings.json")); got != "settings" {
		t.Fatalf("settings.json = %q, want settings", got)
	}
}

func TestMigrateBreaksStaleLock(t *testing.T) {
	base := t.TempDir()
	writeFile(t, lockPath(base), "crashed-holder")
	age(t, lockPath(base), 2*fastLockTiming.staleAfter)
	writeFile(t, personalAIRouter(base, "settings.json"), "settings")

	mustMigrate(t, base)

	if got := readFile(t, current(base, "settings.json")); got != "settings" {
		t.Fatalf("settings.json = %q, want settings", got)
	}
}

func TestMigrateFailsOnStaleLockItCannotRemove(t *testing.T) {
	base := t.TempDir()
	// A non-empty directory is a lock no platform lets os.Remove delete.
	writeFile(t, filepath.Join(lockPath(base), "held"), "")
	age(t, lockPath(base), 2*fastLockTiming.staleAfter)

	_, err := migrateWithin(t, base, 5*time.Second)

	if err == nil {
		t.Fatal("migrate err = nil, want an error for an unremovable stale lock")
	}
	assertMissing(t, lockPath(base)+".break")
}

func TestBreakIfStaleLeavesFreshLock(t *testing.T) {
	base := t.TempDir()
	writeFile(t, lockPath(base), "live-holder")

	if err := breakIfStale(lockPath(base), fastLockTiming.staleAfter); err != nil {
		t.Fatalf("breakIfStale: %v", err)
	}

	if got := readFile(t, lockPath(base)); got != "live-holder" {
		t.Fatalf("lock = %q, want the live holder's lock left in place", got)
	}
}

func TestHeldLockStaysFreshPastStaleAfter(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, orgDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lock, err := acquireLock(lockPath(base), fastLockTiming)
	if err != nil {
		t.Fatalf("acquireLock: %v", err)
	}

	time.Sleep(2 * fastLockTiming.staleAfter)

	if isStale(lockPath(base), fastLockTiming.staleAfter) {
		t.Fatal("held lock went stale; the heartbeat did not refresh it")
	}
	lock.release()
	assertMissing(t, lockPath(base))
}

func TestReleaseLeavesAnotherHoldersLock(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, orgDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lock, err := acquireLock(lockPath(base), fastLockTiming)
	if err != nil {
		t.Fatalf("acquireLock: %v", err)
	}
	writeFile(t, lockPath(base), "someone-else")

	lock.release()

	if got := readFile(t, lockPath(base)); got != "someone-else" {
		t.Fatalf("lock = %q, want another holder's lock left in place", got)
	}
}
