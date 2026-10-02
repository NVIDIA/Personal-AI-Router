// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Keep profile editing independent of the host OS so tests can use temporary
// homes on every platform, without touching the developer's shell configuration.
func addToShellPath(home, shell, zdotdir, configHome, dir string, receipt *pathReceipt, save func() error) error {
	quote := "'" + strings.ReplaceAll(dir, "'", "'\"'\"'") + "'"
	block := "\n# PAIR engine command-line tools\ncase \":$PATH:\" in\n    *:" + quote + ":*) ;;\n    *) export PATH=\"${PATH:+$PATH:}\"" + quote + " ;;\nesac\n"
	var profiles []string
	switch filepath.Base(shell) {
	case "zsh":
		if zdotdir == "" {
			zdotdir = home
		}
		profiles = []string{filepath.Join(zdotdir, ".zprofile"), filepath.Join(zdotdir, ".zshrc")}
	case "bash":
		login := filepath.Join(home, ".profile")
		for _, name := range []string{".bash_profile", ".bash_login"} {
			candidate := filepath.Join(home, name)
			if _, err := os.Stat(candidate); err == nil {
				login = candidate
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		profiles = []string{login, filepath.Join(home, ".bashrc")}
	case "fish":
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		profiles = []string{filepath.Join(configHome, "fish", "conf.d", "nvpair-path.fish")}
		quote = "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(dir) + "'"
		block = "\n# PAIR engine command-line tools\nif not contains -- " + quote + " $PATH\n    set -gx PATH $PATH " + quote + "\nend\n"
	case "sh", "dash", "ksh", "ash", "busybox":
		profiles = []string{filepath.Join(home, ".profile")}
	default:
		// tcsh, nushell, elvish, xonsh and friends each need their own syntax.
		// The caller reports this as a warning with the directory to add, so an
		// unrecognized shell costs the user one manual step, not the install.
		return fmt.Errorf("automatic PATH setup is not supported for the %s shell", filepath.Base(shell))
	}
	for _, profile := range profiles {
		profile, err := filepath.Abs(profile)
		if err != nil {
			return err
		}
		if err := appendPathBlock(profile, block, receipt, save); err != nil {
			return fmt.Errorf("update %s: %w", filepath.Base(profile), err)
		}
	}
	return nil
}

// appendPathBlock records the snippet as ours before writing it.
//
// Ownership is recorded even when the profile already carries a byte-identical
// block, because PAIR is demonstrably the author: a reinstall after the receipt
// was lost — wiping the data directory deletes it — must be able to re-adopt
// the block, or the next uninstall reports success while leaving it behind.
func appendPathBlock(profile, block string, receipt *pathReceipt, save func() error) error {
	// Recorded before the profile is read rather than between reading it and
	// rewriting it. save() is two fsyncs, and every one of them sat inside the
	// window in which a concurrent editor's save to the same profile would be
	// discarded by the rewrite below.
	owned := pathBlock{Profile: profile, Text: block}
	recorded := false
	for _, previous := range receipt.ShellBlocks {
		recorded = recorded || previous == owned
	}
	if !recorded {
		receipt.ShellBlocks = append(receipt.ShellBlocks, owned)
		if err := save(); err != nil {
			return err
		}
	}
	contents, before, err := readProfile(profile)
	if err != nil {
		return err
	}
	if strings.Contains(string(contents), block) {
		return nil
	}
	return writeShellProfile(profile, append(contents, block...), before)
}

// removeShellPath removes exact recorded snippets and preserves other content.
func removeShellPath(receipt *pathReceipt) error {
	for _, block := range receipt.ShellBlocks {
		contents, before, err := readProfile(block.Profile)
		if err != nil {
			// Named by basename: this message reaches cluster peers.
			return fmt.Errorf("read %s: %w", filepath.Base(block.Profile), err)
		}
		// A user-edited block is no longer ours to delete.
		if block.Text == "" || !strings.Contains(string(contents), block.Text) {
			continue
		}
		next := strings.Replace(string(contents), block.Text, "", 1)
		if err := writeShellProfile(block.Profile, []byte(next), before); err != nil {
			return fmt.Errorf("update %s: %w", filepath.Base(block.Profile), err)
		}
	}
	return nil
}

// profileSnapshot fingerprints a profile so a rewrite can tell whether anything
// else changed it since it was read.
type profileSnapshot struct {
	size    int64
	modTime time.Time
	absent  bool
}

func statProfile(profile string) (profileSnapshot, error) {
	info, err := os.Stat(profile)
	if errors.Is(err, os.ErrNotExist) {
		return profileSnapshot{absent: true}, nil
	}
	if err != nil {
		return profileSnapshot{}, err
	}
	return profileSnapshot{size: info.Size(), modTime: info.ModTime()}, nil
}

// errProfileChanged reports that someone else wrote to the profile mid-update.
// Both callers surface it as a retryable failure, which is the right answer:
// the next attempt reads the user's new contents and appends to those.
var errProfileChanged = errors.New("the file changed while PAIR was updating it")

// readProfile fingerprints a profile before reading it. Taking the stat first
// means a write that races the read fails the later comparison instead of
// passing it — the worst case is a spurious retry, never a silent overwrite.
func readProfile(profile string) ([]byte, profileSnapshot, error) {
	before, err := statProfile(profile)
	if err != nil {
		return nil, profileSnapshot{}, err
	}
	contents, err := os.ReadFile(profile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, profileSnapshot{}, err
	}
	return contents, before, nil
}

// writeShellProfile replaces a profile in one step: write a sibling temporary
// file, flush it, then rename it over the original. Both directions go through
// here. An append cannot use O_APPEND, because a short write — ENOSPC, a quota —
// would leave the user a profile truncated mid-statement that every new shell
// then fails to parse, and that PAIR could neither match nor repair afterwards.
// The flush matters for the same reason: rename swaps the directory entry
// atomically but says nothing about the data reaching stable storage, and PAIR
// did not create these files so it has no copy to restore.
//
// Symlinks are resolved so a dotfile linked into a dotfiles repository is
// updated in place rather than replaced by a regular file.
//
// The cost is inode identity: the replacement carries the permission bits over
// but not ACLs, extended attributes, or additional hard links. That is accepted
// deliberately. Preserving them means writing through the original inode, which
// is exactly the short-write hazard above, and the alternative — copying each
// class of metadata — is platform-specific, incomplete, and guards a dotfile
// arrangement far rarer than a full disk. Do not turn this back into an
// in-place write.
//
// since is the fingerprint readProfile took, re-checked immediately before the
// rename. Replacing the whole file from a copy read earlier would discard an
// edit that landed in between, and the PAIR lock cannot prevent that one — the
// competing writer is the user's editor, not another engine-manager.
func writeShellProfile(profile string, contents []byte, since profileSnapshot) error {
	target := profile
	perm := os.FileMode(0o644)
	switch resolved, err := filepath.EvalSymlinks(profile); {
	case err == nil:
		info, statErr := os.Stat(resolved)
		if statErr != nil {
			return statErr
		}
		target, perm = resolved, info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pair-path-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := writeAndSync(f, contents, perm); err != nil {
		return err
	}
	switch now, err := statProfile(profile); {
	case err != nil:
		return err
	case now != since:
		return fmt.Errorf("%w: %s", errProfileChanged, filepath.Base(profile))
	}
	if err := os.Rename(f.Name(), target); err != nil {
		return err
	}
	return syncDir(dir)
}

// writeAndSync fills an open file and closes it durably, so the caller's rename
// publishes the bytes rather than an empty file.
func writeAndSync(f *os.File, contents []byte, perm os.FileMode) error {
	if err := f.Chmod(perm); err != nil {
		return errors.Join(err, f.Close())
	}
	if _, err := f.Write(contents); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}
