// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// taskkillTimeout bounds each taskkill invocation so a hung taskkill can't
// wedge StopAll (and the whole app shutdown) now that the broker no longer
// force-kills engine-manager on a timeout.
const taskkillTimeout = 5 * time.Second

// configureSysProcAttr hides the child's console window
// (HideWindow + CREATE_NO_WINDOW), matching every other NVPAIR subprocess.
func configureSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// gracefulSignal stops the process tree and is the only stop signal
// engine-manager sends: stop() sends this once and waits for the engine to
// exit. Windows has no SIGTERM, and the engines we spawn run windowless
// (CREATE_NO_WINDOW), so a non-/F taskkill only posts WM_CLOSE — which a
// windowless process can't receive ("can only be terminated forcefully"), i.e.
// it does nothing. Never force-killing such a process would leave the engine
// running forever, so on Windows the stop is taskkill /T /F.
func gracefulSignal(cmd *exec.Cmd) error {
	return taskkill(cmd, true)
}

func taskkill(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	args := []string{"/T", "/PID", strconv.Itoa(cmd.Process.Pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	kc := exec.CommandContext(ctx, "taskkill", args...)
	configureSysProcAttr(kc) // hide taskkill's own console window
	return kc.Run()
}

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const (
	// TCP_TABLE_OWNER_PID_LISTENER — listening sockets with their owning PID.
	tcpTableOwnerPIDListener = 3
	errInsufficientBuffer    = 122 // ERROR_INSUFFICIENT_BUFFER
)

// mibTCPRowOwnerPID mirrors MIB_TCPROW_OWNER_PID (IPv4). Every field is a
// DWORD, so the C layout is a run of uint32s with no padding.
type mibTCPRowOwnerPID struct {
	state      uint32
	localAddr  uint32
	localPort  uint32
	remoteAddr uint32
	remotePort uint32
	owningPID  uint32
}

// mibTCP6RowOwnerPID mirrors MIB_TCP6ROW_OWNER_PID (IPv6).
type mibTCP6RowOwnerPID struct {
	localAddr     [16]byte
	localScopeID  uint32
	localPort     uint32
	remoteAddr    [16]byte
	remoteScopeID uint32
	remotePort    uint32
	state         uint32
	owningPID     uint32
}

// pidOnPort returns the PID of the process listening on the given TCP port
// (IPv4 first, then IPv6) plus that process's full image path. ok is false
// when nothing is listening or the owner can't be resolved, so a caller can
// fail closed (decline the stop). It is precise to a single port: a genuine
// desktop app on a different port is never surfaced.
func pidOnPort(port int) (pid int, image string, ok bool) {
	if p, found := listenerPID(windows.AF_INET, port); found {
		return p, imagePathForPID(p), true
	}
	if p, found := listenerPID(windows.AF_INET6, port); found {
		return p, imagePathForPID(p), true
	}
	return 0, "", false
}

// listenerPID walks the owner-PID TCP table for one address family and
// returns the PID whose local port matches. The table is fetched with the
// standard two-call sizing dance (NULL buffer to learn the size, then a
// real buffer).
func listenerPID(family uint32, port int) (int, bool) {
	var size uint32
	r, _, _ := procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(family), tcpTableOwnerPIDListener, 0)
	if r != errInsufficientBuffer && r != 0 {
		return 0, false
	}
	if size == 0 {
		return 0, false
	}
	buf := make([]byte, size)
	r, _, _ = procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(family), tcpTableOwnerPIDListener, 0)
	if r != 0 {
		return 0, false
	}
	// Both MIB_*TABLE_OWNER_PID structs are { DWORD dwNumEntries; row[] }, so
	// the rows begin at offset 4 (the row structs are 4-byte aligned).
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	if n == 0 {
		return 0, false
	}
	if family == windows.AF_INET {
		rows := unsafe.Slice((*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[4])), int(n))
		for i := range rows {
			if ntohsPort(rows[i].localPort) == port {
				return int(rows[i].owningPID), true
			}
		}
		return 0, false
	}
	rows := unsafe.Slice((*mibTCP6RowOwnerPID)(unsafe.Pointer(&buf[4])), int(n))
	for i := range rows {
		if ntohsPort(rows[i].localPort) == port {
			return int(rows[i].owningPID), true
		}
	}
	return 0, false
}

// ntohsPort converts the local-port DWORD (the port number in network byte
// order, held in the low 16 bits) to a host-order int.
func ntohsPort(dw uint32) int {
	return int(((dw & 0xFF) << 8) | ((dw >> 8) & 0xFF))
}

// imagePathForPID resolves a PID's full executable path. Empty on any error
// (access denied, exited) so the image check fails closed.
func imagePathForPID(pid int) string {
	if pid <= 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

// signalPID asks the PID's process tree to stop (taskkill /T), escalating to
// a forced /F kill when force is set. It is the PID-addressed kill used only by
// the orphan reclaim (a process we lost the *exec.Cmd handle to), distinct from
// the normal graceful-only stop() path.
func signalPID(pid int, force bool) error {
	if pid <= 0 {
		return nil
	}
	args := []string{"/T", "/PID", strconv.Itoa(pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	kc := exec.CommandContext(ctx, "taskkill", args...)
	configureSysProcAttr(kc) // hide taskkill's own console window
	return kc.Run()
}

// pidAlive reports whether the PID is still running (WaitForSingleObject with
// a zero timeout: WAIT_TIMEOUT means it hasn't signaled exit). A handle we
// can't open is treated as gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	s, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return true
	}
	return s == uint32(windows.WAIT_TIMEOUT)
}
