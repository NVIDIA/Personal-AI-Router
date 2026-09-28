// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"time"
)

type vllmQwen38CommandPolicy struct {
	StageRoot, BundleRoot, Unit                     string
	Timeout                                         time.Duration
	MemoryMaxBytes, StageMaxBytes, FreeReserveBytes uint64
	TasksMax                                        int
}

func runQwen38BoundedCommand(ctx context.Context, bin string, args, childEnv []string, dir string, policy vllmQwen38CommandPolicy) ([]byte, error) {
	wrapper, wrapperArgs, wrapperEnv, err := qwen38BoundedCommand(bin, args, childEnv, dir, policy)
	if err != nil {
		return nil, err
	}
	output, runErr := runManagedVLLMCommand(ctx, dir, wrapper, wrapperArgs, wrapperEnv)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return output, errors.Join(runErr, verifyQwen38BoundedCommand(cleanupCtx, policy))
}
