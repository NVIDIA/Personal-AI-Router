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

func mustMigrate(t *testing.T, base string) []Migration {
	t.Helper()
	migrations, err := migrate(base, time.Second)
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

	if len(migrations) != 1 || len(migrations[0].Kept) != 0 {
		t.Fatalf("migrations = %+v, want one with nothing kept", migrations)
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

func TestMigrateReleasesLock(t *testing.T) {
	base := t.TempDir()

	mustMigrate(t, base)

	assertMissing(t, filepath.Join(base, orgDir, migrationLockName))
}

func TestMigrateWaitsForLiveLock(t *testing.T) {
	base := t.TempDir()
	writeFile(t, personalAIRouter(base, "settings.json"), "settings")
	writeFile(t, filepath.Join(base, orgDir, migrationLockName), "")

	_, err := migrate(base, 200*time.Millisecond)

	if !errors.Is(err, ErrMigrationBusy) {
		t.Fatalf("migrate err = %v, want ErrMigrationBusy", err)
	}
	if got := readFile(t, personalAIRouter(base, "settings.json")); got != "settings" {
		t.Fatalf("source settings.json = %q, want untouched", got)
	}
}

func TestMigrateBreaksStaleLock(t *testing.T) {
	base := t.TempDir()
	lock := filepath.Join(base, orgDir, migrationLockName)
	writeFile(t, lock, "")
	stale := time.Now().Add(-2 * lockStaleAfter)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatalf("age lock: %v", err)
	}
	writeFile(t, personalAIRouter(base, "settings.json"), "settings")

	mustMigrate(t, base)

	if got := readFile(t, current(base, "settings.json")); got != "settings" {
		t.Fatalf("settings.json = %q, want settings", got)
	}
}
