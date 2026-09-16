// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package discovery

import "syscall"

// setReuseAddr is the macOS counterpart of the Unix build. Besides SO_REUSEADDR
// it also sets SO_REUSEPORT before bind. macOS's system mDNSResponder already
// holds UDP 5353; without SO_REUSEPORT our bind races with it and
// intermittently fails with "address already in use". SO_REUSEADDR is still set
// so multicast-group sharing with the PAIR responder and sibling processes works
// as on other platforms. The net package happens to set the same options itself
// for a multicast listen address, but the hook makes that dependency explicit
// here rather than incidental.
func setReuseAddr(network, address string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		if sockErr == nil {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
		}
	}); err != nil {
		return err
	}
	return sockErr
}
