// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"context"

	"nvpair-shared/hostbootstrap"
)

func probeLinuxFirewallNative(
	context.Context,
	commandRunner,
	hostbootstrap.Request,
) (nativeProbe, error) {
	return nativeProbe{}, ErrUnsupportedTarget
}

func linuxFirewallNativeEffects(
	context.Context,
	commandRunner,
	hostbootstrap.Request,
) ([]nativeEffect, error) {
	return nil, ErrUnsupportedTarget
}

func linuxFirewallOwnedFamiliesNative(
	hostbootstrap.Request,
) ([]string, error) {
	return nil, ErrUnsupportedTarget
}

func removeLinuxFirewallNative(
	context.Context,
	commandRunner,
	hostbootstrap.Request,
) error {
	return ErrUnsupportedTarget
}
