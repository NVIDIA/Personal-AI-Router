// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func acquireBootstrapOperationLock(root string) (func() error, error) {
	if err := (nativeSecureFS{}).EnsurePrivateDirectory(root); err != nil {
		return nil, err
	}
	dirfd, err := unix.Open(
		root,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, err
	}
	created := false
	fd, err := unix.Openat(
		dirfd,
		"operation.lock",
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL,
		0600,
	)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(
			dirfd,
			"operation.lock",
			unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
	} else if err == nil {
		created = true
	}
	if err != nil {
		_ = unix.Close(dirfd)
		return nil, err
	}
	if created {
		if err := unix.Fchmod(fd, 0600); err != nil {
			_ = unix.Close(fd)
			_ = unix.Close(dirfd)
			return nil, err
		}
		if err := unix.Fchown(fd, 0, 0); err != nil {
			_ = unix.Close(fd)
			_ = unix.Close(dirfd)
			return nil, err
		}
		if err := unix.Fsync(dirfd); err != nil {
			_ = unix.Close(fd)
			_ = unix.Close(dirfd)
			return nil, err
		}
	}
	_ = unix.Close(dirfd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0777 != 0600 ||
		stat.Uid != 0 ||
		stat.Nlink != 1 {
		_ = unix.Close(fd)
		return nil, ErrUnsafeState
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		return nil, ErrStateIdentity
	}
	file := os.NewFile(uintptr(fd), "operation.lock")
	if file == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
		return nil, ErrUnsafeState
	}
	return func() error {
		unlockErr := unix.Flock(fd, unix.LOCK_UN)
		closeErr := file.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}
