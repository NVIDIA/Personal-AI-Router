// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrivateLaunchCommand_ReportsMetalMemoryFailureAfterNonzeroExit(t *testing.T) {
	err := runPrivateLaunchCommand(t.Context(), []string{fakeEngineBin, "--fail-metal-oom"}, nil, nil)

	require.ErrorContains(t, err, "Not enough available GPU memory")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
}

func TestPrivateLaunchCommand_PreservesCancellationAfterMetalOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	err := runPrivateLaunchUntilInterrupted(t, ctx, cancel)

	assert.ErrorIs(t, err, context.Canceled)
	var exitErr *exec.ExitError
	assert.ErrorAs(t, err, &exitErr)
	assert.NotContains(t, err.Error(), "Not enough available GPU memory")
}

func TestPrivateLaunchCommand_PreservesDeadlineAfterMetalOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)

	err := runPrivateLaunchUntilInterrupted(t, ctx, func() { <-ctx.Done() })

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	var exitErr *exec.ExitError
	assert.ErrorAs(t, err, &exitErr)
	assert.NotContains(t, err.Error(), "Not enough available GPU memory")
}

func runPrivateLaunchUntilInterrupted(t *testing.T, ctx context.Context, interrupt func()) error {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "startup-ready")
	result := make(chan error, 1)
	go func() {
		result <- runPrivateLaunchCommand(ctx, []string{fakeEngineBin, "--metal-oom-before-wait"},
			map[string]string{"PAIR_TEST_STARTUP_MARKER": marker}, nil)
	}()
	waitFor(t, 4*time.Second, func() bool {
		_, err := os.Stat(marker)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		require.NoError(t, err)
		return true
	})
	interrupt()
	select {
	case err := <-result:
		require.Error(t, err)
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted startup command did not exit")
		return nil
	}
}
