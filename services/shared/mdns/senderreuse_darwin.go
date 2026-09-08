// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package mdns

import "syscall"

// Darwin requires SO_REUSEPORT in addition to SO_REUSEADDR when a
// unicast-bound sender shares UDP 5353 with the system mDNS responder. Go sets
// the same pair automatically for multicast-address listeners on BSD systems.
func setSenderReuse(network, address string, c syscall.RawConn) error {
	if err := setReuseAddr(network, address, c); err != nil {
		return err
	}
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
