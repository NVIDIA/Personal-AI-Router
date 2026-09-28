// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLlamaWindowsShortAliasAndJunctionRefusal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ptr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	n, err := windows.GetShortPathName(ptr, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 || n >= uint32(len(buffer)) {
		t.Fatal("short-path query failed")
	}
	alias := windows.UTF16ToString(buffer[:n])
	if strings.EqualFold(alias, root) {
		t.Skip("filesystem supplied no distinct8.3 alias")
	}
	if err := validateLlamaOwnedPaths(alias); err != nil {
		t.Fatalf("ordinary8.3 alias rejected: %v", err)
	}
	t.Log("exercised a distinct native8.3 alias")

	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(root, "models")
	cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", junction, outside)
	configureSysProcAttr(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create harmless junction fixture: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.Remove(junction) })
	if err := validateLlamaOwnedPaths(alias); err == nil {
		t.Fatal("junction escape accepted through8.3 alias")
	}
	if err := validateLlamaOwnedPaths(filepath.Join(junction, "missing-child")); err == nil {
		t.Fatal("missing child hid ancestor junction")
	}
	if bytes, err := os.ReadFile(marker); err != nil || string(bytes) != "outside" {
		t.Fatal("external marker changed")
	}
}
