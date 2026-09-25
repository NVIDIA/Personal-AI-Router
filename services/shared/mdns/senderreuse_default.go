// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package mdns

import "syscall"

func setSenderReuse(network, address string, c syscall.RawConn) error {
	return setReuseAddr(network, address, c)
}
