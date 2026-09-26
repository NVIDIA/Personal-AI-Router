//go:build !linux

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
)

func qwen38BoundedCommand(string, []string, []string, string, vllmQwen38CommandPolicy) (string, []string, []string, error) {
	return "", nil, nil, errors.New("Qwen3.8 bounded transient-unit owner requires qualified Linux")
}

func verifyQwen38BoundedCommand(context.Context, vllmQwen38CommandPolicy) error {
	return errors.New("Qwen3.8 bounded transient-unit cleanup requires qualified Linux")
}
