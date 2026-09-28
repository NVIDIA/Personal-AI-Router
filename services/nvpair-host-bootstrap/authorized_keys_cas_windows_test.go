// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNativeAuthorizedKeysCASPublishesAndCleansOperationBackup(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "authorized_keys")
	temporary := filepath.Join(directory, "replacement")
	operationID := "abababababababababababababababab"
	original := []byte("foreign original bytes\n")
	replacement := []byte("foreign original bytes\nowned replacement\n")
	if err := os.WriteFile(destination, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishAuthorizedKeysCAS(
		temporary,
		destination,
		original,
		operationID,
	); err != nil {
		t.Fatalf("publishAuthorizedKeysCAS() error = %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(replacement) {
		t.Fatalf("destination = %q", got)
	}
	if _, err := os.Lstat(destination + ".backup-" + operationID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operation backup remains: %v", err)
	}
}

func TestAuthorizedKeysRecoveryAdoptsPublishedDestinationAndRestoresBackupOnly(t *testing.T) {
	operationID := "abababababababababababababababab"
	t.Run("destination and backup", func(t *testing.T) {
		directory := t.TempDir()
		destination := filepath.Join(directory, "authorized_keys")
		backup := destination + ".backup-" + operationID
		original := []byte("foreign original bytes\n")
		published := []byte("foreign original bytes\nowned replacement\n")
		if err := os.WriteFile(destination, published, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(backup, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := recoverAuthorizedKeysBackup(
			destination,
			operationID,
			func(displaced, current []byte) (bool, error) {
				return bytes.Equal(displaced, original) &&
					bytes.Equal(current, published), nil
			},
		); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("adopted backup remains: %v", err)
		}
	})
	t.Run("backup only", func(t *testing.T) {
		directory := t.TempDir()
		destination := filepath.Join(directory, "authorized_keys")
		backup := destination + ".backup-" + operationID
		original := []byte("foreign original bytes\n")
		if err := os.WriteFile(backup, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := recoverAuthorizedKeysBackup(
			destination,
			operationID,
			func([]byte, []byte) (bool, error) { return false, nil },
		); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("restored destination = %q", got)
		}
		if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restored backup remains: %v", err)
		}
	})
	t.Run("mismatch restores and preserves displaced content", func(t *testing.T) {
		directory := t.TempDir()
		destination := filepath.Join(directory, "authorized_keys")
		backup := destination + ".backup-" + operationID
		conflict := destination + ".conflict-" + operationID
		original := []byte("foreign original bytes\n")
		changed := []byte("foreign concurrent bytes\n")
		if err := os.WriteFile(destination, changed, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(backup, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := recoverAuthorizedKeysBackup(
			destination,
			operationID,
			func([]byte, []byte) (bool, error) { return false, nil },
		); !errors.Is(err, ErrStateIdentity) {
			t.Fatalf("recovery error = %v", err)
		}
		restored, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		preserved, err := os.ReadFile(conflict)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(restored, original) ||
			!bytes.Equal(preserved, changed) {
			t.Fatalf("restored=%q preserved=%q", restored, preserved)
		}
		if _, err := os.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("mismatched backup remains: %v", err)
		}
	})
}

func TestNativeAuthorizedKeysCASPreservesChangedDestination(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "authorized_keys")
	temporary := filepath.Join(directory, "replacement")
	concurrent := []byte("foreign concurrent bytes\n")
	if err := os.WriteFile(destination, concurrent, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("owned replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishAuthorizedKeysCAS(
		temporary,
		destination,
		[]byte("stale original\n"),
		"abababababababababababababababab",
	); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("publishAuthorizedKeysCAS() error = %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(concurrent) {
		t.Fatalf("destination changed to %q", got)
	}
}

func TestNativeAuthorizedKeysCASRefusesOpenWriterOrDeleteConflict(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "authorized_keys")
	temporary := filepath.Join(directory, "replacement")
	original := []byte("foreign original bytes\n")
	if err := os.WriteFile(destination, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("owned replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishAuthorizedKeysCAS(
		temporary,
		destination,
		original,
		"abababababababababababababababab",
	); err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("publish accepted an open writer/delete conflict")
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("destination changed to %q", got)
	}
}

func TestWindowsStatePublicationUsesWriteThroughAtomicReplace(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "pending.json")
	originalMove := windowsMoveFileEx
	defer func() { windowsMoveFileEx = originalMove }()
	called := false
	windowsMoveFileEx = func(
		source *uint16,
		target *uint16,
		flags uint32,
	) error {
		called = true
		if windows.UTF16PtrToString(target) != destination {
			t.Fatalf("target = %q", windows.UTF16PtrToString(target))
		}
		if flags != windows.MOVEFILE_REPLACE_EXISTING|
			windows.MOVEFILE_WRITE_THROUGH {
			t.Fatalf("flags = %#x", flags)
		}
		raw, err := os.ReadFile(windows.UTF16PtrToString(source))
		if err != nil {
			t.Fatalf("temporary was not closed before replace: %v", err)
		}
		if string(raw) != `{"effect":"before"}` {
			t.Fatalf("temporary = %q", raw)
		}
		return errors.New("injected replace failure")
	}
	filesystem := nativeSecureFS{
		ensureDirectory: func(string) error { return nil },
		secureFile:      func(string) error { return nil },
	}
	if err := filesystem.AtomicWriteNoFollow(
		destination,
		[]byte(`{"effect":"before"}`),
		0600,
	); err == nil {
		t.Fatal("replace failure was ignored")
	}
	if !called {
		t.Fatal("write-through replacement was not called")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed replacement published destination: %v", err)
	}
}
