// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package ingressauth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkKeyFileAccess refuses a key file that another account could read or
// change, the way sshd treats authorized_keys: a private mode is not enough if
// someone else owns the file and can change its contents or mode at will. The
// file must belong to the proxy's user or to root, since an administrator may
// provision it for a service user, and must grant nothing to group or others.
// Group read is refused even for a group the proxy belongs to: the gate cannot
// tell a group made for one workload from a shared one such as macOS "staff".
// A FileInfo without Unix ownership data is refused too: on a Unix-like system
// that is not a file the gate can vouch for.
func checkKeyFileAccess(info fs.FileInfo) error {
	return checkKeyFileAccessAs(info, os.Geteuid())
}

func checkKeyFileAccessAs(info fs.FileInfo, euid int) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine the key file's owner")
	}
	if uid := int(st.Uid); uid != euid && uid != 0 {
		return fmt.Errorf("owned by uid %d, not by the proxy's user (uid %d)", uid, euid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("permissions %04o grant access to its group or other users; chmod 600, or supply the key through NVPAIR_PROXY_API_KEYS", perm)
	}
	return nil
}
