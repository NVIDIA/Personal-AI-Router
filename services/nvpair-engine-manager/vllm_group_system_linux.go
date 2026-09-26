// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"context"
)

func vllmSystemProcessTicks(pid int) string { return diagnosticProcessTicks(pid) }

func vllmSystemProcessInput(ctx context.Context, argv []string, input []byte) ([]byte, error) {
	out, stderr, err := diagnosticProcessInputStreams(ctx, argv[0], argv[1:], []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}, bytes.NewReader(input), nil, true)
	defer clear(stderr)
	if err != nil {
		return out, classifyVLLMRankProcess(err, len(out), stderr)
	}
	return out, nil
}
