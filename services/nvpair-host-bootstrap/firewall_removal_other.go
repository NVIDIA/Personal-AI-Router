// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"context"

	"nvpair-shared/hostbootstrap"
)

func linuxFirewallRemovalEffects(
	context.Context,
	commandRunner,
	hostbootstrap.Request,
) ([]nativeEffect, error) {
	return nil, ErrUnsupportedTarget
}
