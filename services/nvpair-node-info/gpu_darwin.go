// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
)

const (
	ioRegistryPath    = "/usr/sbin/ioreg"
	ioRegistryTimeout = 3 * time.Second
)

func detectGPUs() []GPUInfo {
	data, err := readDarwinIORegistry()
	if err != nil {
		log.Printf("GPU detection error: %v", err)
		return nil
	}
	gpus, err := staticGPUsFromIORegistry(data, detectMemoryTotal())
	if err != nil {
		log.Printf("GPU detection error: %v", err)
		return nil
	}
	return gpus
}

func readDarwinIORegistry() ([]byte, error) {
	return readDarwinIORegistryContext(context.Background())
}

func readDarwinIORegistryContext(parent context.Context) ([]byte, error) {
	return readDarwinIORegistryWithRunner(parent, ioRegistryTimeout, func(ctx context.Context) ([]byte, error) {
		return exec.CommandContext(
			ctx,
			ioRegistryPath,
			"-a",
			"-r",
			"-d",
			"1",
			"-c",
			"IOAccelerator",
		).Output()
	})
}

func readDarwinIORegistryWithRunner(
	parent context.Context,
	timeout time.Duration,
	run func(context.Context) ([]byte, error),
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	data, err := run(ctx)
	if err != nil {
		return nil, fmt.Errorf("query IOAccelerator: %w", err)
	}
	return data, nil
}

func staticGPUsFromIORegistry(data []byte, systemMemory uint64) ([]GPUInfo, error) {
	records, err := parseIORegistryGPUs(data, systemMemory)
	if err != nil {
		return nil, err
	}
	gpus := make([]GPUInfo, 0, len(records))
	for _, record := range records {
		gpus = append(gpus, GPUInfo{
			Name:      record.name,
			VramBytes: record.vramTotal,
			statsKey:  record.statsKey,
		})
	}
	return gpus, nil
}
