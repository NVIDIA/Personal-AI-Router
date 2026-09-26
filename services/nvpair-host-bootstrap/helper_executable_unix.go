// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func inspectHelperExecutableNative(
	path string,
	expectedDigest string,
) (helperComponentState, string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return helperComponentState{}, "", nil
	}
	if err != nil {
		return helperComponentState{}, "", err
	}
	state := helperComponentState{Present: true}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		stat.Uid != 0 ||
		stat.Nlink != 1 ||
		info.Mode().Perm() != 0755 {
		return state, "", nil
	}
	digest, present, err := hashOwnedPath(path)
	if err != nil || !present {
		return state, digest, err
	}
	state.Exact = digest == expectedDigest
	return state, digest, nil
}

func securePayloadExecutable(path string) error {
	if err := os.Chmod(path, 0755); err != nil {
		return err
	}
	return os.Chown(path, 0, 0)
}
