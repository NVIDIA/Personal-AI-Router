// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"

	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStaticGPUsFromIORegistry(t *testing.T) {
	const systemMemory = uint64(36 << 30)
	gpus, err := staticGPUsFromIORegistry([]byte(ioRegistryGPUFixture), systemMemory)
	require.NoError(t, err, "staticGPUsFromIORegistry() error")
	require.Len(t, gpus, 2)

	apple := gpus[0]
	require.Equal(t, "Apple M3 Max", apple.Name, "unexpected Apple GPU (%v)", apple)
	require.True(t, apple.VramBytes == systemMemory, "unexpected Apple GPU (%v)", apple)
	require.Equal(t, "ioreg:2a", apple.statsKey, "unexpected Apple GPU (%v)", apple)
	require.Equal(t, uint64(0), apple.VramUsedBytes, "static detection published dynamic fields (%v)", apple)
	require.Equal(t, uint32(0), apple.UtilizationPercent, "static detection published dynamic fields (%v)", apple)

	discrete := gpus[1]
	require.Equal(t, "AMD Radeon Pro", discrete.Name, "unexpected discrete GPU (%v)", discrete)
	require.True(t, discrete.VramBytes == 8<<30, "unexpected discrete GPU (%v)", discrete)
	require.Equal(t, "ioreg:63", discrete.statsKey, "unexpected discrete GPU (%v)", discrete)
}

func TestReadDarwinIORegistryTimeout(t *testing.T) {
	_, err := readDarwinIORegistryWithRunner(context.Background(), time.Millisecond, func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	require.ErrorIs(t, err, context.DeadlineExceeded, "error")
}
