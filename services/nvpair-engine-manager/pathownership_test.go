// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pathLifecycleExecutor(t *testing.T, engine, mode string) (*Executor, string, string) {
	t.Helper()
	ex, cli := pathInstallExecutor(t, engine, mode)
	st := engineStateForTest(t, ex, engine)
	// The uninstaller clears both the detected marker and the declared CLI, so
	// the post-uninstall detect poll actually goes negative.
	st.plat.Uninstall = &Uninstall{Run: append([]string{fakeEngineBin, "remove", cli}, st.plat.Detect...)}
	platform := st.manifest.Platforms[hostKey()]
	platform.Uninstall = st.plat.Uninstall
	st.manifest.Platforms[hostKey()] = platform
	home := t.TempDir()
	ex.addToPath = func(dir string, receipt *pathReceipt, save func() error) error {
		return addToShellPath(home, "sh", "", "", dir, receipt, save)
	}
	ex.removeFromPath = removeShellPath
	return ex, cli, filepath.Join(home, ".profile")
}

func TestUninstallRemovesOwnedPathAfterRestart(t *testing.T) {
	test := func(engine, mode string) {
		t.Run(engine, func(t *testing.T) {
			ex, cli, profile := pathLifecycleExecutor(t, engine, mode)
			original := "export PATH=\"$PATH:/user/tools\"\n"
			if err := os.WriteFile(profile, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := ex.Install(context.Background(), engine); err != nil {
				t.Fatal(err)
			}
			// A fresh executor must use the persisted ownership, not memory.
			restarted := NewExecutor(ex.reg, NewReporter(nil), func(string, any) {}, ex.baseDir)
			restarted.removeFromPath = removeShellPath
			engineStateForTest(t, restarted, engine).installDir = engineStateForTest(t, ex, engine).installDir
			if err := restarted.Uninstall(context.Background(), engine); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(profile)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != original {
				t.Errorf("uninstall changed the user's profile to %q, want %q", data, original)
			}
			if fileExists(cli) {
				t.Errorf("uninstall left the CLI behind at %q", cli)
			}
			if _, err := os.Stat(ex.pathReceiptFile(engine)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ownership receipt still exists: %v", err)
			}
		})
	}
	test("ollama", "process")
	test("lmstudio", "command")
}

func TestDetectedExternalEngineDoesNotAcquirePathOwnership(t *testing.T) {
	ex, cli, _ := pathLifecycleExecutor(t, "lmstudio", "command")
	st := engineStateForTest(t, ex, "lmstudio")
	// The vendor owns this location; PAIR never installed here.
	st.installDir = t.TempDir()
	for _, path := range append([]string{cli}, st.plat.Detect...) {
		if err := os.WriteFile(path, []byte("external CLI"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		t.Error("modified external engine PATH")
		return nil
	}
	ex.removeFromPath = func(*pathReceipt) error {
		t.Error("cleaned up an unowned PATH")
		return nil
	}
	if err := ex.Install(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	if err := ex.Uninstall(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
}

// A receipt that never landed must not make the next install report success
// while silently leaving PATH alone.
func TestManagedInstallWithNoReceiptStillClaimsPath(t *testing.T) {
	ex, cli, _ := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ex.pathReceiptFile("ollama")); err != nil {
		t.Fatal(err)
	}
	var added string
	ex.addToPath = func(dir string, _ *pathReceipt, _ func() error) error { added = dir; return nil }
	// Already detected, so this install short-circuits before any download.
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if added != filepath.Dir(cli) {
		t.Fatalf("added %q, want the lost ownership to be re-recorded for %q", added, filepath.Dir(cli))
	}
}

// A manifest update or a vendor relocation moves the CLI. The recorded entry
// still belongs to PAIR, so it has to move too rather than being left pointing
// at a directory the engine is no longer in.
func TestMovedCLIMigratesTheOwnedPathEntry(t *testing.T) {
	ex, cli, profile := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), filepath.Dir(cli)) {
		t.Fatalf("install did not publish the original directory: %q", before)
	}

	st := engineStateForTest(t, ex, "ollama")
	moved := filepath.Join(st.installDir, "tools", filepath.Base(cli))
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cli, moved); err != nil {
		t.Fatal(err)
	}
	st.plat.Runtime.CLI = moved

	// Already detected, so this is the short-circuit path.
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), filepath.Dir(cli)) {
		t.Fatalf("stale entry survived the move: %q", after)
	}
	if !strings.Contains(string(after), filepath.Dir(moved)) {
		t.Fatalf("new directory was not published: %q", after)
	}
	if warning := pathWarning(ex, "ollama"); warning != "" {
		t.Fatalf("unexpected warning: %q", warning)
	}
}

// Wiping PAIR's data directory deletes the receipts but leaves the profile
// blocks, so a reinstall has to re-adopt a block it demonstrably authored.
func TestReinstallReadoptsItsOwnOrphanedProfileBlock(t *testing.T) {
	ex, _, profile := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	orphaned, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) == 0 {
		t.Fatal("install wrote no PATH block")
	}
	// Simulate the data wipe: the receipts are inside the deleted tree, the
	// shell profile is not.
	if err := os.RemoveAll(ex.pathReceiptDir()); err != nil {
		t.Fatal(err)
	}
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(orphaned) {
		t.Fatalf("reinstall duplicated the block: %q", after)
	}
	if err := ex.Uninstall(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	remaining, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("uninstall left the re-adopted block behind: %q", remaining)
	}
}

// The application uninstaller runs this before deleting the data directory the
// receipts live in.
func TestDrainReleasesThePathBlock(t *testing.T) {
	ex, _, profile := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) == 0 {
		t.Fatal("install wrote no PATH block, so the drain would prove nothing")
	}
	if err := drainUserPaths(ex.baseDir, removeShellPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("drain left a PATH block: %q", after)
	}
}

// Draining releases the entries but not the knowledge that PAIR installed the
// engine. Deleting the record outright is what made a reinstall unable to
// republish PATH for an engine whose CLI lives outside PAIR's install
// directory, because nothing else distinguishes it from an external install.
func TestDrainKeepsTheInstalledFlagSoReinstallCanReadopt(t *testing.T) {
	ex, _, _ := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if err := drainUserPaths(ex.baseDir, removeShellPath); err != nil {
		t.Fatal(err)
	}
	receipt, err := loadPathReceipt(ex.pathReceiptFile("ollama"))
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Installed {
		t.Error("drain dropped the installed flag, so a reinstall cannot re-adopt the engine")
	}
	if receipt.Dir != "" {
		t.Errorf("drain kept a PATH claim it had already released: %q", receipt.Dir)
	}
	if len(receipt.ShellBlocks) != 0 {
		t.Errorf("drain kept %d shell block claims it had already released", len(receipt.ShellBlocks))
	}
}

// Draining an engine PAIR never installed leaves nothing behind at all.
func TestDrainRemovesARecordWithNoInstallBehindIt(t *testing.T) {
	ex, _, _ := pathLifecycleExecutor(t, "ollama", "process")
	file := ex.pathReceiptFile("ollama")
	if err := savePathReceipt(file, &pathReceipt{Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := drainUserPaths(ex.baseDir, removeShellPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("drain kept a record with no installation behind it: %v", err)
	}
}

// LM Studio installs into ~/.lmstudio, outside the directory PAIR installs
// into, so an executable-location test can never recognize it as PAIR's. The
// record has to carry that fact instead: the application uninstaller releases
// every PATH entry while leaving the engine installed, and without the retained
// flag a reinstall reads the engine as an external install and never republishes
// its CLI — permanently, because PAIR also suppresses the vendor's own PATH edit.
func TestReinstallRepublishesPathForAnEngineOutsideTheInstallDirectory(t *testing.T) {
	ex, cli, _ := pathLifecycleExecutor(t, "lmstudio", "command")
	engineStateForTest(t, ex, "lmstudio").installDir = t.TempDir()
	if err := ex.Install(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	// The application uninstaller: release the entries, leave the engine.
	if err := drainUserPaths(ex.baseDir, removeShellPath); err != nil {
		t.Fatal(err)
	}
	var added string
	ex.addToPath = func(dir string, _ *pathReceipt, _ func() error) error { added = dir; return nil }
	if err := ex.Install(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	if added != filepath.Dir(cli) {
		t.Fatalf("reinstall published %q, want the engine's CLI directory %q", added, filepath.Dir(cli))
	}
}

// Declining to touch an external install is not the same as succeeding at it.
// Clearing the warning there retracts a standing report about a CLI that is
// still missing from the user's PATH.
func TestSkippingAnExternalInstallLeavesTheWarningStanding(t *testing.T) {
	ex, _, _ := pathLifecycleExecutor(t, "lmstudio", "command")
	st := engineStateForTest(t, ex, "lmstudio")
	st.installDir = t.TempDir()
	for _, path := range append([]string{}, st.plat.Detect...) {
		if err := os.WriteFile(path, []byte("external CLI"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex.reporter.report(serviceError{ID: pathFailedID("lmstudio"), Message: "an earlier attempt failed", Severity: "warning"})
	if err := ex.Install(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	if pathWarning(ex, "lmstudio") == "" {
		t.Error("a skipped external install cleared a warning it never addressed")
	}
}

// A record PAIR cannot parse is not a record of no claim. Reporting the entries
// released would strand them and clear the retry that is the only route back.
func TestUninstallWithAnUnreadableReceiptReportsFailure(t *testing.T) {
	ex, _, _ := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	file := ex.pathReceiptFile("ollama")
	if err := os.WriteFile(file, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ex.Uninstall(context.Background(), "ollama")
	if !errors.Is(err, errUnreadableReceipt) {
		t.Fatalf("uninstall returned %v, want the unreadable record to surface", err)
	}
	if _, statErr := os.Stat(file); statErr != nil {
		t.Errorf("uninstall deleted the record it could not read: %v", statErr)
	}
}

// The lock file is what carries this across processes: nvpair-tui starts its own
// broker and its own engine-manager against the same data directory, and the
// application uninstaller drains PATH from a third. Two independent handles
// stand in for those separate processes — an in-process mutex alone would let
// the second update overwrite the first.
func TestPathLockIsExclusiveAcrossHandles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "lock")
	open := func() *os.File {
		t.Helper()
		f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	held, other := open(), open()

	locked, err := tryLockExclusive(held)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("could not take a lock nothing else held")
	}
	locked, err = tryLockExclusive(other)
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		t.Error("a second handle took a lock the first already held")
	}
	if err := unlockExclusive(held); err != nil {
		t.Fatal(err)
	}
	locked, err = tryLockExclusive(other)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Error("the lock stayed held after being released")
	}
}

// The engine's operation mutex is held across this wait, so a peer that hangs
// rather than crashing must not park every later install behind it.
func TestPathLockWaitIsBounded(t *testing.T) {
	baseDir := t.TempDir()
	unlock, err := lockUserPath(baseDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// A separate handle stands in for the second process; the in-process mutex
	// is already held by this goroutine's lock, so the wait under test is the
	// file lock's.
	waited := make(chan error, 1)
	go func() {
		f, openErr := os.OpenFile(filepath.Join(pathReceiptDir(baseDir), "lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if openErr != nil {
			waited <- openErr
			return
		}
		defer f.Close()
		waited <- awaitExclusive(f, 150*time.Millisecond)
	}()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("took a lock another handle held")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait for a held lock never ended")
	}
}

// A corrupt receipt must not wedge install for that engine: the fresh claim
// written over it is the only way the file is ever repaired.
func TestUnreadableReceiptIsTreatedAsNoClaim(t *testing.T) {
	ex, cli, _ := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ex.pathReceiptFile("ollama"), []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	var added string
	ex.addToPath = func(dir string, _ *pathReceipt, _ func() error) error { added = dir; return nil }
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatalf("install refused to proceed past a corrupt receipt: %v", err)
	}
	if added != filepath.Dir(cli) {
		t.Fatalf("added %q, want %q", added, filepath.Dir(cli))
	}
	if err := ex.Uninstall(context.Background(), "ollama"); err != nil {
		t.Fatalf("uninstall refused to proceed past a corrupt receipt: %v", err)
	}
}

func TestFailedUninstallPreservesPathOwnership(t *testing.T) {
	ex, _, profile := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := ex.state("ollama")
	if err != nil {
		t.Fatal(err)
	}
	// The command returns successfully but leaves the executable in place.
	st.plat.Uninstall.Run = []string{fakeEngineBin, "echo", "still installed"}
	ex.detectTimeout = time.Millisecond
	if err := ex.Uninstall(context.Background(), "ollama"); err == nil {
		t.Fatal("expected failed uninstall")
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed uninstall changed PATH")
	}
	if _, err := os.Stat(ex.pathReceiptFile("ollama")); err != nil {
		t.Fatalf("lost ownership after failed uninstall: %v", err)
	}
}

func TestPathCleanupCanRetryAfterEngineRemoval(t *testing.T) {
	ex, cli, profile := pathLifecycleExecutor(t, "lmstudio", "command")
	if err := ex.Install(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("profile is read only")
	ex.removeFromPath = func(*pathReceipt) error { return wantErr }
	if err := ex.Uninstall(context.Background(), "lmstudio"); !errors.Is(err, wantErr) {
		t.Fatalf("uninstall error = %v", err)
	}
	if fileExists(cli) {
		t.Fatal("engine was not removed before cleanup")
	}
	ex.removeFromPath = removeShellPath
	if err := ex.Uninstall(context.Background(), "lmstudio"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 0 {
		t.Fatalf("retry left PATH block: %q", contents)
	}
}

// The published PATH and the recorded claim are asserted directly, not just the
// round trip through removal. A no-op addWindowsPath leaves next unchanged and
// the receipt empty, and removeWindowsPath returns an unclaimed PATH untouched —
// so every round trip here would still match its expectation with the append
// deleted entirely.
func TestWindowsPathOwnershipPreservesExistingEntries(t *testing.T) {
	test := func(name, current, wantPublished, wantClaim, wantAfterCleanup string) {
		t.Run(name, func(t *testing.T) {
			receipt := &pathReceipt{}
			expand := func(s string) string { return strings.ReplaceAll(s, "%ENGINE%", `C:\Engine`) }
			next, err := addWindowsPath(current, `C:\Engine`, expand, receipt, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if next != wantPublished {
				t.Errorf("published PATH = %q, want %q", next, wantPublished)
			}
			if receipt.WindowsEntry != wantClaim {
				t.Errorf("recorded claim = %q, want %q", receipt.WindowsEntry, wantClaim)
			}
			if got := removeWindowsPath(next, receipt.WindowsEntry); got != wantAfterCleanup {
				t.Errorf("after cleanup = %q, want %q", got, wantAfterCleanup)
			}
		})
	}
	test("new entry", `C:\UserTools`, `C:\UserTools;C:\Engine`, `C:\Engine`, `C:\UserTools`)
	// Appending after a trailing separator would turn a harmless trailing empty
	// element into a searched interior one, so the separator is dropped instead.
	test("trailing empty entry", `C:\UserTools;`, `C:\UserTools;C:\Engine`, `C:\Engine`, `C:\UserTools`)
	// Already on PATH: nothing is appended, and nothing is claimed. Recording an
	// entry the user put there would let uninstall delete it.
	test("existing entry", `C:\Engine;C:\UserTools`, `C:\Engine;C:\UserTools`, "", `C:\Engine;C:\UserTools`)
	test("existing case variant", `c:\ENGINE;C:\UserTools`, `c:\ENGINE;C:\UserTools`, "", `c:\ENGINE;C:\UserTools`)
	test("existing environment reference", `%ENGINE%;C:\UserTools`, `%ENGINE%;C:\UserTools`, "", `%ENGINE%;C:\UserTools`)
}

// Addition compares case-insensitively after normalizing, so removal has to as
// well: otherwise a PATH editor that re-cased or re-slashed the entry would make
// it unremovable, and uninstall would silently leave it pointing at a deleted
// directory.
func TestWindowsCleanupMatchesRespelledEntries(t *testing.T) {
	test := func(name, current, want string) {
		t.Run(name, func(t *testing.T) {
			if got := removeWindowsPath(current, `C:\Engine`); got != want {
				t.Fatalf("after cleanup = %q, want %q", got, want)
			}
		})
	}
	test("recased", `C:\UserTools;c:\engine`, `C:\UserTools`)
	test("trailing backslash", `C:\UserTools;C:\Engine\`, `C:\UserTools`)
	test("quoted", `"C:\Engine";C:\UserTools`, `C:\UserTools`)
	test("forward slashes", `C:/Engine;C:\UserTools`, `C:\UserTools`)
}

// PAIR appends, and an installer that wants precedence prepends, so a duplicate
// is somebody else's copy in front of PAIR's. Take PAIR's and leave theirs
// where they put it, rather than silently demoting a directory another tool
// deliberately promoted.
func TestWindowsCleanupRemovesTheAppendedDuplicate(t *testing.T) {
	current := `C:\Engine;C:\UserTools;C:\Engine`
	if got, want := removeWindowsPath(current, `C:\Engine`), `C:\Engine;C:\UserTools`; got != want {
		t.Fatalf("after cleanup = %q, want %q", got, want)
	}
}

func TestWindowsPathOwnershipSavedBeforeChangingPath(t *testing.T) {
	wantErr := errors.New("cannot save receipt")
	current := `C:\UserTools`
	got, err := addWindowsPath(current, `C:\Engine`, func(s string) string { return s }, &pathReceipt{}, func() error { return wantErr })
	if !errors.Is(err, wantErr) || got != current {
		t.Fatalf("PATH = %q, error = %v", got, err)
	}
}

func TestWindowsCleanupPreservesUserModifiedEntry(t *testing.T) {
	current := `C:\UserTools;%ENGINE%`
	if got := removeWindowsPath(current, `C:\Engine`); got != current {
		t.Fatalf("modified user entry was removed: %q", got)
	}
}

func TestShellOwnershipSaveFailureLeavesProfileUntouched(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	const original = "# user configuration\n"
	if err := os.WriteFile(profile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("cannot save ownership")
	err := addToShellPath(home, "sh", "", "", "/engine/bin", &pathReceipt{}, func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
	contents, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != original {
		t.Fatalf("profile changed before ownership was saved: %q", contents)
	}
}

// Once a user edits the block, PAIR can no longer prove it wrote what is there,
// so cleanup leaves it alone rather than guessing at the boundaries.
func TestShellCleanupPreservesAUserEditedBlock(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte("export PATH=\"$PATH:/engine/bin\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	receipt := &pathReceipt{}
	if err := addToShellPath(home, "sh", "", "", "/engine/bin", receipt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	before = []byte(strings.ReplaceAll(string(before), "PAIR engine", "user customized engine"))
	if err := os.WriteFile(profile, before, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeShellPath(receipt); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("cleanup modified unowned profile content")
	}
}

// Finding the exact block already present means an earlier PAIR install wrote it
// and lost the receipt. Recording ownership anyway is what lets the next
// uninstall clean it up instead of orphaning it forever.
func TestShellAddAdoptsAnIdenticalBlockItDidNotRecord(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	const userLine = "export PATH=\"$PATH:/user/tools\"\n"
	if err := os.WriteFile(profile, []byte(userLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := testAddToShellPath(home, "sh", "", "", "/engine/bin"); err != nil {
		t.Fatal(err)
	}
	orphaned, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &pathReceipt{}
	if err := addToShellPath(home, "sh", "", "", "/engine/bin", receipt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	readopted, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(readopted) != string(orphaned) {
		t.Fatalf("adoption duplicated the block: %q", readopted)
	}
	if err := removeShellPath(receipt); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != userLine {
		t.Fatalf("after cleanup = %q, want only the user's own line", after)
	}
}
