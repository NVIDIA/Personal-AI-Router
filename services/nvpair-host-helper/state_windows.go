// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"nvpair-shared/hostbootstrap"
)

const helperStateSecurityDescriptor = "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"

func reviewedPrincipalMatchesAccount(
	principal reviewedPrincipal,
	account string,
) bool {
	sid, _, _, err := windows.LookupSID("", account)
	return err == nil &&
		principal.UID == 0 &&
		principal.GID == 0 &&
		principal.SID == sid.String()
}

func validateInitialHelperExecutable(path, expectedDigest string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return hostbootstrap.ErrHelperProtocol
	}
	defer file.Close()
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil ||
		information.NumberOfLinks != 1 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 ||
		!helperStateSecurityExact(path) {
		return hostbootstrap.ErrHelperProtocol
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil ||
		hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

func helperReadSecure(path string) ([]byte, error) {
	if err := validateHelperStatePath(filepath.Dir(path)); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, hostbootstrap.ErrHelperProtocol
	}
	defer file.Close()
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, err
	}
	if information.NumberOfLinks != 1 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 ||
		!helperStateSecurityExact(path) {
		return nil, hostbootstrap.ErrHelperProtocol
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxHelperStateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxHelperStateBytes {
		return nil, hostbootstrap.ErrHelperFrameTooLarge
	}
	return raw, nil
}

func validateHelperStatePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return hostbootstrap.ErrHelperProtocol
	}
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	components := strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator))
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		attributes, ok := info.Sys().(*windows.Win32FileAttributeData)
		if !ok ||
			!info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 ||
			attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return hostbootstrap.ErrHelperProtocol
		}
		if index == len(components)-1 && !helperStateSecurityExact(current) {
			return hostbootstrap.ErrHelperProtocol
		}
	}
	return nil
}

func helperStateSecurityExact(path string) bool {
	actual, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil || actual == nil {
		return false
	}
	expected, err := windows.SecurityDescriptorFromString(helperStateSecurityDescriptor)
	return err == nil && actual.String() == expected.String()
}

func helperStatePath(root, name string) string {
	return filepath.Join(root, name)
}
