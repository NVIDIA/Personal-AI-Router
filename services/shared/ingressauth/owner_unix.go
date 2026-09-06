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

// ownedByProcessUser refuses a key file that belongs to another account, the
// way sshd treats authorized_keys: a private mode is not enough if someone else
// owns the file and can change its contents or mode at will. root may own the
// file, since an administrator may provision it for a service user. A
// FileInfo without Unix ownership data is refused too: on a Unix-like system
// that is not a file the gate can vouch for.
func ownedByProcessUser(info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine the key file's owner")
	}
	if uid := int(st.Uid); uid != os.Geteuid() && uid != 0 {
		return fmt.Errorf("owned by uid %d, not by the proxy's user (uid %d)", uid, os.Geteuid())
	}
	return nil
}
