// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"nvpair-shared/appdir"
)

// Trust-on-first-use pinning for manifest downloads that carry no sha256.
//
// A manifest SHOULD pin its download: the bundled Ollama and MLX manifests both
// name an immutable versioned URL and its published digest, so a changed byte
// is a failed install rather than a silent substitution. LM Studio cannot be
// pinned that way -- its installer lives at a versionless
// https://lmstudio.ai/install.sh with no published checksum, and the script is
// executed via bash -- so the choice there is between accepting whatever the
// URL serves, every time, forever, and this.
//
// TOFU does not make the first download trustworthy; nothing available can.
// What it does is convert every LATER change from invisible into an event the
// operator has to decide about. A CDN compromise or a MITM that arrives after
// the first successful install stops the install instead of running.
//
// It deliberately fails CLOSED. A vendor shipping a new installer trips it too,
// which is not a false positive: the bytes really did change, and for an
// artifact that is about to execute as the user that is worth one deliberate
// confirmation. The error names the record file, and deleting it is the
// confirmation -- no override flag, no new RPC field, and no way to click
// through it by accident.

const installerPinsFile = "installer-pins.json"

const (
	// How long to wait for another process's critical section (a read, a
	// compare and a rename -- milliseconds in practice).
	installerPinLockWait = 10 * time.Second
	// Older than this and the holder is assumed dead.
	installerPinLockStale = 2 * time.Minute
)

// installerPin is one remembered download. Version and FirstSeen are recorded
// for the human reading the file after a refusal, not for the comparison.
type installerPin struct {
	SHA256    string `json:"sha256"`
	Engine    string `json:"engine"`
	FirstSeen string `json:"firstSeen"`
}

var installerPinsMu sync.Mutex

func installerPinsPath() (string, error) { return appdir.Path(installerPinsFile) }

// loadInstallerPins reads the record. Only "the file does not exist" means "no
// pins": every other failure is reported, because treating a corrupt or
// unreadable record as an empty one is a trust reset, and one an attacker can
// cause on purpose by truncating the file.
func loadInstallerPins(path string) (map[string]installerPin, error) {
	pins := map[string]installerPin{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read installer pin record %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &pins); err != nil {
		return nil, fmt.Errorf("installer pin record %s is unreadable (%v); refusing to treat it as empty, "+
			"because that would re-trust every installer. Inspect or delete it deliberately", path, err)
	}
	return pins, nil
}

// checkInstallerPin compares an unpinned download's digest against what this
// machine saw last time. It returns an error only when a remembered digest
// disagrees; a first sighting is recorded and allowed.
func checkInstallerPin(engine, url, sum string) error {
	path, err := installerPinsPath()
	if err != nil {
		// No writable record location: fall back to the previous behaviour
		// rather than blocking installs on a directory problem. Warned, not
		// silent -- a control that quietly turns itself off is worse than one
		// that was never claimed.
		slog.Warn("installer TOFU pinning unavailable: no writable data dir; download not compared against a previous install",
			"engine", engine, "url", url, "err", err)
		return nil
	}

	installerPinsMu.Lock()
	defer installerPinsMu.Unlock()

	// The mutex above only serialises goroutines inside ONE engine-manager, and
	// this fork routinely runs two: the broker supervises one while a headless
	// command spawns another. Without a cross-process lock both can read "URL
	// absent", accept different bytes, and race to overwrite -- so first-use
	// TOFU would be defeated exactly when it matters.
	unlock, err := lockInstallerPins(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("installer pin record is busy: %w", err)
	}
	defer unlock()

	pins, err := loadInstallerPins(path)
	if err != nil {
		return err
	}
	if prev, ok := pins[url]; ok {
		if prev.SHA256 == sum {
			return nil
		}
		// Name the single entry to remove, not the file. The file holds every
		// remembered installer, so "delete it and retry" would quietly discard
		// the pins for every OTHER engine as the price of accepting one change.
		return fmt.Errorf(
			"installer for %q changed since it was first trusted on this machine.\n"+
				"  url:        %s\n"+
				"  trusted:    %s (first seen %s)\n"+
				"  served now: %s\n"+
				"This URL carries no publisher checksum, so PAIR cannot tell a vendor "+
				"release apart from a tampered download. If you have confirmed the change "+
				"is the vendor's, remove the %q entry from %s and install again "+
				"(deleting the whole file would also discard every other engine's pin).",
			engine, url, prev.SHA256, prev.FirstSeen, sum, url, path)
	}

	pins[url] = installerPin{SHA256: sum, Engine: engine, FirstSeen: time.Now().UTC().Format(time.RFC3339)}
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installer pin record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create installer pin dir: %w", err)
	}
	// Write to a sibling and rename. os.WriteFile truncates in place, so a crash
	// or a concurrent engine-manager mid-write would leave a torn file -- which
	// loadInstallerPins reads as "no pins at all", silently resetting every
	// engine's TOFU state. rename(2) is atomic within a directory, so a reader
	// sees either the old file or the new one.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".installer-pins-*")
	if err != nil {
		return fmt.Errorf("create installer pin record: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write installer pin record: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("secure installer pin record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("close installer pin record: %w", err)
	}
	// A pin that was not persisted means the next install has nothing to compare
	// against, so this install was effectively unpinned. Refuse rather than
	// execute an artifact under a control that did not actually engage.
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("record installer pin: %w", err)
	}
	return nil
}


// lockInstallerPins takes an advisory cross-process lock around the pin record.
//
// An O_EXCL lock file rather than flock(2) so the behaviour is identical on
// Windows, where engine-manager also runs and flock does not exist. The cost of
// that choice is stale locks after a crash, which is why the holder's age is
// checked: a lock older than the longest an install can plausibly hold it is
// broken rather than deadlocking every future install.
func lockInstallerPins(dir string) (func(), error) {
	lockPath := filepath.Join(dir, ".installer-pins.lock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(installerPinLockWait)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// Break a lock whose owner plainly died: the critical section is a read,
		// a compare and a rename, so anything this old is not still running.
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > installerPinLockStale {
			slog.Warn("breaking a stale installer pin lock", "path", lockPath, "age", time.Since(info.ModTime()))
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another install is holding %s", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
