// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// File-only fixture: no systemd, process, network or installed unit actions.
func TestHeadlessUpgradeRecoveryExchangePreservesConcurrentPublisher(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, headlessUnit)
	if err := os.WriteFile(unit, []byte("reviewed-original"), 0600); err != nil {
		t.Fatal(err)
	}
	exchanges := 0
	host := nativeHeadlessUpgrade{unitPath: unit, exchange: func(left, right string) error {
		exchanges++
		published := "foreign-first"
		if exchanges == 2 {
			published = "foreign-second"
		}
		// Deterministically publish in each checked-then-exchange window.
		if err := os.WriteFile(right, []byte(published), 0600); err != nil {
			return err
		}
		return unix.Renameat2(unix.AT_FDCWD, left, unix.AT_FDCWD, right, unix.RENAME_EXCHANGE)
	}}
	if err := host.replaceUnit("reviewed-original", "our-replacement"); err == nil {
		t.Fatal("racing publications were reported as a successful replacement")
	}
	if exchanges != 2 {
		t.Fatalf("recovery exchange was not exercised: %d", exchanges)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	preserved := map[string]bool{}
	for _, file := range files {
		body, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		preserved[string(body)] = true
	}
	if !preserved["foreign-first"] || !preserved["foreign-second"] {
		t.Fatalf("an unowned publication was deleted during recovery: %v", preserved)
	}
}
