// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"
)

func TestIsOurEngineImage(t *testing.T) {
	bin := filepath.Join("/opt", "nvpair", "ollama.exe")
	cases := []struct {
		image string
		want  bool
	}{
		{bin, true},
		{`/opt/nvpair/OLLAMA.EXE`, true},
		{bin + ` (deleted)`, true},
		{`/opt/nvpair/other.exe`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := isOurEngineImage(tc.image, bin); got != tc.want {
			t.Errorf("isOurEngineImage(%q, %q) = %v, want %v", tc.image, bin, got, tc.want)
		}
	}
	if isOurEngineImage(bin, ``) {
		t.Fatal("empty binPath must not match")
	}
}

func TestIsManagedInstallPath(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "ollama", "bin", "ollama"+exeExt())
	outside := filepath.Join(t.TempDir(), "ollama"+exeExt())
	if !isManagedInstallPath(inside, filepath.Join(base, "ollama")) {
		t.Fatalf("managed child path %q was rejected", inside)
	}
	if isManagedInstallPath(outside, filepath.Join(base, "ollama")) {
		t.Fatalf("external path %q was accepted", outside)
	}
	if isManagedInstallPath("", filepath.Join(base, "ollama")) {
		t.Fatal("empty binary path must not be managed")
	}
}
