// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type nativeHeadlessUpgrade struct {
	unitPath string
	exchange func(string, string) error // Optional deterministic file-race test seam.
}

func (h nativeHeadlessUpgrade) exchangeUnit(left, right string) error {
	if h.exchange != nil {
		return h.exchange(left, right)
	}
	return unix.Renameat2(unix.AT_FDCWD, left, unix.AT_FDCWD, right, unix.RENAME_EXCHANGE)
}

func (h nativeHeadlessUpgrade) readUnit() (string, error) {
	data, err := readOwnedHeadlessUnit(h.unitPath)
	return string(data), err
}

func (h nativeHeadlessUpgrade) systemctl(ctx context.Context, action string) error {
	args := []string{action}
	if action != "daemon-reload" {
		args = append(args, headlessUnit)
	}
	_, err := headlessSystemctl(ctx, args...)
	return err
}

func (h nativeHeadlessUpgrade) state(ctx context.Context, executable string) (string, error) {
	args := []string{"show", headlessUnit}
	for _, field := range headlessUnitStateFields {
		args = append(args, "--property="+field)
	}
	raw, err := headlessSystemctl(ctx, args...)
	if err != nil {
		return "", err
	}
	state, pid, group, err := parseHeadlessUnitState(raw, h.unitPath)
	if err != nil {
		return "", err
	}
	if pid > 0 {
		actual, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
		expected, expectedErr := os.Stat(executable)
		if err != nil || expectedErr != nil || !os.SameFile(actual, expected) {
			return "", errors.New("headless unit is running a different executable")
		}
		return state, nil
	}
	if group != "" {
		if !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
			return "", errors.New("headless unit control group is unconfirmed")
		}
		file, err := os.Open(filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(group, "/"), "cgroup.procs"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("headless child termination could not be confirmed")
		}
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(file, 65537))
			_ = file.Close()
			if readErr != nil || len(body) > 65536 || strings.TrimSpace(string(body)) != "" {
				return "", errors.New("headless unit still owns processes; replacement was held")
			}
		}
	}
	return "stopped", nil
}

// Exchange only this reviewed fixed unit. If another writer changed it during
// the exchange, restore that writer's bytes and retain any unresolved evidence.
func (h nativeHeadlessUpgrade) replaceUnit(expected, replacement string) error {
	if len(replacement) > 8192 {
		return errors.New("replacement unit exceeds its supported size")
	}
	current, err := h.readUnit()
	if err != nil || current != expected {
		return errors.New("headless unit changed before replacement")
	}
	file, err := os.CreateTemp(filepath.Dir(h.unitPath), ".pair-unit-upgrade-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, err = file.WriteString(replacement); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	current, err = h.readUnit()
	if err != nil || current != expected {
		return errors.New("headless unit changed before atomic replacement")
	}
	if err = h.exchangeUnit(temporary, h.unitPath); err != nil {
		return errors.New("atomic headless unit exchange is unavailable; no fallback replacement was attempted")
	}
	removeTemporary = false
	displaced, displacedErr := readOwnedHeadlessUnit(temporary)
	if displacedErr != nil || string(displaced) != expected {
		if now, readErr := h.readUnit(); readErr == nil && now == replacement {
			// Another publisher may race even this recovery exchange. Retain
			// the temporary: its newly displaced entry is not proven ours.
			_ = h.exchangeUnit(temporary, h.unitPath)
		}
		return errors.New("unit changed during replacement; foreign bytes were preserved and completion is unconfirmed")
	}
	removeTemporary = true
	dir, err := os.Open(filepath.Dir(h.unitPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func runHeadlessUpgrade(ctx context.Context, request headlessUpgradeRequest, brokerOverride, lifetime string) (any, error) {
	if lifetime != "persistent" && lifetime != "session" {
		return nil, errors.New("startup lifetime must be persistent or session")
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	broker, err := resolveBrokerPath(brokerOverride)
	if err != nil {
		return nil, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, err
	}
	broker, err = filepath.Abs(broker)
	if err != nil {
		return nil, err
	}
	plan, err := planHeadlessUpgrade(request, executable, broker, config)
	if err != nil {
		return nil, err
	}
	unitDir := filepath.Join(config, "systemd", "user")
	real, err := filepath.EvalSymlinks(unitDir)
	st, statErr := os.Stat(unitDir)
	if err != nil || statErr != nil || real != unitDir || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
		return nil, errors.New("reviewed unit directory is redirected or not private")
	}
	identity, ok := st.Sys().(*syscall.Stat_t)
	if !ok || identity.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("reviewed unit directory belongs to another account")
	}
	lockPath := filepath.Join(unitDir, ".pair-headless-upgrade.lock")
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("headless upgrade lock is unavailable")
	}
	defer unix.Close(fd)
	var lockStat unix.Stat_t
	if unix.Fstat(fd, &lockStat) != nil || lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Mode&0077 != 0 || lockStat.Uid != uint32(os.Geteuid()) || lockStat.Nlink != 1 {
		return nil, errors.New("headless upgrade lock ownership is invalid")
	}
	if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return nil, errors.New("another product upgrade is already operating on this headless unit")
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	return executeHeadlessUpgrade(ctx, plan, nativeHeadlessUpgrade{unitPath: filepath.Join(unitDir, headlessUnit)})
}
