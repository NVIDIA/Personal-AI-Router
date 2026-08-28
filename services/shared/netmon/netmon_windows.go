// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package netmon

// Windows backend: register for IP-interface change notifications via
// iphlpapi's NotifyIpInterfaceChange. The OS invokes our callback on its own
// thread whenever an interface or address changes; we translate that into an
// onEvent() so the Monitor re-enumerates. We pass AF_UNSPEC to hear about both
// IPv4 and IPv6 changes and initialNotification=false (we already took an
// initial Snapshot in Watch).

import (
	"context"

	"golang.org/x/sys/windows"
)

func watchChanges(ctx context.Context, onEvent func()) {
	// The callback runs on an OS thread the kernel owns; onEvent only does a
	// non-blocking channel send, so it is safe to call from here.
	callback := windows.NewCallback(func(callerContext uintptr, row uintptr, notificationType uint32) uintptr {
		onEvent()
		return 0
	})

	var handle windows.Handle
	if err := windows.NotifyIpInterfaceChange(windows.AF_UNSPEC, callback, nil, false, &handle); err != nil {
		// Could not register; degrade to polling so the Monitor still works.
		poll(ctx, onEvent)
		return
	}

	// Hold the registration for the lifetime of ctx. The handle and callback
	// stay referenced so neither is collected while notifications can fire.
	<-ctx.Done()
	_ = windows.CancelMibChangeNotify2(handle)
}
