// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package netmon

// macOS (and other BSD) backend: open a PF_ROUTE socket and read routing
// messages. The kernel emits RTM_NEWADDR/RTM_DELADDR/RTM_IFINFO (and route
// changes) on this socket whenever interfaces or addresses change. We treat
// any readable message as a hint and let the Monitor re-enumerate to decide
// whether the addressing actually changed.

import (
	"context"

	"golang.org/x/sys/unix"
)

func watchChanges(ctx context.Context, onEvent func()) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		poll(ctx, onEvent)
		return
	}

	go func() {
		<-ctx.Done()
		_ = unix.Close(fd)
	}()

	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := unix.Read(fd, buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err == unix.EINTR {
				continue
			}
			poll(ctx, onEvent)
			return
		}
		if n <= 0 {
			continue
		}
		onEvent()
	}
}
