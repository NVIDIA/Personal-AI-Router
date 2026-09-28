// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import "context"

func vllmSystemProcessTicks(int) string { return "" }

func vllmSystemProcessInput(context.Context, []string, []byte) ([]byte, error) {
	return nil, errVLLMGroupNativeUnavailable
}
