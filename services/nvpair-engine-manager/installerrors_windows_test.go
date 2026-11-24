// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLMSInstallFailure_ClassifiesWindowsSocketReset(t *testing.T) {
	err := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "wsarecv", Err: syscall.WSAECONNRESET}}
	require.Equal(t, "network", classifyLMSInstallFailure(err))
}
