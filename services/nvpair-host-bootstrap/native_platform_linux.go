// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"nvpair-shared/hostbootstrap"
)

func inspectLinuxSSHNative(
	ctx context.Context,
	runner commandRunner,
) (linuxSSHInspection, bool, error) {
	packageStatus, packageVerify, installed, packageExact, available, err :=
		inspectLinuxSSHPackageNative(ctx, runner)
	if err != nil {
		return linuxSSHInspection{}, available, err
	}
	service, err := runner.Run(ctx, commandSpec{
		Path: "/usr/bin/systemctl",
		Args: []string{
			"show",
			"ssh",
			"--property=LoadState",
			"--property=ActiveState",
			"--property=UnitFileState",
			"--property=FragmentPath",
			"--property=DropInPaths",
			"--property=ExecStart",
		},
	})
	if err != nil {
		return linuxSSHInspection{}, false, err
	}
	inspection := parseLinuxSSHInspection(
		packageStatus,
		packageVerify,
		service,
	)
	inspection.PackageInstalled = installed
	inspection.PackageVerified = packageExact
	return inspection, true, nil
}

func inspectLinuxSSHPackageNative(
	ctx context.Context,
	runner commandRunner,
) (string, string, bool, bool, bool, error) {
	packageStatus, err := runner.Run(ctx, commandSpec{
		Path: "/usr/bin/dpkg-query",
		Args: []string{"-W", "-f=${Status}", "openssh-server"},
	})
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return "", "", false, false, true, nil
		}
		return "", "", false, false, false, err
	}
	packageVerify, err := runner.Run(ctx, commandSpec{
		Path: "/usr/bin/dpkg",
		Args: []string{"--verify", "openssh-server"},
	})
	if err != nil {
		return "", "", true, false, false, err
	}
	installed := strings.TrimSpace(packageStatus) ==
		"Status: install ok installed"
	exact := installed && strings.TrimSpace(packageVerify) == ""
	return packageStatus, packageVerify, installed, exact, true, nil
}

type nativeProcessInspector struct{}

func newNativeProcessInspector(commandRunner) processInspector {
	return nativeProcessInspector{}
}

func (nativeProcessInspector) Snapshot() ([]processRecord, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []processRecord
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil {
			continue
		}
		executable, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		close := strings.LastIndexByte(string(stat), ')')
		if close == -1 {
			continue
		}
		fields := strings.Fields(string(stat)[close+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			continue
		}
		processes = append(processes, processRecord{
			PID:  uint32(pid),
			PPID: uint32(ppid),
			Path: filepath.Clean(executable),
		})
	}
	return processes, nil
}

func currentNativeTarget() (hostbootstrap.Target, string, error) {
	architecture, err := nativeArchitecture(runtime.GOARCH)
	if err != nil {
		return hostbootstrap.Target{}, "", err
	}
	distro, err := readLinuxDistro("/etc/os-release")
	if err != nil {
		return hostbootstrap.Target{}, "", err
	}
	return hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: architecture,
	}, distro, nil
}

func readLinuxDistro(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		if filepath.Clean(target) != "/usr/lib/os-release" {
			return "", ErrUnsupportedTarget
		}
		info, err = os.Stat(path)
		if err != nil {
			return "", err
		}
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok ||
		!info.Mode().IsRegular() ||
		info.Mode().Perm()&0022 != 0 ||
		stat.Uid != 0 {
		return "", ErrUnsupportedTarget
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var identifier string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || key != "ID" {
			continue
		}
		if identifier != "" {
			return "", ErrUnsupportedTarget
		}
		identifier = strings.Trim(value, `"`)
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if identifier != "debian" && identifier != "ubuntu" {
		return "", ErrUnsupportedTarget
	}
	return identifier, nil
}

func nativeStateDirectory(value string) (string, error) {
	if value != linuxStateDirectory {
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
