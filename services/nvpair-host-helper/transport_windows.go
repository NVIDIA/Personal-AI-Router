// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"nvpair-shared/hostbootstrap"
)

const windowsHelperPipe = `\\.\pipe\nvpair-host-helper`

func fixedStateDirectory() string {
	programData := os.Getenv("ProgramData")
	return filepath.Join(
		programData,
		"NVIDIA Corporation",
		"Personal AI Router",
		"host-bootstrap",
	)
}

func fixedBootstrapPath() string {
	return `C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-bootstrap.exe`
}

func listenLocal(
	principal reviewedPrincipal,
	_ bool,
	_ bool,
) (net.Listener, error) {
	if principal.SID == "" {
		return nil, hostbootstrap.ErrHelperProtocol
	}
	return winio.ListenPipe(windowsHelperPipe, &winio.PipeConfig{
		SecurityDescriptor: "O:SYG:SYD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;" + principal.SID + ")",
		MessageMode:        false,
		InputBufferSize:    hostbootstrap.MaxHelperFrameBytes + 4,
		OutputBufferSize:   hostbootstrap.MaxHelperFrameBytes + 4,
	})
}

func dialLocal(ctx context.Context) (net.Conn, error) {
	return winio.DialPipeContext(ctx, windowsHelperPipe)
}

func nativePeerIdentity(connection net.Conn) (peerIdentity, error) {
	syscallConnection, ok := connection.(syscall.Conn)
	if !ok {
		return peerIdentity{}, ErrPeerUnauthorized
	}
	raw, err := syscallConnection.SyscallConn()
	if err != nil {
		return peerIdentity{}, err
	}
	var processID uint32
	var pipeErr error
	if err := raw.Control(func(fd uintptr) {
		pipeErr = windows.GetNamedPipeClientProcessId(windows.Handle(fd), &processID)
	}); err != nil {
		return peerIdentity{}, err
	}
	if pipeErr != nil || processID == 0 {
		return peerIdentity{}, ErrPeerUnauthorized
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return peerIdentity{}, err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return peerIdentity{}, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return peerIdentity{}, err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return peerIdentity{}, err
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return peerIdentity{}, err
	}
	administrator, err := token.IsMember(adminSID)
	if err != nil {
		return peerIdentity{}, err
	}
	return peerIdentity{
		SID:           user.User.Sid.String(),
		System:        user.User.Sid.Equals(systemSID),
		Administrator: administrator,
	}, nil
}
