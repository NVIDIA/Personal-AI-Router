// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"context"
	"errors"
)

func runPlatformService(ctx context.Context) error {
	return runHelperServer(ctx, false)
}

func runPlatformHeadlessService(context.Context) error {
	return errors.New("headless-service is supported only by Windows SCM")
}
