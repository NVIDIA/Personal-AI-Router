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

// fakeInfo is a fs.FileInfo whose mode and Sys() the test controls, so
// ownership cases that would otherwise need root (a file owned by someone
// else, or a root-owned group-readable Secret mount) can be exercised.
type fakeInfo struct {
	fs.FileInfo
	mode fs.FileMode
	sys  any
}

func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (f fakeInfo) Sys() any          { return f.sys }

func TestCheckKeyFileAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, []byte(keyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	real, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkKeyFileAccess(real); err != nil {
		t.Fatalf("a file this process just created is refused: %v", err)
	}

	const me, other, group = 1000, 1001, 2000
	cases := []struct {
		name string
		mode fs.FileMode
		sys  any
		ok   bool
	}{
		{"owned by the process user", 0o600, &syscall.Stat_t{Uid: me}, true},
		{"owned by root", 0o400, &syscall.Stat_t{Uid: 0}, true},
		{"owned by another user", 0o600, &syscall.Stat_t{Uid: other}, false},
		{"world-readable", 0o604, &syscall.Stat_t{Uid: me}, false},
		{"group-readable", 0o640, &syscall.Stat_t{Uid: me, Gid: group}, false},
		// A Kubernetes Secret under a pod fsGroup: the group might be the
		// workload's alone or a shared one; the gate cannot tell, so refuses.
		{"root-owned, group-readable", 0o440, &syscall.Stat_t{Uid: 0, Gid: group}, false},
		{"no ownership data", 0o600, nil, false},
		{"foreign Sys type", 0o600, struct{}{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkKeyFileAccessAs(fakeInfo{FileInfo: real, mode: tc.mode, sys: tc.sys}, me)
			if (err == nil) != tc.ok {
				t.Fatalf("checkKeyFileAccessAs err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
