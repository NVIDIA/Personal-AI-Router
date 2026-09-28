// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func acquireBootstrapOperationLock(root string) (func() error, error) {
	if err := (nativeSecureFS{}).EnsurePrivateDirectory(root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "operation.lock")
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		0,
		nil,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	created := err == nil
	if errors.Is(err, windows.ERROR_FILE_EXISTS) {
		handle, err = windows.CreateFile(
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
	}
	if err != nil {
		return nil, ErrStateIdentity
	}
	if created {
		if err := setBootstrapLockSecurity(handle); err != nil {
			_ = windows.CloseHandle(handle)
			return nil, err
		}
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		handle,
		&information,
	); err != nil ||
		information.NumberOfLinks != 1 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 ||
		!bootstrapLockSecurityExact(handle) {
		_ = windows.CloseHandle(handle)
		return nil, ErrUnsafeState
	}
	return func() error {
		return windows.CloseHandle(handle)
	}, nil
}

func setBootstrapLockSecurity(handle windows.Handle) error {
	descriptor, err := windows.SecurityDescriptorFromString(
		stateSecurityDescriptor,
	)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	group, _, err := descriptor.Group()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		group,
		dacl,
		nil,
	)
}

func bootstrapLockSecurityExact(handle windows.Handle) bool {
	actual, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil || actual == nil {
		return false
	}
	expected, err := windows.SecurityDescriptorFromString(
		stateSecurityDescriptor,
	)
	return err == nil && actual.String() == expected.String()
}
