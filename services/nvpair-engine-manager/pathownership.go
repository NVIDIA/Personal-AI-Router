// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A receipt is created only after PAIR installs an engine. PATH changes are
// recorded before writing them, so a failed write or process restart is retryable.
// Keep it outside the engine directory, which the vendor uninstaller deletes.
type pathReceipt struct {
	// Dir is the directory PAIR published on the user's PATH.
	Dir          string      `json:"dir,omitempty"`
	WindowsEntry string      `json:"windowsEntry,omitempty"`
	ShellBlocks  []pathBlock `json:"shellBlocks,omitempty"`
	// Installed records that PAIR ran this engine's installer, which is a
	// longer-lived fact than any individual PATH entry. It outlives the entries
	// themselves: releasing PATH when the application is uninstalled keeps this
	// flag, so a reinstall re-adopts an engine PAIR placed instead of mistaking
	// it for one the user installed. Only an engine uninstall clears it, by
	// deleting the whole record.
	//
	// The directory is not a usable substitute. An engine whose vendor installer
	// owns its location — LM Studio writes ~/.lmstudio — is indistinguishable
	// from an external install by path alone.
	Installed bool `json:"installed,omitempty"`
}

type pathBlock struct {
	Profile string `json:"profile"`
	Text    string `json:"text"`
}

// pathReceiptDir keeps ownership independent of the removable engine files.
func (e *Executor) pathReceiptDir() string {
	return pathReceiptDir(e.baseDir)
}

func pathReceiptDir(baseDir string) string {
	return filepath.Join(baseDir, "engine-path")
}

func (e *Executor) pathReceiptFile(engine string) string {
	return filepath.Join(e.pathReceiptDir(), engine+".json")
}

// errUnreadableReceipt marks a record that exists but cannot be parsed.
//
// The two directions need opposite things from it, so it is a distinct error
// rather than an empty record. Install must proceed: refusing would leave no
// way to clear the file short of the user finding and deleting it, and writing
// a fresh claim over it is both safe and self-repairing. Uninstall must not,
// because an empty record makes it skip the removal and then report the entries
// released — clearing the retry along with the warning, on evidence it never
// read. The entry outlives the only record that could identify it.
var errUnreadableReceipt = errors.New("PATH ownership record is unreadable")

// loadPathReceipt returns an empty record for a file that is absent, and
// errUnreadableReceipt alongside one for a file that is present but corrupt.
// An absent record and an unreadable one are not the same fact, and callers
// that treat them alike report success they cannot back up.
func loadPathReceipt(file string) (*pathReceipt, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return &pathReceipt{}, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt pathReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		slog.Warn("PATH ownership record is unreadable", "file", file, "err", err)
		return &pathReceipt{}, fmt.Errorf("%w: %s", errUnreadableReceipt, filepath.Base(file))
	}
	return &receipt, nil
}

// savePathReceipt atomically persists ownership before PATH is modified.
func savePathReceipt(file string, receipt *pathReceipt) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return writeJSONAtomic(file, receipt)
}

// lockUserPath serializes a whole read-modify-write cycle — the receipt, the
// registry value or shell profiles, and the receipt again — within this process
// and across every process sharing the user's data directory.
//
// One mutex is not enough: nvpair-tui starts its own broker and its own
// engine-manager, so a terminal install can run concurrently with a desktop
// install against the same HKCU\Environment\Path, the same dotfiles, and the
// same receipts. Unsynchronized, the second write discards the first, and two
// shell installs can append duplicate blocks with only one removable record.
func lockUserPath(baseDir string) (func(), error) {
	userPathMu.Lock()
	release := func() { userPathMu.Unlock() }
	dir := pathReceiptDir(baseDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		release()
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		release()
		return nil, err
	}
	if err := awaitExclusive(f, userPathLockTimeout); err != nil {
		_ = f.Close()
		release()
		return nil, err
	}
	return func() {
		_ = unlockExclusive(f)
		_ = f.Close()
		release()
	}, nil
}

// userPathLockTimeout bounds the wait for another process's PATH update.
//
// The engine's operation mutex is held across this wait, so waiting forever
// turns one wedged peer — a network home directory that stops responding, not
// a crash, which releases the lock — into every later install and uninstall
// for that engine hanging behind it. PATH work already degrades to a warning
// everywhere else, and the shell probes are bounded for the same reason.
const userPathLockTimeout = 10 * time.Second

// awaitExclusive polls rather than blocking so the wait has an end. The timeout
// is a parameter rather than a package variable the test can reassign: the
// suite runs under -race, and a global swapped around a goroutine that reads it
// is a data race waiting for the one run where the timing lines up.
func awaitExclusive(f *os.File, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; {
		locked, err := tryLockExclusive(f)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another process has held the PATH lock for over %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// pairOwnsInstall reports whether PAIR placed the engine already on disk, which
// is what entitles it to publish a PATH entry and later withdraw one.
//
// Any one of three facts establishes it:
//
//   - a recorded directory, the ordinary case;
//   - the Installed flag, which outlives the application uninstaller releasing
//     every entry, so a reinstall re-adopts what PAIR placed;
//   - an executable inside the engine's managed install directory, which covers
//     a first install whose receipt never landed.
//
// The last is not sufficient on its own. An engine whose vendor installer owns
// its location never satisfies it, so testing only that made a lost record
// permanent for exactly the engine whose vendor PATH edit PAIR suppresses.
func pairOwnsInstall(receipt *pathReceipt, st *engineState) bool {
	return receipt.Dir != "" || receipt.Installed || managedCLI(st)
}

// updateInstallPath records fresh PAIR installs and retries only owned installs.
// It returns the directory it resolved so a failure can be logged against it.
func (e *Executor) updateInstallPath(engine string, st *engineState, installedNow bool) (string, error) {
	unlock, err := lockUserPath(e.baseDir)
	if err != nil {
		return "", fmt.Errorf("lock PATH ownership: %w", err)
	}
	defer unlock()
	file := e.pathReceiptFile(engine)
	receipt, err := loadPathReceipt(file)
	// A corrupt record is no claim for install's purposes: the fresh one written
	// below replaces it, which is the only way the file ever gets repaired.
	if err != nil && !errors.Is(err, errUnreadableReceipt) {
		return "", fmt.Errorf("read PATH ownership: %w", err)
	}
	if !installedNow && !pairOwnsInstall(receipt, st) {
		// An external installation is not ours to modify, and not ours to
		// complain about either — the vendor's own installer handles its PATH.
		return "", nil
	}
	// Resolved after the external check so a vendor layout PAIR cannot publish
	// never produces a warning about a directory PAIR was never going to touch.
	dir, err := pathCLIDir(st)
	if err != nil {
		return "", err
	}
	if receipt.Dir != "" && receipt.Dir != dir {
		// The CLI moved — a manifest update, or a vendor that relocated it.
		// Reaching here means PAIR owns the recorded entry, so release it before
		// claiming the new one. Leaving it would point the user at a directory
		// that no longer holds the engine, with the record still claiming PAIR
		// put it there.
		if err := e.removeFromPath(receipt); err != nil {
			return dir, err
		}
		receipt = &pathReceipt{}
	}
	receipt.Dir = dir
	receipt.Installed = true
	save := func() error { return savePathReceipt(file, receipt) }
	if err := save(); err != nil {
		return dir, fmt.Errorf("save PATH ownership: %w", err)
	}
	return dir, e.addToPath(dir, receipt, save)
}

// uninstallPath keeps the receipt on failure so cleanup can be retried even
// when the executable has already been removed by the vendor uninstaller.
func (e *Executor) uninstallPath(engine string) error {
	unlock, lockErr := lockUserPath(e.baseDir)
	if lockErr != nil {
		return fmt.Errorf("lock PATH ownership: %w", lockErr)
	}
	defer unlock()
	file := e.pathReceiptFile(engine)
	// An unreadable record is a failure here, not an absent claim. Skipping the
	// removal and reporting the entries released would strand them and clear the
	// retry that is the only route back.
	receipt, err := loadPathReceipt(file)
	if err == nil && receipt.Dir != "" {
		err = e.removeFromPath(receipt)
		if err == nil {
			err = os.Remove(file)
		}
	}
	if err != nil {
		err = fmt.Errorf("%s is uninstalled, but its PATH entries could not be cleaned up: %w", engine, err)
		// redactHome for the same reason the install side does it: nvpair-errors
		// push-syncs this message to every peer, and the wrapped error carries
		// the absolute path of a file in this user's home directory.
		e.reporter.report(serviceError{ID: uninstallFailedID(engine), Message: redactHome(err.Error()), Severity: "error", Action: "retry", EngineType: engine, Operation: "uninstall"})
		return err
	}
	e.reporter.clear(uninstallFailedID(engine))
	e.reporter.clear(pathFailedID(engine))
	return nil
}

// removeAllUserPaths releases every PATH entry the engines under baseDir own.
//
// The receipts live inside the data directory the application uninstaller
// deletes, and nothing in HKCU\Environment or the user's dotfiles is under it.
// Without this step, uninstalling PAIR — or uninstalling it without first
// uninstalling each engine — orphans those entries with no record left to
// remove them by.
func removeAllUserPaths(baseDir string) error {
	return drainUserPaths(baseDir, removeUserPath)
}

// drainUserPaths takes the remover as a parameter so the platform the tests run
// on does not decide which half of a receipt they can check.
func drainUserPaths(baseDir string, remove func(*pathReceipt) error) error {
	unlock, err := lockUserPath(baseDir)
	if err != nil {
		return err
	}
	defer unlock()
	dir := pathReceiptDir(baseDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		receipt, err := loadPathReceipt(file)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := remove(receipt); err != nil {
			failures = append(failures, err)
			continue
		}
		// The entries are gone; the installation is not. Retaining the Installed
		// flag is what lets a later reinstall re-adopt an engine PAIR placed.
		// Deleting the record outright made that unrecoverable for an engine
		// whose CLI lives outside the managed install directory, because nothing
		// else distinguishes it from one the user installed.
		if receipt.Installed {
			if err := savePathReceipt(file, &pathReceipt{Installed: true}); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if err := os.Remove(file); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
