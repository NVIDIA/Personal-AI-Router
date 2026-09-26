// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const stateSecurityDescriptor = "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"

type nativeSecureFS struct {
	publish         func(string, string) error
	ensureDirectory func(string) error
	secureFile      func(string) error
}

var windowsMoveFileEx = windows.MoveFileEx

func (nativeSecureFS) EnsurePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnsafeState
	}
	volume := filepath.VolumeName(path)
	if volume == "" {
		return ErrUnsafeState
	}
	current := volume + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator))
	for index, component := range parts {
		if component == "" || component == "." || component == ".." {
			return ErrUnsafeState
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		created := false
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err != nil {
				return err
			}
			created = true
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || isWindowsReparse(info) {
			return ErrUnsafeState
		}
		if created {
			if err := setExactStateSecurity(current); err != nil {
				return err
			}
		}
		if index == len(parts)-1 && !hasExactStateSecurity(current) {
			return ErrUnsafeState
		}
	}
	return nil
}

func (nativeSecureFS) ReadNoFollow(
	path string,
	limit int64,
) ([]byte, secureMetadata, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, secureMetadata{}, err
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
		return nil, secureMetadata{}, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, secureMetadata{}, ErrUnsafeState
	}
	defer file.Close()
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, secureMetadata{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, secureMetadata{}, err
	}
	if int64(len(raw)) > limit {
		return nil, secureMetadata{}, ErrUnsafeState
	}
	reparse := information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	directory := information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	metadata := secureMetadata{
		Regular: !directory && !reparse,
		Reparse: reparse,
		Links:   uint64(information.NumberOfLinks),
		Mode:    0600,
	}
	if hasExactStateSecurity(path) {
		metadata.Owner = rootOwner
	}
	return raw, metadata, nil
}

func (filesystem nativeSecureFS) AtomicWriteNoFollow(
	path string,
	data []byte,
	mode fs.FileMode,
) error {
	if mode.Perm() != 0600 {
		return ErrUnsafeState
	}
	if _, metadata, err := filesystem.ReadNoFollow(path, maxStateFileBytes); err == nil {
		if !safeMetadata(metadata) {
			return ErrUnsafeState
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	ensureDirectory := filesystem.ensureDirectory
	if ensureDirectory == nil {
		ensureDirectory = filesystem.EnsurePrivateDirectory
	}
	if err := ensureDirectory(directory); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".nvpair-state-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	secureFile := filesystem.secureFile
	if secureFile == nil {
		secureFile = setExactStateSecurity
	}
	if err := secureFile(temporary); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	publish := filesystem.publish
	if publish == nil {
		publish = publishWindowsStateFile
	}
	return publish(temporary, path)
}

func publishWindowsStateFile(temporary, destination string) error {
	source, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windowsMoveFileEx(
		source,
		target,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func (filesystem nativeSecureFS) RemoveNoFollow(path string) error {
	_, metadata, err := filesystem.ReadNoFollow(path, maxStateFileBytes)
	if err != nil {
		return err
	}
	if !safeMetadata(metadata) {
		return ErrUnsafeState
	}
	return os.Remove(path)
}

func isWindowsReparse(info fs.FileInfo) bool {
	data, ok := info.Sys().(*windows.Win32FileAttributeData)
	return ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func setExactStateSecurity(path string) error {
	descriptor, err := windows.SecurityDescriptorFromString(stateSecurityDescriptor)
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
	return windows.SetNamedSecurityInfo(
		path,
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

func hasExactStateSecurity(path string) bool {
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
	expected, err := windows.SecurityDescriptorFromString(stateSecurityDescriptor)
	return err == nil && actual.String() == expected.String()
}

func safeMetadata(metadata secureMetadata) bool {
	return metadata.Regular &&
		!metadata.Symlink &&
		!metadata.Reparse &&
		metadata.Links == 1 &&
		metadata.Mode.Perm() == 0600 &&
		metadata.Owner == rootOwner
}
