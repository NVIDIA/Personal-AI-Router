// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

func diagnosticProfileOwnership(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("diagnostic profiles must be regular files")
	}
	return nil // Controller-only on these platforms; native participant admission remains Linux-only.
}

func diagnosticManagedTrustCurrentUID() (int, bool)         { return 0, false }
func diagnosticManagedTrustFileUID(os.FileInfo) (int, bool) { return 0, false }
func diagnosticManagedTrustOpen(string) (*os.File, error) {
	return nil, errDiagnosticManagedTrustPlatform
}

func diagnosticProcess(context.Context, string, []string, []string, func(*exec.Cmd) error) ([]byte, error) {
	return nil, errors.New("NCCL process execution is available only on Linux")
}
func diagnosticProcessInput(context.Context, string, []string, []string, io.Reader, func(*exec.Cmd) error) ([]byte, error) {
	return nil, errors.New("NCCL prerequisite processes execute only on Linux")
}
func diagnosticSameProcess(int, string) bool { return false }
func diagnosticLock(context.Context, string) (func(), error) {
	return nil, errors.New("NCCL rank admission locks are available only on Linux")
}
func diagnosticCancelRank(context.Context, string) (bool, error) {
	return false, errors.New("NCCL rank cleanup is available only on Linux")
}
func diagnosticRankMain(string, string) int { return 2 }
