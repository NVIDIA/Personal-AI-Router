// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// persistUserPath selects the installing user's shell configuration.
func persistUserPath(dir string, receipt *pathReceipt, save func() error) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	shell := loginShell()
	// Resolved only for zsh. An unreadable .zshenv has to fail rather than fall
	// back, and a bash user's install must not be the thing it fails.
	zdotdir := ""
	if filepath.Base(shell) == "zsh" {
		if zdotdir, err = zshDotDir(home); err != nil {
			return err
		}
	}
	return addToShellPath(home, shell, zdotdir, os.Getenv("XDG_CONFIG_HOME"), dir, receipt, save)
}

// removeUserPath uses recorded profiles, even if the user has changed shells.
func removeUserPath(receipt *pathReceipt) error {
	return removeShellPath(receipt)
}

// loginShell reports the shell the user actually logs in with, from the account
// database rather than $SHELL.
//
// Engine-manager is spawned by the broker, which the desktop application
// spawns, so its environment is the GUI session's. $SHELL is usually right
// there but is not guaranteed to be set at all, and writing to the wrong
// shell's profiles succeeds silently while leaving nothing on PATH.
func loginShell() string {
	u, err := user.Current()
	if err != nil {
		slog.Debug("no account record for the current user", "err", err)
		return environmentShell()
	}
	if shell := accountShell(u); shell != "" {
		return shell
	}
	return environmentShell()
}

// accountShell reads the login shell out of the passwd database. On Linux the
// file covers ordinary users; on macOS it holds only system accounts, so a real
// account has to come from Directory Services.
func accountShell(u *user.User) string {
	if runtime.GOOS == "darwin" {
		return directoryServiceShell(u.Username)
	}
	if shell := passwdFileShell("/etc/passwd", u.Username); shell != "" {
		return shell
	}
	// NSS-backed accounts (LDAP, SSSD) are absent from the file.
	getent, err := exec.LookPath("getent")
	if err != nil {
		return ""
	}
	return passwdEntryShell(probeAccountDatabase(getent, "passwd", u.Username), u.Username)
}

func directoryServiceShell(username string) string {
	out := probeAccountDatabase("/usr/bin/dscl", ".", "-read", "/Users/"+username, "UserShell")
	_, value, ok := strings.Cut(out, ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// accountProbeTimeout bounds a single account-database lookup.
//
// getent blocks on an unreachable LDAP or SSSD backend and dscl on a directory
// server that is not answering, both of which are ordinary on a laptop off the
// corporate network. This runs inside an install whose PATH step is supposed to
// degrade to a warning, so a hung probe has to become a miss rather than an
// install that never returns.
const accountProbeTimeout = 3 * time.Second

// probeAccountDatabase returns the command's trimmed output, or "" if it fails
// or outlasts its budget. The caller falls back to the environment.
func probeAccountDatabase(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), accountProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Killing the probe closes the pipe Output is reading only if nothing it
	// spawned still holds the other end. WaitDelay caps that too, so the
	// deadline is on returning rather than on the process alone.
	cmd.WaitDelay = accountProbeTimeout
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("account database lookup failed", "command", name, "err", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

func passwdFileShell(file, username string) string {
	f, err := os.Open(file)
	if err != nil {
		slog.Debug("could not read the passwd database", "file", file, "err", err)
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if shell := passwdEntryShell(scanner.Text(), username); shell != "" {
			return shell
		}
	}
	// "Not listed" and "stopped looking" are different facts. The caller falls
	// back either way, but only one of them explains a profile written to the
	// wrong shell, so it has to reach the log.
	if err := scanner.Err(); err != nil {
		slog.Debug("stopped reading the passwd database early", "file", file, "err", err)
	}
	return ""
}

// passwdEntryShell returns field 7 of a passwd line belonging to username.
func passwdEntryShell(line, username string) string {
	fields := strings.Split(line, ":")
	if len(fields) < 7 || fields[0] != username {
		return ""
	}
	return strings.TrimSpace(fields[6])
}

// environmentShell is the last resort when the account database is unreadable.
func environmentShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	if runtime.GOOS == "darwin" {
		return "zsh"
	}
	return "bash"
}

// zshDotDir resolves where zsh looks for .zprofile and .zshrc.
//
// ZDOTDIR is conventionally assigned in ~/.zshenv, which zsh reads before
// anything else, so it is essentially never visible in this process's inherited
// environment. Missing it writes two stray dotfiles into $HOME that the user's
// zsh never reads, and reports success.
func zshDotDir(home string) (string, error) {
	if dir := os.Getenv("ZDOTDIR"); dir != "" {
		return dir, nil
	}
	return zshenvDotDir(home)
}

// zshenvDotDir scans ~/.zshenv for a plain ZDOTDIR assignment. Only a literal
// value is honoured: anything built from a command substitution or a conditional
// cannot be evaluated here, and guessing at it would be worse than falling back
// to $HOME.
func zshenvDotDir(home string) (string, error) {
	data, err := os.ReadFile(filepath.Join(home, ".zshenv"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		// Falling back to $HOME on a permission or I/O error would write two
		// dotfiles the user's zsh never reads and report success — the exact
		// outcome this function exists to prevent, on evidence it never got.
		return "", err
	}
	found := ""
	for _, line := range strings.Split(string(data), "\n") {
		statement := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		value, ok := strings.CutPrefix(statement, "ZDOTDIR=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		} else if cut := strings.IndexAny(value, " \t"); cut >= 0 {
			// Unquoted, so zsh ends the word at the first space and the rest is
			// a trailing comment. Without this, `ZDOTDIR=$HOME/.config/zsh # tidy`
			// cleared every guard below and PAIR created a directory named
			// `zsh # tidy`.
			value = value[:cut]
		}
		value = expandHomePrefix(home, value)
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "$`(){}") {
			continue
		}
		found = value // A later assignment wins, as it would in zsh.
	}
	return found, nil
}

// expandHomePrefix resolves the only references a static read can resolve.
func expandHomePrefix(home, value string) string {
	for _, prefix := range []string{"$HOME/", "${HOME}/", "~/"} {
		if rest, ok := strings.CutPrefix(value, prefix); ok {
			return filepath.Join(home, rest)
		}
	}
	return value
}
