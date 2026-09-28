// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"nvpair-shared/hostbootstrap"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func inspectHelperEndpointNative(
	request hostbootstrap.Request,
	endpoint localEndpoint,
) (helperComponentState, error) {
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return helperComponentState{}, err
	}
	timeout := 200 * time.Millisecond
	connection, err := winio.DialPipe(endpoint.Address, &timeout)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, os.ErrNotExist) {
		return helperComponentState{}, nil
	}
	if err != nil {
		return helperComponentState{Present: true}, nil
	}
	defer connection.Close()
	syscallConnection, ok := connection.(syscall.Conn)
	if !ok {
		return helperComponentState{Present: true}, nil
	}
	raw, err := syscallConnection.SyscallConn()
	if err != nil {
		return helperComponentState{Present: true}, err
	}
	var actual *windows.SECURITY_DESCRIPTOR
	var securityErr error
	if err := raw.Control(func(fd uintptr) {
		actual, securityErr = windows.GetSecurityInfo(
			windows.Handle(fd),
			windows.SE_KERNEL_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|
				windows.GROUP_SECURITY_INFORMATION|
				windows.DACL_SECURITY_INFORMATION,
		)
	}); err != nil {
		return helperComponentState{Present: true}, err
	}
	if securityErr != nil || actual == nil {
		return helperComponentState{Present: true}, securityErr
	}
	expectedText := strings.ReplaceAll(endpoint.DACL, "TARGET_SID", principal.SID)
	expected, err := windows.SecurityDescriptorFromString(expectedText)
	if err != nil {
		return helperComponentState{}, err
	}
	return helperComponentState{
		Present: true,
		Exact:   strings.EqualFold(actual.String(), expected.String()),
	}, nil
}

type nativeProcessInspector struct{}

func newNativeProcessInspector(commandRunner) processInspector {
	return nativeProcessInspector{}
}

func (nativeProcessInspector) Snapshot() ([]processRecord, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var processes []processRecord
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		name := strings.ToLower(windows.UTF16ToString(entry.ExeFile[:]))
		relevant := name == "nvpair.exe" ||
			name == "nvpair-tui.exe" ||
			name == "nvpair-ui-broker.exe"
		process, openErr := windows.OpenProcess(
			windows.PROCESS_QUERY_LIMITED_INFORMATION,
			false,
			entry.ProcessID,
		)
		if openErr == nil {
			buffer := make([]uint16, 32768)
			size := uint32(len(buffer))
			queryErr := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size)
			_ = windows.CloseHandle(process)
			if queryErr == nil {
				processes = append(processes, processRecord{
					PID:  entry.ProcessID,
					PPID: entry.ParentProcessID,
					Path: windows.UTF16ToString(buffer[:size]),
				})
			} else if relevant {
				return nil, ErrVerification
			}
		} else if relevant {
			return nil, ErrVerification
		}
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return processes, nil
}

func validateReviewedAccount(account hostbootstrap.AccountIdentity) error {
	resolved, err := user.Lookup(account.Name)
	if err != nil {
		return err
	}
	home := filepath.Clean(resolved.HomeDir)
	if !strings.EqualFold(account.HomePath, home) ||
		!strings.EqualFold(
			account.AuthorizedKeysPath,
			filepath.Join(home, ".ssh", "authorized_keys"),
		) {
		return ErrUnsupportedIdentity
	}
	return nil
}

func currentNativeTarget() (hostbootstrap.Target, string, error) {
	architecture, err := nativeArchitecture(runtime.GOARCH)
	if err != nil {
		return hostbootstrap.Target{}, "", err
	}
	return hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: architecture,
	}, "", nil
}

func nativeStateDirectory(value string) (string, error) {
	if value != windowsStateDirectory {
		return "", ErrUnsupportedTarget
	}
	programData := os.Getenv("ProgramData")
	if programData == "" || !filepath.IsAbs(programData) {
		return "", ErrUnsupportedTarget
	}
	return filepath.Join(
		programData,
		"NVIDIA Corporation",
		"Personal AI Router",
		"host-bootstrap",
	), nil
}

func nativePrivileged() (bool, error) {
	return windows.Token(0).IsElevated(), nil
}

func nativeArchitecture(value string) (hostbootstrap.Architecture, error) {
	switch value {
	case "amd64":
		return hostbootstrap.ArchitectureAMD64, nil
	case "arm64":
		return hostbootstrap.ArchitectureARM64, nil
	default:
		return "", ErrUnsupportedTarget
	}
}

func resolveReviewedPrincipal(account string) (reviewedPrincipalState, error) {
	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return reviewedPrincipalState{}, err
	}
	return reviewedPrincipalState{SID: sid.String()}, nil
}

func nativeGroupName(principal reviewedPrincipalState) string {
	return principal.SID
}

func validateAuthorizedKeyRead(path, account string) error {
	principal, err := resolveReviewedPrincipal(account)
	if err != nil {
		return err
	}
	if err := validateWindowsAuthorizedKeyAncestors(
		filepath.Dir(path),
		authorizedKeysDescriptor(principal.SID),
	); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	links, err := windowsLinkCount(path)
	if err != nil ||
		links != 1 ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		isWindowsReparse(info) ||
		!hasWindowsSecurity(path, authorizedKeysDescriptor(principal.SID)) {
		return ErrUnsafeState
	}
	return nil
}

func validateWindowsAuthorizedKeyAncestors(
	directory string,
	descriptor string,
) error {
	if !filepath.IsAbs(directory) ||
		filepath.Clean(directory) != directory {
		return ErrUnsafeState
	}
	volume := filepath.VolumeName(directory)
	current := volume + string(filepath.Separator)
	components := strings.Split(
		strings.TrimPrefix(directory, current),
		string(filepath.Separator),
	)
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 ||
			isWindowsReparse(info) {
			return ErrUnsafeState
		}
		if index == len(components)-1 &&
			!hasWindowsSecurity(current, descriptor) {
			return ErrUnsafeState
		}
	}
	return nil
}

func writeAuthorizedKeysNoFollow(
	path string,
	body []byte,
	principal reviewedPrincipalState,
	expected []byte,
	operationID string,
) error {
	if principal.SID == "" {
		return ErrUnsupportedIdentity
	}
	directory := filepath.Dir(path)
	descriptor := authorizedKeysDescriptor(principal.SID)
	if err := rejectWindowsReparseComponents(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(directory, 0700); err != nil {
			return err
		}
		if err := setWindowsSecurity(directory, descriptor); err != nil {
			return err
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() ||
		info.Mode()&os.ModeSymlink != 0 ||
		isWindowsReparse(info) ||
		!hasWindowsSecurity(directory, descriptor) {
		return ErrUnsafeState
	}
	if existing, err := os.Lstat(path); err == nil {
		if !existing.Mode().IsRegular() ||
			existing.Mode()&os.ModeSymlink != 0 ||
			isWindowsReparse(existing) ||
			!hasWindowsSecurity(path, descriptor) {
			return ErrUnsafeState
		}
		links, err := windowsLinkCount(path)
		if err != nil || links != 1 {
			return ErrUnsafeState
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(directory, ".nvpair-authorized-keys-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := setWindowsSecurity(temporary, descriptor); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
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
	return publishAuthorizedKeysCAS(temporary, path, expected, operationID)
}

func publishAuthorizedKeysCAS(temporary, destination string, expected []byte, operationID string) error {
	if !stateHex32.MatchString(operationID) {
		return ErrStateIdentity
	}
	replacement, err := readBoundedRegularFile(temporary, 1<<20)
	if err != nil {
		return err
	}
	adopted := false
	if err := recoverAuthorizedKeysBackup(
		destination,
		operationID,
		func(displaced, current []byte) (bool, error) {
			exact := bytes.Equal(displaced, expected) &&
				bytes.Equal(current, replacement)
			adopted = adopted || exact
			return exact, nil
		},
	); err != nil {
		return err
	}
	if adopted {
		return nil
	}
	name, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.DELETE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		if len(expected) != 0 {
			return ErrStateIdentity
		}
		from, err := windows.UTF16PtrFromString(temporary)
		if err != nil {
			return err
		}
		return windows.MoveFileEx(from, name, windows.MOVEFILE_WRITE_THROUGH)
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), destination)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return ErrUnsafeState
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil ||
		information.NumberOfLinks != 1 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = file.Close()
		return ErrUnsafeState
	}
	current, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	if err != nil ||
		closeErr != nil ||
		len(current) > 1<<20 ||
		!bytes.Equal(current, expected) {
		return ErrStateIdentity
	}
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	backup, err := windows.UTF16PtrFromString(destination + ".backup-" + operationID)
	if err != nil {
		return err
	}
	result, _, callErr := replaceFileW.Call(
		uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(from)),
		uintptr(unsafe.Pointer(backup)),
		1,
		0,
		0,
	)
	if result == 0 {
		return callErr
	}
	adopted = false
	if err := recoverAuthorizedKeysBackup(
		destination,
		operationID,
		func(displaced, current []byte) (bool, error) {
			exact := bytes.Equal(displaced, expected) &&
				bytes.Equal(current, replacement)
			adopted = adopted || exact
			return exact, nil
		},
	); err != nil {
		return err
	}
	if !adopted {
		return ErrStateIdentity
	}
	return nil
}

func recoverAuthorizedKeysBackup(
	destination string,
	operationID string,
	validate func([]byte, []byte) (bool, error),
) error {
	if !stateHex32.MatchString(operationID) {
		return ErrStateIdentity
	}
	backup := destination + ".backup-" + operationID
	conflict := destination + ".conflict-" + operationID
	if _, err := os.Lstat(conflict); err == nil {
		return ErrStateIdentity
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, destinationErr := os.Lstat(destination)
	_, backupErr := os.Lstat(backup)
	destinationPresent := destinationErr == nil
	backupPresent := backupErr == nil
	if destinationErr != nil && !errors.Is(destinationErr, fs.ErrNotExist) {
		return destinationErr
	}
	if backupErr != nil && !errors.Is(backupErr, fs.ErrNotExist) {
		return backupErr
	}
	switch {
	case !destinationPresent && !backupPresent:
		return nil
	case !destinationPresent && backupPresent:
		from, err := windows.UTF16PtrFromString(backup)
		if err != nil {
			return err
		}
		to, err := windows.UTF16PtrFromString(destination)
		if err != nil {
			return err
		}
		if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
			return err
		}
		return nil
	case destinationPresent && !backupPresent:
		return nil
	}
	displaced, err := readBoundedRegularFile(backup, 1<<20)
	if err != nil {
		return err
	}
	current, err := readBoundedRegularFile(destination, 1<<20)
	if err != nil {
		return err
	}
	exact, validationErr := validate(displaced, current)
	backupName, err := windows.UTF16PtrFromString(backup)
	if err != nil {
		return err
	}
	if validationErr == nil && exact {
		return windows.DeleteFile(backupName)
	}
	destinationName, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	conflictName, err := windows.UTF16PtrFromString(conflict)
	if err != nil {
		return err
	}
	result, _, callErr := replaceFileW.Call(
		uintptr(unsafe.Pointer(destinationName)),
		uintptr(unsafe.Pointer(backupName)),
		uintptr(unsafe.Pointer(conflictName)),
		1,
		0,
		0,
	)
	if result == 0 {
		return callErr
	}
	if validationErr != nil {
		return validationErr
	}
	return ErrStateIdentity
}

func authorizedKeysDescriptor(sid string) string {
	return "O:" + sid + "G:" + sid +
		"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")"
}

func rejectWindowsReparseComponents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnsafeState
	}
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || isWindowsReparse(info) {
			return ErrUnsafeState
		}
	}
	return nil
}

func setWindowsSecurity(path, sddl string) error {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
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

func hasWindowsSecurity(path, sddl string) bool {
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
	expected, err := windows.SecurityDescriptorFromString(sddl)
	return err == nil && strings.EqualFold(actual.String(), expected.String())
}

func windowsLinkCount(path string) (uint32, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return 0, err
	}
	return information.NumberOfLinks, nil
}

func nativeFileLinkCount(path string, _ fs.FileInfo) (uint64, error) {
	links, err := windowsLinkCount(path)
	return uint64(links), err
}

func validateRootDefinition(path string, _ fs.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	links, err := windowsLinkCount(path)
	if err != nil ||
		links != 1 ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		isWindowsReparse(info) {
		return ErrUnsafeState
	}
	return nil
}

func ensureRootDefinitionDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnsafeState
	}
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			return ErrUnsafeState
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0755); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || isWindowsReparse(info) {
			return ErrUnsafeState
		}
	}
	return nil
}

func rootDefinitionDirectoryExact(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() ||
		info.Mode()&os.ModeSymlink != 0 ||
		isWindowsReparse(info) {
		return false, ErrUnsafeState
	}
	return true, nil
}
