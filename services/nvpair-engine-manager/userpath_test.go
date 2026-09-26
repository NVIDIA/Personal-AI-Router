// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendWindowsPath(t *testing.T) {
	expand := func(s string) string { return strings.ReplaceAll(s, "%USERPROFILE%", `C:\Users\Test`) }
	test := func(name, current, dir, want string) {
		t.Run(name, func(t *testing.T) {
			if got := appendWindowsPath(current, dir, expand); got != want {
				t.Fatalf("PATH = %q, want %q", got, want)
			}
		})
	}
	test("first entry", "", `C:\Engine Tools`, `C:\Engine Tools`)
	test("preserves order and references", `%USERPROFILE%\bin;C:\Tools`, `C:\Engine`, `%USERPROFILE%\bin;C:\Tools;C:\Engine`)
	test("case insensitive existing directory", `C:\TOOLS\;D:\Other`, `c:\tools`, `C:\TOOLS\;D:\Other`)
	test("existing expanded directory", `%USERPROFILE%\bin`, `C:\Users\Test\bin`, `%USERPROFILE%\bin`)
	test("existing quoted directory", `"C:\Engine Tools"`, `C:\Engine Tools`, `"C:\Engine Tools"`)
	// Windows ignores a trailing empty element but searches an interior one, and
	// has historically resolved it against the current directory.
	test("trailing separator", `C:\Tools;`, `C:\Engine`, `C:\Tools;C:\Engine`)
	test("only separators", `;;`, `C:\Engine`, `C:\Engine`)
	test("different directory with same prefix", `C:\Engine-old`, `C:\Engine`, `C:\Engine-old;C:\Engine`)
}

func TestShellPathNamesAnUnsupportedShell(t *testing.T) {
	err := testAddToShellPath(t.TempDir(), "/usr/local/bin/nu", "", "", "/opt/bin")
	if err == nil || !strings.Contains(err.Error(), "nu") {
		t.Fatalf("error = %v, want the shell named", err)
	}
}

// Every shell PAIR claims to support has to actually get a profile written.
func TestShellPathSupportsPosixShellFamilies(t *testing.T) {
	for _, shell := range []string{"sh", "dash", "ksh", "ash", "busybox"} {
		t.Run(shell, func(t *testing.T) {
			home := t.TempDir()
			if err := testAddToShellPath(home, "/bin/"+shell, "", "", "/opt/bin"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, ".profile")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShellPathProfilesPreserveContentAndAreIdempotent(t *testing.T) {
	test := func(name, shell string, profiles []string) {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			dir := "/opt/Engine Tools/bin"
			const original = "# existing user configuration\n"
			for _, profile := range profiles {
				filename := filepath.Join(home, profile)
				if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := testAddToShellPath(home, shell, "", "", dir); err != nil {
				t.Fatal(err)
			}
			first := make(map[string]string)
			for _, profile := range profiles {
				data, err := os.ReadFile(filepath.Join(home, profile))
				if err != nil {
					t.Fatal(err)
				}
				first[profile] = string(data)
				if !strings.HasPrefix(string(data), original) || !strings.Contains(string(data), dir) {
					t.Fatalf("profile content = %q", data)
				}
			}
			if err := testAddToShellPath(home, shell, "", "", dir); err != nil {
				t.Fatal(err)
			}
			for _, profile := range profiles {
				data, err := os.ReadFile(filepath.Join(home, profile))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != first[profile] {
					t.Fatalf("repeat changed %s", profile)
				}
			}
		})
	}
	test("bash login and interactive terminals", "/bin/bash", []string{".profile", ".bashrc"})
	test("bash honors existing login profile", "/bin/bash", []string{".bash_profile", ".bashrc"})
	test("zsh login and interactive terminals", "/bin/zsh", []string{".zprofile", ".zshrc"})
	test("fish configuration", "/usr/bin/fish", []string{filepath.Join(".config", "fish", "conf.d", "nvpair-path.fish")})
}

func TestShellPathUsesConfiguredProfileDirectories(t *testing.T) {
	home, zdir, config := t.TempDir(), t.TempDir(), t.TempDir()
	if err := testAddToShellPath(home, "zsh", zdir, "", "/opt/ollama/bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(zdir, ".zshrc")); err != nil {
		t.Fatal(err)
	}
	if err := testAddToShellPath(home, "fish", "", config, "/opt/lms/bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(config, "fish", "conf.d", "nvpair-path.fish")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("wrote to home instead of configured directories: %v", entries)
	}
}

func TestShellPathReportsUnwritableProfile(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".profile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testAddToShellPath(home, "bash", "", "", "/opt/bin"); err == nil {
		t.Fatal("expected a profile write failure")
	}
}

func TestShellPathPreservesLiteralDirectoriesAtRuntime(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	test := func(name, dir string) {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := testAddToShellPath(home, "sh", "", "", dir); err != nil {
				t.Fatal(err)
			}
			profile, err := os.ReadFile(filepath.Join(home, ".profile"))
			if err != nil {
				t.Fatal(err)
			}
			// Source twice to check runtime deduplication, not just file deduplication.
			command := "PATH=/existing\n" + string(profile) + string(profile) + "printf '%s' \"$PATH\""
			out, err := exec.Command(shell, "-c", command).CombinedOutput()
			if err != nil {
				t.Fatalf("source profile: %v: %s", err, out)
			}
			if string(out) != "/existing:"+dir {
				t.Fatalf("PATH = %q, want literal directory %q appended once", out, dir)
			}
		})
	}
	test("spaces", "/opt/Engine Tools/bin")
	test("quotes and shell metacharacters", "/opt/it's $(printf injected) [engine]/bin")
}

// Profile-only tests use temporary homes and do not need a persistent receipt.
func testAddToShellPath(home, shell, zdotdir, configHome, dir string) error {
	return addToShellPath(home, shell, zdotdir, configHome, dir, &pathReceipt{}, func() error { return nil })
}

// A dotfile is often a symlink into a dotfiles repository. Replacing the link
// with a regular file would detach the user's profile from the repository that
// manages it, so the write has to go through to the target.
func TestShellProfileWriteFollowsASymlink(t *testing.T) {
	home := t.TempDir()
	store := t.TempDir()
	target := filepath.Join(store, "profile")
	if err := os.WriteFile(target, []byte("# managed elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".profile")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := testAddToShellPath(home, "sh", "", "", "/opt/engine/bin"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the profile symlink was replaced by a regular file")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "/opt/engine/bin") {
		t.Errorf("the symlink target was not updated: %q", contents)
	}
	if !strings.Contains(string(contents), "# managed elsewhere") {
		t.Errorf("the symlink target lost its original contents: %q", contents)
	}
}

// The rewrite replaces the whole file from a copy read earlier, and the PAIR
// lock cannot cover the competing writer here — it is the user's editor. An
// edit that lands in the gap has to be noticed rather than discarded.
func TestShellProfileWriteRefusesToDiscardAConcurrentEdit(t *testing.T) {
	profile := filepath.Join(t.TempDir(), ".profile")
	if err := os.WriteFile(profile, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contents, before, err := readProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	edited := "original\nadded by the user's editor\n"
	if err := os.WriteFile(profile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	err = writeShellProfile(profile, append(contents, "# PAIR\n"...), before)
	if !errors.Is(err, errProfileChanged) {
		t.Fatalf("write returned %v, want the concurrent edit to be refused", err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != edited {
		t.Errorf("the user's edit was overwritten: %q", after)
	}
}
