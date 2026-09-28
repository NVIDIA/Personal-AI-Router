// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

type nativeWindowsServiceAPI struct{}

func newNativeWindowsServiceAPI() windowsServiceAPI {
	return nativeWindowsServiceAPI{}
}

func (nativeWindowsServiceAPI) Inspect(name string) (windowsServiceInfo, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return windowsServiceInfo{}, err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return windowsServiceInfo{}, ErrStateMissing
	}
	if err != nil {
		return windowsServiceInfo{}, err
	}
	defer service.Close()
	config, err := service.Config()
	if err != nil {
		return windowsServiceInfo{}, err
	}
	status, err := service.Query()
	if err != nil {
		return windowsServiceInfo{}, err
	}
	command, err := windows.DecomposeCommandLine(config.BinaryPathName)
	if err != nil || len(command) == 0 {
		return windowsServiceInfo{}, ErrVerification
	}
	return windowsServiceInfo{
		Executable:  command[0],
		Arguments:   append([]string(nil), command[1:]...),
		ServiceType: config.ServiceType,
		StartType:   config.StartType,
		Account:     config.ServiceStartName,
		State:       uint32(status.State),
	}, nil
}
