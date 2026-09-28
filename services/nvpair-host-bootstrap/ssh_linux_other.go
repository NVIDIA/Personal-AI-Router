// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import "context"

func inspectLinuxSSHNative(
	context.Context,
	commandRunner,
) (linuxSSHInspection, bool, error) {
	return linuxSSHInspection{}, false, ErrUnsupportedTarget
}

func inspectLinuxSSHPackageNative(
	context.Context,
	commandRunner,
) (string, string, bool, bool, bool, error) {
	return "", "", false, false, false, ErrUnsupportedTarget
}
