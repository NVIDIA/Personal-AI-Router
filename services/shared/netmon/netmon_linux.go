// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package netmon

// Linux backend: open a netlink route socket and subscribe to the address and
// link multicast groups. Any RTM_NEW*/RTM_DEL* message for an address or link
// means the addressing may have changed, so we translate every readable
// message into an onEvent() and let the Monitor decide (via re-enumeration)
// whether anything actually changed.

import (
	"context"

	"golang.org/x/sys/unix"
)

func watchChanges(ctx context.Context, onEvent func()) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		poll(ctx, onEvent)
		return
	}

	addr := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_LINK,
	}
	if err := unix.Bind(fd, addr); err != nil {
		_ = unix.Close(fd)
		poll(ctx, onEvent)
		return
	}

	// Close on cancellation to unblock the blocking Read below.
	go func() {
		<-ctx.Done()
		_ = unix.Close(fd)
	}()

	buf := make([]byte, 65536)
	for {
		if ctx.Err() != nil {
			return
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err == unix.EINTR {
				continue
			}
			// Socket error (e.g. closed); fall back to polling.
			poll(ctx, onEvent)
			return
		}
		if n <= 0 {
			continue
		}
		onEvent()
	}
}
