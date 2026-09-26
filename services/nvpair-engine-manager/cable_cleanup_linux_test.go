// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Only pure parsers/predicates are called. No test opens proc, lock files, or
// native descriptors. This file is cross-compiled, not executed, on Windows.
func TestCableCleanupLinuxProcParsers(t *testing.T) {
	stat := func(state, flags, start string) []byte {
		return []byte(fmt.Sprintf("200 (name with ) brackets) %s 99 0 0 0 0 %s 0 0 0 0 0 0 0 0 0 0 0 0 %s 0\n", state, flags, start))
	}
	for _, test := range []struct {
		name, state, flags, start string
		kernel, dead              bool
	}{{"ordinary", "S", "0", "123", false, false}, {"kernel", "I", "2097152", "123", true, false}, {"early-kernel", "I", "2097152", "0", true, false}, {"zombie", "Z", "0", "123", false, true}} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCableCleanupStat(stat(test.state, test.flags, test.start), 200)
			if err != nil || got.parent != 99 || got.start != test.start || got.kernel != test.kernel || got.dead != test.dead {
				t.Fatalf("wrong kernel process classification: %+v %v", got, err)
			}
		})
	}
	for _, data := range [][]byte{[]byte("200 no-fields"), stat("S", "invalid", "123"), stat("S", "0", "invalid"), []byte(strings.Replace(string(stat("S", "0", "123")), "200 ", "201 ", 1))} {
		if _, err := parseCableCleanupStat(data, 200); err == nil {
			t.Fatal("invalid process identity accepted")
		}
	}
	if uid, err := cableCleanupUID([]byte("Name:\ttest\nUid:\t1000\t1001\t1001\t1001\n")); err != nil || uid != 1001 {
		t.Fatal("effective UID was not read from kernel metadata")
	}
}

func TestCableCleanupLinuxProcCoverage(t *testing.T) {
	valid := "20 1 0:5 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw\n"
	for _, test := range []struct {
		name, body string
		valid      bool
	}{{"normal", valid, true}, {"hidepid", strings.Replace(valid, "proc rw", "proc rw,hidepid=2", 1), false}, {"subset", strings.Replace(valid, "proc rw", "proc rw,subset=pid", 1), false}, {"wrong-filesystem", strings.Replace(valid, "- proc proc", "- tmpfs tmpfs", 1), false}, {"subtree", strings.Replace(valid, " / /proc ", " /123 /proc ", 1), false}, {"duplicate", valid + valid, false}, {"missing", "", false}} {
		t.Run(test.name, func(t *testing.T) {
			if (cableCleanupProcMounts([]byte(test.body)) == nil) != test.valid {
				t.Fatal("filtered/mismatched proc view was accepted")
			}
		})
	}
}

func TestCableCleanupLinuxProtectedInode(t *testing.T) {
	base := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: 0, Nlink: 1, Size: 0}
	if !cableCleanupLockStat(base) {
		t.Fatal("protected reservation rejected")
	}
	for _, change := range []func(*unix.Stat_t){func(s *unix.Stat_t) { s.Mode = unix.S_IFLNK | 0600 }, func(s *unix.Stat_t) { s.Mode |= 0020 }, func(s *unix.Stat_t) { s.Uid = 1000 }, func(s *unix.Stat_t) { s.Nlink = 2 }, func(s *unix.Stat_t) { s.Size = 1 }} {
		stat := base
		change(&stat)
		if cableCleanupLockStat(stat) {
			t.Fatal("unprotected reservation accepted")
		}
	}
}
