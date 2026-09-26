// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"nvpair-shared/hostbootstrap"
)

const darwinHelperSocket = "/var/run/nvpair-host-helper.sock"

func fixedStateDirectory() string {
	return "/Library/Application Support/NVIDIA/Personal AI Router/host-bootstrap"
}

func fixedBootstrapPath() string {
	return "/Library/PrivilegedHelperTools/nvpair-host-bootstrap"
}

func listenLocal(
	principal reviewedPrincipal,
	_ bool,
	allowStaleRemoval bool,
) (net.Listener, error) {
	info, err := os.Lstat(darwinHelperSocket)
	if err == nil {
		if !allowStaleRemoval {
			return nil, hostbootstrap.ErrHelperProtocol
		}
		if err := validateDarwinSocket(info, principal); err != nil {
			return nil, hostbootstrap.ErrHelperProtocol
		}
		connection, dialErr := net.DialTimeout(
			"unix",
			darwinHelperSocket,
			100*time.Millisecond,
		)
		if dialErr == nil {
			_ = connection.Close()
			return nil, hostbootstrap.ErrHelperProtocol
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) &&
			!errors.Is(dialErr, os.ErrNotExist) {
			return nil, hostbootstrap.ErrHelperProtocol
		}
		if err := os.Remove(darwinHelperSocket); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: darwinHelperSocket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chown(darwinHelperSocket, 0, int(principal.GID)); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := os.Chmod(darwinHelperSocket, 0660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	info, err = os.Lstat(darwinHelperSocket)
	if err != nil || validateDarwinSocket(info, principal) != nil {
		_ = listener.Close()
		return nil, hostbootstrap.ErrHelperProtocol
	}
	return listener, nil
}

func validateDarwinSocket(info os.FileInfo, principal reviewedPrincipal) error {
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

func dialLocal(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", darwinHelperSocket)
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
		credentials, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			socketErr = err
			return
		}
		identity.UID = credentials.Uid
		if credentials.Ngroups > 0 {
			identity.GID = credentials.Groups[0]
		}
	}); err != nil {
		return peerIdentity{}, err
	}
	if socketErr != nil {
		return peerIdentity{}, socketErr
	}
	return identity, nil
}
