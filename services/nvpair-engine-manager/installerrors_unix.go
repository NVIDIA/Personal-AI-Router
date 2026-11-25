// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"errors"
	"syscall"
)

func isInstallConnectionReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}
