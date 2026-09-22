// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The Unix guard must match the Windows reparse-point walk: a symbolic link is
// refused whether it sits at the leaf, at an ancestor of a leaf that does not
// exist yet, or dangles. Ordinary directories and plain missing leaves pass.
func TestLlamaUnixPathGuardWalksAncestors(t *testing.T) {
	// Resolve the fixture roots first: on macOS t.TempDir() lives under /var,
	// itself a symlink to /private/var, which must not count as a redirect the
	// fixture introduced.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}

	// (a) An ordinary managed tree is accepted, including its missing slots.
	if err := validateLlamaOwnedPaths(root); err != nil {
		t.Fatalf("ordinary tree rejected: %v", err)
	}
	// (e) A plain missing leaf beneath a real ancestor is accepted.
	if err := validateLlamaPath(filepath.Join(root, "runtime", "missing.bin")); err != nil {
		t.Fatalf("plain missing leaf rejected: %v", err)
	}

	// (b) A models slot redirected to an existing outside directory is refused.
	models := filepath.Join(root, "models")
	if err := os.Symlink(outside, models); err != nil {
		t.Skipf("cannot create symlink fixture: %v", err)
	}
	if err := validateLlamaOwnedPaths(root); err == nil {
		t.Fatal("symlinked models slot accepted")
	}
	// (d) A child that does not exist yet must not hide the symlinked ancestor.
	if err := validateLlamaPath(filepath.Join(models, "new.gguf")); err == nil {
		t.Fatal("missing leaf hid ancestor symlink")
	}
	if err := validateLlamaPath(filepath.Join(models, "nested", "deeper", "new.gguf")); err == nil {
		t.Fatal("deep missing leaf hid ancestor symlink")
	}
	if err := validateLlamaOwnedPaths(filepath.Join(models, "missing-child")); err == nil {
		t.Fatal("missing root beneath a symlink accepted")
	}

	// (c) A dangling link at the leaf is still a redirect.
	previous := filepath.Join(root, "previous")
	if err := os.Symlink(filepath.Join(outside, "gone"), previous); err != nil {
		t.Fatal(err)
	}
	if err := validateLlamaPath(previous); err == nil {
		t.Fatal("dangling symlink leaf accepted")
	}
	if err := validateLlamaPath(filepath.Join(previous, "llama")); err == nil {
		t.Fatal("missing leaf under dangling symlink accepted")
	}

	// The guard only inspects; nothing outside the managed tree may change.
	if data, err := os.ReadFile(marker); err != nil || string(data) != "outside" {
		t.Fatalf("external marker changed: %q %v", data, err)
	}
}
