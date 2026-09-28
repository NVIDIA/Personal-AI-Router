// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"nvpair-shared/hostbootstrap"
)

const linuxHelperSocket = "/run/nvpair-host-helper.sock"

func fixedStateDirectory() string {
	return "/var/lib/nvpair/host-bootstrap"
}

func fixedBootstrapPath() string {
	return "/usr/libexec/nvpair-host-bootstrap"
}

func listenLocal(
	principal reviewedPrincipal,
	console bool,
	allowStaleRemoval bool,
) (net.Listener, error) {
	if listener, inherited, err := inheritedSystemdListener(); inherited || err != nil {
		if err == nil {
			err = validateLinuxSocket(principal)
		}
		if err != nil && listener != nil {
			_ = listener.Close()
		}
		return listener, err
	}
	if !console {
		return nil, hostbootstrap.ErrHelperProtocol
	}
	if _, err := os.Lstat(linuxHelperSocket); err == nil {
		if !allowStaleRemoval {
			return nil, hostbootstrap.ErrHelperProtocol
		}
		if err := removeStaleUnixSocket(
			linuxHelperSocket,
			principal,
		); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: linuxHelperSocket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chown(linuxHelperSocket, 0, int(principal.GID)); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := os.Chmod(linuxHelperSocket, 0660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := validateLinuxSocket(principal); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func validateLinuxSocket(principal reviewedPrincipal) error {
	info, err := os.Lstat(linuxHelperSocket)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok ||
		info.Mode()&os.ModeSocket == 0 ||
		info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0660 ||
		stat.Uid != 0 ||
		stat.Gid != principal.GID {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

func inheritedSystemdListener() (net.Listener, bool, error) {
	pidText, pidSet := os.LookupEnv("LISTEN_PID")
	fdsText, fdsSet := os.LookupEnv("LISTEN_FDS")
	if !pidSet && !fdsSet {
		return nil, false, nil
	}
	if !pidSet || !fdsSet {
		return nil, true, hostbootstrap.ErrHelperProtocol
	}
	pid, pidErr := strconv.Atoi(pidText)
	fds, fdsErr := strconv.Atoi(fdsText)
	if pidErr != nil || fdsErr != nil || pid != os.Getpid() || fds != 1 {
		return nil, true, hostbootstrap.ErrHelperProtocol
	}
	file := os.NewFile(uintptr(3), "nvpair-host-helper.socket")
	if file == nil {
		return nil, true, hostbootstrap.ErrHelperProtocol
	}
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, true, err
	}
	if _, ok := listener.(*net.UnixListener); !ok {
		_ = listener.Close()
		return nil, true, hostbootstrap.ErrHelperProtocol
	}
	return listener, true, nil
}

func removeStaleUnixSocket(
	path string,
	principal reviewedPrincipal,
) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok ||
		info.Mode()&os.ModeSocket == 0 ||
		info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0660 ||
		stat.Uid != 0 ||
		stat.Gid != principal.GID {
		return hostbootstrap.ErrHelperProtocol
	}
	connection, dialErr := net.DialTimeout(
		"unix",
		path,
		100*time.Millisecond,
	)
	if dialErr == nil {
		_ = connection.Close()
		return hostbootstrap.ErrHelperProtocol
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) &&
		!errors.Is(dialErr, os.ErrNotExist) {
		return hostbootstrap.ErrHelperProtocol
	}
	return os.Remove(path)
}

func dialLocal(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", linuxHelperSocket)
}

func nativePeerIdentity(connection net.Conn) (peerIdentity, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return peerIdentity{}, ErrPeerUnauthorized
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return peerIdentity{}, err
	}
	var identity peerIdentity
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			socketErr = err
			return
		}
		identity.UID = credentials.Uid
		identity.GID = credentials.Gid
	}); err != nil {
		return peerIdentity{}, err
	}
	if socketErr != nil {
		return peerIdentity{}, socketErr
	}
	return identity, nil
}
