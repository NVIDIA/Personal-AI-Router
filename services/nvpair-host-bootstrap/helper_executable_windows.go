// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
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
	links, err := windowsLinkCount(path)
	if err != nil ||
		links != 1 ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		isWindowsReparse(info) ||
		!hasExactStateSecurity(path) {
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
	return setExactStateSecurity(path)
}
