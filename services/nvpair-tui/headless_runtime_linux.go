// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type headlessRuntimeOwner struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"startTicks"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
}

func headlessProcessTicks(pid int) (string, error) {
	file, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 {
		return "", errors.New("process identity is unavailable")
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("process identity is unavailable")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", errors.New("process identity is unavailable")
	}
	return fields[19], nil
}

func headlessOwnerPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), "control.owner.json")
}

func readHeadlessOwner(socket string) (headlessRuntimeOwner, error) {
	var owner headlessRuntimeOwner
	data, err := readOwnedHeadlessUnit(headlessOwnerPath(socket))
	if err != nil {
		return owner, err
	}
	if json.Unmarshal(data, &owner) != nil || owner.PID <= 0 || owner.StartTicks == "" || owner.Inode == 0 {
		return owner, errors.New("private runtime receipt is invalid")
	}
	return owner, nil
}

func headlessDeadOwner(owner headlessRuntimeOwner) bool {
	ticks, err := headlessProcessTicks(owner.PID)
	return errors.Is(err, os.ErrNotExist) || (err == nil && ticks != owner.StartTicks)
}

func headlessOwnerMatches(owner headlessRuntimeOwner, st os.FileInfo) bool {
	identity, ok := st.Sys().(*syscall.Stat_t)
	return ok && st.Mode()&os.ModeSocket != 0 && st.Mode().Perm()&0077 == 0 && identity.Uid == uint32(os.Geteuid()) && uint64(identity.Dev) == owner.Device && identity.Ino == owner.Inode
}

// The lock inode is retained permanently while the runtime directory exists.
// Unlinking/recreating it would let two contenders acquire different flocks.
func acquireHeadlessRuntime(socket string) (func(), error) {
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(socket), "control.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("cannot acquire private runtime ownership")
	}
	st, err := lock.Stat()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	identity, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || identity.Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0077 != 0 {
		_ = lock.Close()
		return nil, errors.New("private runtime lock has unverified ownership")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("a PAIR headless owner is already running")
	}
	release := func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN); _ = lock.Close() }
	if err := reconcileHeadlessSocket(socket); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func reconcileHeadlessSocket(socket string) error {
	st, statErr := os.Lstat(socket)
	owner, ownerErr := readHeadlessOwner(socket)
	if errors.Is(statErr, os.ErrNotExist) {
		if errors.Is(ownerErr, os.ErrNotExist) {
			return nil
		}
		if ownerErr != nil || !headlessDeadOwner(owner) {
			return errors.New("private runtime receipt cannot be safely retired")
		}
		return os.Remove(headlessOwnerPath(socket))
	}
	if statErr != nil || ownerErr != nil || !headlessOwnerMatches(owner, st) || !headlessDeadOwner(owner) {
		return errors.New("existing private endpoint is live, foreign, or unverifiable; it was not removed")
	}
	connection, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return errors.New("existing private endpoint is still listening; it was not removed")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("stale endpoint could not be proven disconnected; it was not removed")
	}
	current, err := os.Lstat(socket)
	if err != nil || !os.SameFile(st, current) || !headlessOwnerMatches(owner, current) {
		return errors.New("private endpoint changed during stale-owner recovery")
	}
	if err := os.Remove(socket); err != nil {
		return err
	}
	return os.Remove(headlessOwnerPath(socket))
}

func writeHeadlessOwner(socket string, st os.FileInfo) (headlessRuntimeOwner, error) {
	ticks, err := headlessProcessTicks(os.Getpid())
	if err != nil {
		return headlessRuntimeOwner{}, err
	}
	identity, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return headlessRuntimeOwner{}, errors.New("socket identity is unavailable")
	}
	owner := headlessRuntimeOwner{PID: os.Getpid(), StartTicks: ticks, Device: uint64(identity.Dev), Inode: identity.Ino}
	data, _ := json.Marshal(owner)
	return owner, installHeadlessUnit(headlessOwnerPath(socket), string(data))
}
