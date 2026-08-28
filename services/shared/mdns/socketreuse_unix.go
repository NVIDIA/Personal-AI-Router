// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package mdns

import "syscall"

// setReuseAddr is a net.ListenConfig.Control hook that sets SO_REUSEADDR on
// the socket before bind. mDNS requires multiple processes on one host to
// share UDP 5353; SO_REUSEADDR (the same option Go's ListenMulticastUDP and
// grandcat/zeroconf use) lets our responder coexist with the scanner,
// node-info, and any system mDNS responder (Bonjour/Avahi). We intentionally
// do not set SO_REUSEPORT — on Linux it load-balances unicast datagrams
// across the sharing sockets, which would steal unicast mDNS replies.
func setReuseAddr(network, address string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return sockErr
}
