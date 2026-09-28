// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"nvpair-shared/hostbootstrap"
)

func reviewedPrincipalMatchesAccount(
	principal reviewedPrincipal,
	account string,
) bool {
	resolved, err := user.Lookup(account)
	if err != nil {
		return false
	}
	uid, uidErr := strconv.ParseUint(resolved.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(resolved.Gid, 10, 32)
	return uidErr == nil &&
		gidErr == nil &&
		principal.SID == "" &&
		principal.UID == uint32(uid) &&
		principal.GID == uint32(gid)
}

func validateInitialHelperExecutable(path, expectedDigest string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return hostbootstrap.ErrHelperProtocol
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0777 != 0755 ||
		stat.Uid != 0 ||
		stat.Nlink != 1 {
		return hostbootstrap.ErrHelperProtocol
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil ||
		hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

func helperReadSecure(path string) ([]byte, error) {
	if err := validateHelperStatePath(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, hostbootstrap.ErrHelperProtocol
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0777 != 0600 ||
		stat.Uid != 0 ||
		stat.Nlink != 1 {
		return nil, hostbootstrap.ErrHelperProtocol
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxHelperStateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxHelperStateBytes {
		return nil, hostbootstrap.ErrHelperFrameTooLarge
	}
	return raw, nil
}

func validateHelperStatePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return hostbootstrap.ErrHelperProtocol
	}
	current := string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(path, current), current)
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*unix.Stat_t)
		if !ok ||
			!info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 ||
			stat.Uid != 0 ||
			info.Mode().Perm()&0022 != 0 {
			return hostbootstrap.ErrHelperProtocol
		}
		if index == len(components)-1 && info.Mode().Perm() != 0700 {
			return hostbootstrap.ErrHelperProtocol
		}
	}
	return nil
}

func helperStatePath(root, name string) string {
	return filepath.Join(root, name)
}
