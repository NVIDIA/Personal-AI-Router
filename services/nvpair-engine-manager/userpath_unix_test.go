// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Getting ZDOTDIR wrong is not a cosmetic error: PAIR writes .zprofile and
// .zshrc into the directory this returns, so a wrong answer leaves two stray
// dotfiles the user's zsh never reads and reports the PATH entry published.
func TestZshenvDotDir(t *testing.T) {
	test := func(name, zshenv, want string) {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(zshenv), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := zshenvDotDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if want == "$HOME/.config/zsh" {
				want = filepath.Join(home, ".config", "zsh")
			}
			if got != want {
				t.Errorf("ZDOTDIR = %q, want %q", got, want)
			}
		})
	}
	test("absent assignment", "# nothing here\n", "")
	test("plain assignment", "ZDOTDIR=$HOME/.config/zsh\n", "$HOME/.config/zsh")
	test("exported assignment", "export ZDOTDIR=$HOME/.config/zsh\n", "$HOME/.config/zsh")
	test("double quoted", "export ZDOTDIR=\"$HOME/.config/zsh\"\n", "$HOME/.config/zsh")
	test("single quoted", "export ZDOTDIR='$HOME/.config/zsh'\n", "$HOME/.config/zsh")
	// zsh ends an unquoted word at the first space, so the comment is not part
	// of the directory. Treating it as one made PAIR create `zsh # keep tidy`.
	test("trailing comment", "export ZDOTDIR=$HOME/.config/zsh # keep tidy\n", "$HOME/.config/zsh")
	// A later assignment wins, as it would in zsh.
	test("reassigned", "ZDOTDIR=$HOME/first\nZDOTDIR=$HOME/.config/zsh\n", "$HOME/.config/zsh")
	// Anything PAIR cannot evaluate the way zsh would is declined rather than
	// guessed at: the fallback to $HOME is the safe answer.
	test("command substitution", "export ZDOTDIR=$(dirname /a/b)\n", "")
	test("parameter expansion", "export ZDOTDIR=${XDG_CONFIG_HOME}/zsh\n", "")
	test("relative path", "export ZDOTDIR=.config/zsh\n", "")
}

// An unreadable .zshenv is not an absent one. Falling back to $HOME on a
// permission error produces exactly the stray-dotfile outcome above, while
// reporting success.
func TestZshenvDotDirDistinguishesUnreadableFromAbsent(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		got, err := zshenvDotDir(t.TempDir())
		if err != nil {
			t.Fatalf("an absent .zshenv is not an error: %v", err)
		}
		if got != "" {
			t.Errorf("ZDOTDIR = %q, want the caller to fall back to $HOME", got)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a 0o000 file regardless of its mode")
		}
		home := t.TempDir()
		zshenv := filepath.Join(home, ".zshenv")
		if err := os.WriteFile(zshenv, []byte("export ZDOTDIR=$HOME/.config/zsh\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := zshenvDotDir(home); err == nil {
			t.Error("an unreadable .zshenv was reported as absent")
		}
	})
}
