// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"syscall"
)

func isInstallConnectionReset(err error) bool {
	// Windows sockets return the Winsock code, distinct from ECONNRESET.
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.ECONNRESET)
}
