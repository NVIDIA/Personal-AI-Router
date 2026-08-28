// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAtomicWriteCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.json")

	if err := atomicWrite(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "first\n" {
		t.Fatalf("got %q, want first", got)
	}

	if err := atomicWrite(path, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after replace: %v", err)
	}
	if string(got) != "second\n" {
		t.Fatalf("got %q, want second", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %q", e.Name())
		}
	}
}

func TestAtomicWriteRetriesTransientRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.json")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var attempts atomic.Int32
	orig := renameFile
	renameFile = func(oldpath, newpath string) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("rename: Access is denied.")
		}
		return orig(oldpath, newpath)
	}
	t.Cleanup(func() { renameFile = orig })

	if err := atomicWrite(path, []byte("new\n"), 0o600); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("rename attempts = %d, want 3", got)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new\n" {
		t.Fatalf("got %q, want new", got)
	}
}

func TestAtomicWriteNonTransientRenameFailsFast(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.json")

	var attempts atomic.Int32
	orig := renameFile
	renameFile = func(oldpath, newpath string) error {
		attempts.Add(1)
		return errors.New("rename: no such file or directory")
	}
	t.Cleanup(func() { renameFile = orig })

	err := atomicWrite(path, []byte("x\n"), 0o600)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("rename attempts = %d, want 1 (no retry)", got)
	}
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("error = %v, want wrapped non-transient cause", err)
	}
}

func TestIsTransientReplaceError(t *testing.T) {
	if !isTransientReplaceError(errors.New("Access is denied.")) {
		t.Fatal("expected Access is denied to be transient")
	}
	if !isTransientReplaceError(errors.New("The process cannot access the file because it is being used by another process.")) {
		t.Fatal("expected sharing text to be transient")
	}
	if isTransientReplaceError(errors.New("no such file or directory")) {
		t.Fatal("ENOENT must not be treated as transient")
	}
	if isTransientReplaceError(nil) {
		t.Fatal("nil must not be transient")
	}
}
