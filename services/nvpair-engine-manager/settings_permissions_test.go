// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSettingsOverrideRestrictsExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	e := settingsExecutor(t, false)
	if err := os.Chmod(e.overrideDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.overrideDir, "fake.json")
	if err := os.WriteFile(path, []byte(`{"engine":"fake"}`), 0644); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PAIR_TEST=private"}
	if err := e.persistRuntimeConfig("fake", 54321, nil, &environment); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{e.overrideDir: 0700, path: 0600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s permissions=%o, want %o", path, got, want)
		}
	}
}
