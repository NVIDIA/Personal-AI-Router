// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

func ownedHeadlessDirectory(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return errors.New("owning-account runtime directory is unavailable")
	}
	identity, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || identity.Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0077 != 0 {
		return errors.New("runtime directory must belong to this account and have mode 0700")
	}
	return nil
}

func headlessSocketPath() (string, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/run/user", strconv.Itoa(os.Geteuid()))
	}
	if !filepath.IsAbs(runtimeDir) {
		return "", errors.New("XDG_RUNTIME_DIR must be absolute")
	}
	if err := ownedHeadlessDirectory(runtimeDir); err != nil {
		return "", err
	}
	dir := filepath.Join(runtimeDir, "nvidia-pair")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", errors.New("cannot create private PAIR runtime directory")
	}
	if err := ownedHeadlessDirectory(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "control.sock")
	if len(path) >= 104 {
		return "", errors.New("private control socket path is too long")
	}
	return path, nil
}

func headlessSameUser(conn net.Conn) bool {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return false
	}
	allowed := false
	err = raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		allowed = err == nil && credential.Uid == uint32(os.Geteuid())
	})
	return err == nil && allowed
}

func listenHeadless() (net.Listener, func(), error) {
	path, err := headlessSocketPath()
	if err != nil {
		return nil, nil, err
	}
	release, err := acquireHeadlessRuntime(path)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		release()
		return nil, nil, errors.New("private control endpoint could not be acquired")
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		release()
		return nil, nil, err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		release()
		return nil, nil, err
	}
	owner, err := writeHeadlessOwner(path, identity)
	if err != nil {
		_ = listener.Close()
		current, e := os.Lstat(path)
		if e == nil && os.SameFile(identity, current) {
			_ = os.Remove(path)
		}
		release()
		return nil, nil, err
	}
	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			_ = listener.Close()
			current, err := os.Lstat(path)
			if err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(identity, current) {
				_ = os.Remove(path)
				if currentOwner, e := readHeadlessOwner(path); e == nil && currentOwner == owner {
					_ = os.Remove(headlessOwnerPath(path))
				}
			}
			release()
		})
	}
	return listener, cleanup, nil
}

func dialHeadless(ctx context.Context) (net.Conn, error) {
	path, err := headlessSocketPath()
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("PAIR headless control is not running; start its supported user service or use normal pairing")
	}
	identity, ok := st.Sys().(*syscall.Stat_t)
	if !ok || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm()&0077 != 0 || identity.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("private control endpoint ownership could not be verified")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, errors.New("PAIR private control is unavailable; no endpoint was removed")
	}
	if !headlessSameUser(conn) {
		_ = conn.Close()
		return nil, errors.New("private control peer is not the owning account")
	}
	if !headlessSameExecutable(conn) {
		_ = conn.Close()
		return nil, errors.New("private control belongs to a different PAIR executable; preserve its owner and use the matching installation")
	}
	return conn, nil
}

func headlessSameExecutable(conn net.Conn) bool {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return false
	}
	var pid int32
	if err := raw.Control(func(fd uintptr) {
		credential, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err == nil {
			pid = credential.Pid
		}
	}); err != nil || pid <= 0 {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	actual, err := os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false
	}
	expected, err := os.Stat(self)
	return err == nil && os.SameFile(actual, expected)
}

// Passive port admission avoids starting a second fleet against an existing
// desktop/TUI/service owner. It never kills a process or takes a TCP listener.
func headlessPortConflicts() error {
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) && strings.HasSuffix(path, "tcp6") {
			continue
		}
		if err != nil {
			return errors.New("cannot verify existing PAIR listener conflicts")
		}
		data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
		_ = file.Close()
		if err != nil || len(data) > 4<<20 {
			return errors.New("listener conflict observation exceeded its supported bound")
		}
		if err := checkHeadlessPorts(string(data)); err != nil {
			return err
		}
	}
	return nil
}

func checkHeadlessPorts(table string) error {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.Contains(line, "local_address") {
			continue
		}
		if len(fields) < 4 {
			return errors.New("unrecognized listener table")
		}
		if fields[3] != "0A" {
			continue
		}
		_, raw, ok := strings.Cut(fields[1], ":")
		if !ok {
			return errors.New("unrecognized listener table")
		}
		port, err := strconv.ParseUint(raw, 16, 16)
		if err != nil {
			return errors.New("unrecognized listener port")
		}
		if port >= 14318 && port <= 14323 {
			return fmt.Errorf("PAIR service port %d is already in use; preserve the existing owner and use its normal pairing path", port)
		}
	}
	return nil
}
