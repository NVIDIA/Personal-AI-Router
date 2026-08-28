// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStaticGPUsFromIORegistry(t *testing.T) {
	const systemMemory = uint64(36 << 30)
	gpus, err := staticGPUsFromIORegistry([]byte(ioRegistryGPUFixture), systemMemory)
	if err != nil {
		t.Fatalf("staticGPUsFromIORegistry() error = %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("got %d GPUs, want 2", len(gpus))
	}

	apple := gpus[0]
	if apple.Name != "Apple M3 Max" || apple.VramBytes != systemMemory ||
		apple.statsKey != "ioreg:2a" {
		t.Fatalf("unexpected Apple GPU: %+v", apple)
	}
	if apple.VramUsedBytes != 0 || apple.UtilizationPercent != 0 {
		t.Fatalf("static detection published dynamic fields: %+v", apple)
	}

	discrete := gpus[1]
	if discrete.Name != "AMD Radeon Pro" || discrete.VramBytes != 8<<30 ||
		discrete.statsKey != "ioreg:63" {
		t.Fatalf("unexpected discrete GPU: %+v", discrete)
	}
}

func TestReadDarwinIORegistryTimeout(t *testing.T) {
	_, err := readDarwinIORegistryWithRunner(context.Background(), time.Millisecond, func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
}
