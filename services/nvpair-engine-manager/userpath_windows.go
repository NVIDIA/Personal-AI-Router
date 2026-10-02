// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"log/slog"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Resolved once. Building the lazy DLL per call re-resolves the module and
// leaks a reference for every PATH update.
var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
)

// persistUserPath appends a user PATH entry only after recording its ownership.
func persistUserPath(dir string, receipt *pathReceipt, save func() error) error {
	return updateWindowsPath(func(current string) (string, error) {
		return addWindowsPath(current, dir, expandPath, receipt, save)
	})
}

// removeUserPath removes only the registry entry recorded for this installation.
func removeUserPath(receipt *pathReceipt) error {
	if receipt.WindowsEntry == "" {
		return nil
	}
	return updateWindowsPath(func(current string) (string, error) {
		return removeWindowsPath(current, receipt.WindowsEntry), nil
	})
}

// updateWindowsPath preserves the registry value type and notifies new processes.
func updateWindowsPath(update func(string) (string, error)) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	current, kind, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		current, kind, err = "", registry.EXPAND_SZ, nil
	}
	if err != nil {
		return err
	}
	next, err := update(current)
	if err != nil {
		return err
	}
	if next == current {
		return nil
	}
	if kind == registry.EXPAND_SZ {
		err = key.SetExpandStringValue("Path", next)
	} else {
		err = key.SetStringValue("Path", next)
	}
	if err != nil {
		return err
	}
	broadcastEnvironmentChange()
	return nil
}

// broadcastEnvironmentChange tells Explorer that future processes should pick up
// the new user PATH. Failure is logged, never returned: SMTO_ABORTIFHUNG makes
// SendMessageTimeoutW return 0 whenever any top-level window is not pumping
// messages — routine on a busy desktop, and certain in a session with no window
// station — while the registry value it is announcing is already authoritative
// for every shell started from here on.
func broadcastEnvironmentChange() {
	name, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		slog.Warn("skipped the environment change broadcast", "err", err)
		return
	}
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001a
		smtoAbortIfHung = 0x0002
		timeoutMs       = 5000
	)
	// The final lpdwResult is optional and nothing here reads it, so pass NULL
	// rather than a pointer into a variadic call that offers no lifetime guarantee.
	delivered, _, callErr := procSendMessageTimeoutW.Call(
		hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(name)),
		smtoAbortIfHung, timeoutMs, 0,
	)
	runtime.KeepAlive(name)
	if delivered == 0 {
		// callErr is a syscall.Errno and is never nil, so log it as the last
		// error rather than as proof anything went wrong.
		slog.Warn("PATH was saved, but the environment change broadcast did not complete; new terminals still inherit it", "last_error", callErr)
	}
}
