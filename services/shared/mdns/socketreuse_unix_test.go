// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package mdns

import (
	"context"
	"net"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// TestSetReuseAddrOptions runs the setReuseAddr Control hook against a real
// socket on an ephemeral port (never 5353, so it cannot contend with a system
// mDNS responder) and asserts the socket options it leaves behind: SO_REUSEADDR
// on every platform, and SO_REUSEPORT only on macOS.
//
// The options are read through golang.org/x/sys/unix rather than syscall
// because the syscall package does not define SO_REUSEPORT on Linux; unix
// defines it portably. macOS reports the option constant (SO_REUSEADDR = 4 on
// darwin) rather than the value we set, so "set" is any nonzero value.
func TestSetReuseAddrOptions(t *testing.T) {
	lc := net.ListenConfig{Control: setReuseAddr}
	pktConn, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer pktConn.Close()

	udpConn, ok := pktConn.(*net.UDPConn)
	if !ok {
		t.Fatalf("ListenPacket returned %T, want *net.UDPConn", pktConn)
	}
	rawConn, err := udpConn.SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}

	var reuseAddr, reusePort int
	if err := rawConn.Control(func(fd uintptr) {
		reuseAddr, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR)
		reusePort, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}

	if reuseAddr == 0 {
		t.Errorf("SO_REUSEADDR = %d, want set (nonzero)", reuseAddr)
	}
	wantPortSet := runtime.GOOS == "darwin"
	if (reusePort != 0) != wantPortSet {
		t.Errorf("SO_REUSEPORT = %d (set=%v), want set=%v on %s", reusePort, reusePort != 0, wantPortSet, runtime.GOOS)
	}
}
