// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package ingressauth

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// fakeInfo is a fs.FileInfo whose Sys() the test controls, so ownership cases
// that would otherwise need root (a file owned by someone else) can be
// exercised.
type fakeInfo struct {
	fs.FileInfo
	sys any
}

func (f fakeInfo) Sys() any { return f.sys }

func TestOwnedByProcessUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, []byte(keyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ownedByProcessUser(real); err != nil {
		t.Fatalf("a file this process just created is refused: %v", err)
	}

	me := uint32(os.Geteuid())
	cases := []struct {
		name string
		sys  any
		ok   bool
	}{
		{"owned by the process user", &syscall.Stat_t{Uid: me}, true},
		{"owned by root", &syscall.Stat_t{Uid: 0}, true},
		{"owned by another user", &syscall.Stat_t{Uid: me + 1}, false},
		{"no ownership data", nil, false},
		{"foreign Sys type", struct{}{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ownedByProcessUser(fakeInfo{FileInfo: real, sys: tc.sys})
			if (err == nil) != tc.ok {
				t.Fatalf("ownedByProcessUser err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
