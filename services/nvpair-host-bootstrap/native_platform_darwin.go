// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"

	"nvpair-shared/hostbootstrap"
)

type nativeProcessInspector struct {
	runner commandRunner
}

func newNativeProcessInspector(runner commandRunner) processInspector {
	return nativeProcessInspector{runner: runner}
}

func (inspector nativeProcessInspector) Snapshot() ([]processRecord, error) {
	output, err := inspector.runner.Run(context.Background(), commandSpec{
		Path: "/bin/ps",
		Args: []string{"-axo", "pid=,ppid=,comm="},
	})
	if err != nil {
		return nil, err
	}
	var processes []processRecord
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 3 {
			continue
		}
		pid, pidErr := strconv.ParseUint(fields[0], 10, 32)
		ppid, ppidErr := strconv.ParseUint(fields[1], 10, 32)
		if pidErr != nil || ppidErr != nil {
			continue
		}
		processes = append(processes, processRecord{
			PID:  uint32(pid),
			PPID: uint32(ppid),
			Path: fields[2],
		})
	}
	return processes, nil
}

func currentNativeTarget() (hostbootstrap.Target, string, error) {
	architecture, err := nativeArchitecture(runtime.GOARCH)
	if err != nil {
		return hostbootstrap.Target{}, "", err
	}
	return hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformDarwin,
		Architecture: architecture,
	}, "", nil
}

func nativeStateDirectory(value string) (string, error) {
	if value != darwinStateDirectory {
		return "", ErrUnsupportedTarget
	}
	return value, nil
}

func nativePrivileged() (bool, error) {
	return os.Geteuid() == 0, nil
}

func nativeArchitecture(value string) (hostbootstrap.Architecture, error) {
	switch value {
	case "amd64":
		return hostbootstrap.ArchitectureAMD64, nil
	case "arm64":
		return hostbootstrap.ArchitectureARM64, nil
	default:
		return "", ErrUnsupportedTarget
	}
}
