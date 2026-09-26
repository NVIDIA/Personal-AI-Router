// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import "context"

func cableCleanupHostScope() (*cableCleanupScope, error) {
	return nil, cableCleanupFault("unsupported")
}

func nativeCableCleanupIO() cableCleanupIO {
	return cableCleanupIO{verify: func(context.Context, cableCleanupRequest) (cableCleanupScope, cableCleanupProcess, error) {
		return cableCleanupScope{}, cableCleanupProcess{}, cableCleanupFault("unsupported")
	}}
}
