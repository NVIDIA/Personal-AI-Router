// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// detectGPUs enumerates display adapters via DXGI and reports each adapter's
// dedicated VRAM. DXGI is the only supported source on Windows because it:
//   - is vendor-agnostic (NVIDIA / AMD / Intel / iGPU all enumerate the same way),
//   - returns a human-readable adapter name in the same struct as the VRAM size
//     (no second API and no PCI matching needed),
//   - ships in-box on every supported Windows target (Win10/11, x64 and ARM64).
//
// We deliberately do NOT use the WMI Win32_VideoController.AdapterRAM field:
// it is a UINT32 capped at 4 GiB and is wrong for every modern dGPU.
//
// VRAM is a hardware property and never changes at runtime, but WHICH adapters
// enumerate is not settled at boot, so detection runs at startup and again
// whenever DXGI reports that the adapter set changed (adapterChangeSource
// below, consumed by the stats collector in stats_windows.go).

// IID_IDXGIFactory1 = {770aae78-f26f-4dba-a829-253c83d1b387}
// https://learn.microsoft.com/en-us/windows/win32/api/dxgi/nn-dxgi-idxgifactory1
var iidIDXGIFactory1 = windows.GUID{
	Data1: 0x770aae78,
	Data2: 0xf26f,
	Data3: 0x4dba,
	Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87},
}

const (
	// DXGI_ERROR_NOT_FOUND — returned by EnumAdapters1 once the adapter list
	// is exhausted. This is the loop-termination signal, not an error.
	dxgiErrorNotFound = 0x887A0002
	// DXGI_ADAPTER_FLAG_SOFTWARE — set on Microsoft's WARP / Basic Render
	// Driver. We skip those: they're not a physical GPU and reporting them
	// would mislead the UI into thinking the node has a usable accelerator.
	dxgiAdapterFlagSoftware = 0x2
)

// dxgiAdapterDesc1 mirrors the C DXGI_ADAPTER_DESC1 layout. SIZE_T members
// (the three *Memory fields) are 8 bytes on amd64/arm64 — our only Windows
// targets — so uintptr matches the C ABI exactly. Go's natural alignment
// inserts the same 4-byte gap between Revision (uint32) and DedicatedVideoMemory
// (uintptr) that the C compiler does, so no manual padding is required. A
// gpu_windows_test.go assertion pins the total size at 312 bytes to catch any
// accidental field-order regression.
type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	AdapterLuidLow        uint32
	AdapterLuidHigh       int32
	Flags                 uint32
}

// IID_IDXGIFactory7 = {a4966eed-76db-44da-84c1-ee9a7afb20a8}
// https://learn.microsoft.com/en-us/windows/win32/api/dxgi1_6/nn-dxgi1_6-idxgifactory7
var iidIDXGIFactory7 = windows.GUID{
	Data1: 0xa4966eed,
	Data2: 0x76db,
	Data3: 0x44da,
	Data4: [8]byte{0x84, 0xc1, 0xee, 0x9a, 0x7a, 0xfb, 0x20, 0xa8},
}

var (
	modDXGI                = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1 = modDXGI.NewProc("CreateDXGIFactory1")
	procCreateDXGIFactory2 = modDXGI.NewProc("CreateDXGIFactory2")
)

// COM vtable indices. Each interface inherits all methods from its parents,
// so the index is the cumulative count from IUnknown downward.
const (
	// IUnknown: QueryInterface(0), AddRef(1), Release(2)
	vtblIUnknownQueryInterface = 0
	vtblIUnknownRelease        = 2

	// IDXGIObject: SetPrivateData(3), SetPrivateDataInterface(4),
	//              GetPrivateData(5), GetParent(6)
	// IDXGIFactory: EnumAdapters(7), MakeWindowAssociation(8),
	//               GetWindowAssociation(9), CreateSwapChain(10),
	//               CreateSoftwareAdapter(11)
	// IDXGIFactory1: EnumAdapters1(12), IsCurrent(13)
	vtblIDXGIFactory1EnumAdapters1 = 12

	// IDXGIFactory2 (14-24, ending at CreateSwapChainForComposition),
	// IDXGIFactory3: GetCreationFlags(25), IDXGIFactory4: EnumAdapterByLuid(26),
	// EnumWarpAdapter(27), IDXGIFactory5: CheckFeatureSupport(28),
	// IDXGIFactory6: EnumAdapterByGpuPreference(29),
	// IDXGIFactory7: RegisterAdaptersChangedEvent(30),
	//                UnregisterAdaptersChangedEvent(31)
	vtblIDXGIFactory7RegisterAdaptersChangedEvent   = 30
	vtblIDXGIFactory7UnregisterAdaptersChangedEvent = 31

	// IDXGIObject (3-6) + IDXGIAdapter: EnumOutputs(7), GetDesc(8),
	//             CheckInterfaceSupport(9)
	// IDXGIAdapter1: GetDesc1(10)
	vtblIDXGIAdapter1GetDesc1 = 10
)

// COM interface pointers are kept as unsafe.Pointer (not uintptr) throughout.
// The objects are allocated by the system (DXGI), not by Go's runtime, so the
// GC never moves them — but go vet's `unsafeptr` analyzer cannot prove that
// for a uintptr round-trip and would flag every vtable dereference. Keeping
// them as unsafe.Pointer satisfies the analyzer and is the canonical pattern
// for hand-rolled COM in Go.

// vtableCall invokes the COM method at the given vtable index on `iface`.
// The first machine word of a COM object points at its method table; each
// table slot is a function pointer. The implicit `this` (= iface) is
// prepended to args automatically.
//
// Callers pass out-parameters as uintptr(unsafe.Pointer(&local)). Without the
// directive below that conversion hides the pointer from escape analysis, so
// the local stays on the goroutine stack; if the stack grows inside this call
// (append, SyscallN) the runtime copies it and the system writes the result to
// the old address, and the caller sees S_OK with its local still zero. The
// directive moves such pointees to the heap and keeps them alive for the call,
// as syscall.(*LazyProc).Call and x/sys/windows.(*Proc).Call do.
//
//go:uintptrescapes
func vtableCall(iface unsafe.Pointer, index int, args ...uintptr) uintptr {
	vtbl := *(**[64]uintptr)(iface)
	fn := vtbl[index]
	call := append([]uintptr{uintptr(iface)}, args...)
	r, _, _ := syscall.SyscallN(fn, call...)
	return r
}

func comRelease(iface unsafe.Pointer) {
	if iface == nil {
		return
	}
	vtableCall(iface, vtblIUnknownRelease)
}

func enumAdapters1(factory unsafe.Pointer, index uint32) (unsafe.Pointer, uintptr) {
	var adapter unsafe.Pointer
	hr := vtableCall(factory, vtblIDXGIFactory1EnumAdapters1,
		uintptr(index),
		uintptr(unsafe.Pointer(&adapter)),
	)
	return adapter, hr
}

func getDesc1(adapter unsafe.Pointer, desc *dxgiAdapterDesc1) uintptr {
	return vtableCall(adapter, vtblIDXGIAdapter1GetDesc1,
		uintptr(unsafe.Pointer(desc)),
	)
}

// luidUint64 packs a Windows LUID (LowPart + HighPart) into the QWORD form
// written under HKLM\SOFTWARE\Microsoft\DirectX\*\AdapterLuid.
func luidUint64(low uint32, high int32) uint64 {
	return uint64(uint32(high))<<32 | uint64(low)
}

// virtualDisplayNameFragments are case-insensitive substrings that mark
// Microsoft remoting / virtual display adapters. These are real WDDM
// adapters (not DXGI_ADAPTER_FLAG_SOFTWARE) so the SOFTWARE skip alone
// does not remove them; under RDP they show up beside the physical GPU.
var virtualDisplayNameFragments = []string{
	"remote display",
	"microsoft basic display",
	"virtual display adapter",
}

// isVirtualDisplayAdapter reports whether a DXGI adapter Description is a
// known Microsoft remoting/virtual display device (e.g. "Microsoft Remote
// Display Adapter") that should never be reported as a node GPU.
func isVirtualDisplayAdapter(name string) bool {
	lower := strings.ToLower(name)
	for _, frag := range virtualDisplayNameFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// loadPhysicalAdapterLUIDs returns the set of AdapterLuid QWORDs from
// HKLM\SOFTWARE\Microsoft\DirectX. Physical GPUs appear there; RDP
// phantom clones of the same card (same name/DeviceID, different LUID)
// do not. A nil map means the gate should not be applied (caller falls
// back to name-denylist-only).
func loadPhysicalAdapterLUIDs() (map[uint64]struct{}, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\DirectX`, registry.READ)
	if err != nil {
		return nil, err
	}
	defer key.Close()

	names, err := key.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}

	out := make(map[uint64]struct{}, len(names))
	for _, name := range names {
		sub, err := registry.OpenKey(key, name, registry.READ)
		if err != nil {
			continue
		}
		luid, _, err := sub.GetIntegerValue("AdapterLuid")
		sub.Close()
		if err != nil {
			continue
		}
		out[luid] = struct{}{}
	}
	return out, nil
}

// adapterCandidate is one DXGI adapter that has already passed the
// DXGI_ADAPTER_FLAG_SOFTWARE and virtual-display-name checks, i.e. everything
// that depends only on the adapter itself. luid is the packed value the
// DirectX registry gate is keyed by.
type adapterCandidate struct {
	gpu  GPUInfo
	luid uint64
}

// A failed DXGI factory leaves the inventory empty and detection runs again on
// the next adapter change, so warn only once. Registry filtering runs only after
// candidates have been found and always keeps at least one; its warnings are
// bounded by how rarely the adapter set changes, so they need no latches.
var dxgiFactoryWarned atomic.Bool

// selectPhysicalAdapters applies the DirectX-registry LUID gate to a whole
// enumeration, and declines to apply it when it would leave nothing behind.
// A missing or empty registry set leaves the candidates unchanged.
//
// The gate exists to drop RDP phantom clones: under a remote session the same
// card enumerates twice and only the real adapter's LUID is written under
// HKLM\SOFTWARE\Microsoft\DirectX. That case still keeps at least one
// candidate, so it filters exactly as it always did.
//
// The failure it guards against is a stale registry. LUIDs are reassigned on
// every boot, but those registry values are rewritten by the graphics stack
// roughly a minute after boot — so a service that enumerates inside that
// window is matching this boot's LUIDs against the previous boot's, no
// adapter matches, and the gate drops the entire inventory. A machine that
// just enumerated real adapters never has zero GPUs, so an empty result is
// proof the registry is stale rather than proof the adapters are phantoms:
// keep them all.
func selectPhysicalAdapters(candidates []adapterCandidate, physical map[uint64]struct{}) []adapterCandidate {
	if len(candidates) == 0 || len(physical) == 0 {
		return candidates
	}
	kept := make([]adapterCandidate, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := physical[c.luid]; ok {
			kept = append(kept, c)
			continue
		}
		slog.Debug("skipping DXGI adapter absent from DirectX registry",
			"name", c.gpu.Name, "luid", fmt.Sprintf("0x%016x", c.luid))
	}
	if len(kept) == 0 {
		slog.Warn("DirectX registry lists none of this boot's adapter LUIDs; treating it as stale and keeping every adapter",
			"candidates", len(candidates),
			"registry_luids", len(physical))
		return candidates
	}
	return kept
}

// physicalAdapterGate loads the registry LUID set for selectPhysicalAdapters,
// downgrading every failure to "no gate" (nil, which keeps all adapters).
func physicalAdapterGate() map[uint64]struct{} {
	physical, err := loadPhysicalAdapterLUIDs()
	if err != nil {
		slog.Warn("DirectX registry LUID gate unavailable; RDP phantom clones may appear",
			"err", err)
		return nil
	}
	if len(physical) == 0 {
		slog.Warn("DirectX registry listed no AdapterLuid values; skipping LUID gate")
		return nil
	}
	return physical
}

// enumerateAdapterCandidates walks DXGI once and returns every adapter that is
// neither a software renderer nor a Microsoft remoting/virtual display. The
// registry gate is left to the caller so selectPhysicalAdapters can be tested
// without DXGI.
func enumerateAdapterCandidates() []adapterCandidate {
	var factory unsafe.Pointer
	hr, _, _ := procCreateDXGIFactory1.Call(
		uintptr(unsafe.Pointer(&iidIDXGIFactory1)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hr != 0 || factory == nil {
		msg := "CreateDXGIFactory1 failed; no GPUs will be reported"
		if dxgiFactoryWarned.CompareAndSwap(false, true) {
			slog.Warn(msg, "hr", fmt.Sprintf("0x%08x", uint32(hr)))
		} else {
			slog.Debug(msg, "hr", fmt.Sprintf("0x%08x", uint32(hr)))
		}
		return nil
	}
	defer comRelease(factory)

	var candidates []adapterCandidate
	for i := uint32(0); ; i++ {
		adapter, hr := enumAdapters1(factory, i)
		if uint32(hr) == dxgiErrorNotFound {
			break
		}
		if hr != 0 || adapter == nil {
			slog.Warn("IDXGIFactory1::EnumAdapters1 failed; stopping enumeration",
				"idx", i, "hr", fmt.Sprintf("0x%08x", uint32(hr)))
			break
		}

		var desc dxgiAdapterDesc1
		descHR := getDesc1(adapter, &desc)
		comRelease(adapter)
		if descHR != 0 {
			slog.Warn("IDXGIAdapter1::GetDesc1 failed; skipping adapter",
				"idx", i, "hr", fmt.Sprintf("0x%08x", uint32(descHR)))
			continue
		}
		if desc.Flags&dxgiAdapterFlagSoftware != 0 {
			continue
		}

		name := windows.UTF16ToString(desc.Description[:])
		if isVirtualDisplayAdapter(name) {
			slog.Debug("skipping virtual/remote display adapter", "name", name)
			continue
		}

		candidates = append(candidates, adapterCandidate{
			gpu: GPUInfo{
				Name:      name,
				VramBytes: uint64(desc.DedicatedVideoMemory),
				statsKey:  luidKey(desc.AdapterLuidLow, desc.AdapterLuidHigh),
			},
			luid: luidUint64(desc.AdapterLuidLow, desc.AdapterLuidHigh),
		})
	}
	return candidates
}

// detectGPUs enumerates the host's display adapters and returns the ones that
// should be reported as node GPUs. Safe to call repeatedly: each call builds its
// own DXGI factory, so it sees the adapter set as it is now rather than the set
// a long-lived factory was created against. The Windows stats collector calls it
// again whenever DXGI signals an adapter change (stats_windows.go).
func detectGPUs() []GPUInfo {
	candidates := enumerateAdapterCandidates()
	if len(candidates) == 0 {
		return nil
	}
	selected := selectPhysicalAdapters(candidates, physicalAdapterGate())
	gpus := make([]GPUInfo, 0, len(selected))
	for _, c := range selected {
		gpus = append(gpus, c.gpu)
	}
	return gpus
}

// adapterChangeSource tells the stats collector that the DXGI adapter set
// changed. It is the seam between the Windows event plumbing and the collector's
// re-detection logic, so the logic can run against a fake in tests.
type adapterChangeSource interface {
	// Changed delivers a value each time the adapter set changed since the last
	// receive. Bursts may be merged into one value, never lost.
	Changed() <-chan struct{}
	// Close stops delivery and releases every OS resource. Safe to call once.
	Close()
}

// dxgiAdapterEvents is the production adapterChangeSource. It registers an
// auto-reset event with IDXGIFactory7::RegisterAdaptersChangedEvent, so DXGI
// itself says when an adapter appears, disappears, or has its LUID reissued
// (driver update or restart, GPU timeout and recovery, hot-plug) and nothing has
// to poll. A goroutine blocks in WaitForMultipleObjects on that event plus a
// private stop event and forwards each signal to the Changed channel.
//
// After each signal the registration moves to a fresh factory: an adapter
// change leaves the registering factory non-current (IsCurrent returns false),
// and DXGI's guidance is to recreate a non-current factory, so the watcher does
// not depend on a stale factory to keep signalling. The new registration is
// made before the old one is dropped, so there is no window without one. The
// factory is only used for the registration; re-detection builds its own
// (detectGPUs). After open, only the waiter goroutine touches factory and
// cookie, until Close has joined it.
type dxgiAdapterEvents struct {
	factory   unsafe.Pointer // IDXGIFactory7 holding the current registration
	cookie    uint32
	event     windows.Handle // auto-reset, signaled by DXGI
	stopEvent windows.Handle // manual-reset, signaled by Close
	changed   chan struct{}
	done      chan struct{}
}

func hrFailed(hr uintptr) bool { return int32(uint32(hr)) < 0 }

// registerAdapterEvent creates an IDXGIFactory7 and registers event with it. It
// returns an error when IDXGIFactory7 is unavailable (no CreateDXGIFactory2, or
// a Windows release before 10 1803) or the registration fails.
func registerAdapterEvent(event windows.Handle) (unsafe.Pointer, uint32, error) {
	if err := procCreateDXGIFactory2.Find(); err != nil {
		return nil, 0, fmt.Errorf("CreateDXGIFactory2 unavailable: %w", err)
	}
	var base unsafe.Pointer
	hr, _, _ := procCreateDXGIFactory2.Call(0,
		uintptr(unsafe.Pointer(&iidIDXGIFactory1)),
		uintptr(unsafe.Pointer(&base)),
	)
	if hrFailed(hr) || base == nil {
		return nil, 0, fmt.Errorf("CreateDXGIFactory2 failed: hr=0x%08x", uint32(hr))
	}
	var factory unsafe.Pointer
	hr = vtableCall(base, vtblIUnknownQueryInterface,
		uintptr(unsafe.Pointer(&iidIDXGIFactory7)),
		uintptr(unsafe.Pointer(&factory)),
	)
	comRelease(base)
	if hrFailed(hr) || factory == nil {
		return nil, 0, fmt.Errorf("IDXGIFactory7 unavailable: hr=0x%08x", uint32(hr))
	}
	var cookie uint32
	hr = vtableCall(factory, vtblIDXGIFactory7RegisterAdaptersChangedEvent,
		uintptr(event),
		uintptr(unsafe.Pointer(&cookie)),
	)
	if hrFailed(hr) {
		comRelease(factory)
		return nil, 0, fmt.Errorf("RegisterAdaptersChangedEvent failed: hr=0x%08x", uint32(hr))
	}
	return factory, cookie, nil
}

// unregisterAdapterEvent undoes registerAdapterEvent. A nil factory is a no-op.
func unregisterAdapterEvent(factory unsafe.Pointer, cookie uint32) {
	if factory == nil {
		return
	}
	vtableCall(factory, vtblIDXGIFactory7UnregisterAdaptersChangedEvent, uintptr(cookie))
	comRelease(factory)
}

// openDXGIAdapterEvents creates the two events and the first registration. Its
// error means IDXGIFactory7 is unavailable or the registration failed; the
// collector then falls back to a single deferred re-detection.
func openDXGIAdapterEvents() (adapterChangeSource, error) {
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateEvent: %w", err)
	}
	stopEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(event)
		return nil, fmt.Errorf("CreateEvent: %w", err)
	}
	factory, cookie, err := registerAdapterEvent(event)
	if err != nil {
		_ = windows.CloseHandle(event)
		_ = windows.CloseHandle(stopEvent)
		return nil, err
	}
	slog.Info("DXGI adapters-changed event registered", "cookie", cookie)

	s := &dxgiAdapterEvents{
		factory:   factory,
		cookie:    cookie,
		event:     event,
		stopEvent: stopEvent,
		changed:   make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	go s.wait()
	return s, nil
}

func (s *dxgiAdapterEvents) Changed() <-chan struct{} { return s.changed }

// wait forwards each adapters-changed signal until Close signals the stop
// event, re-arming the registration after each one.
func (s *dxgiAdapterEvents) wait() {
	defer close(s.done)
	handles := []windows.Handle{s.event, s.stopEvent}
	for {
		r, err := windows.WaitForMultipleObjects(handles, false, windows.INFINITE)
		if err != nil {
			slog.Warn("waiting for the DXGI adapters-changed event failed; adapter changes will no longer be noticed", "err", err)
			return
		}
		if r != windows.WAIT_OBJECT_0 {
			return // stop event
		}
		select {
		case s.changed <- struct{}{}:
		default: // one change is already pending; the receiver will see it
		}
		s.rearm()
	}
}

// rearm moves the registration to a fresh factory. If that fails the previous
// registration is kept, so the watcher degrades to whatever the old factory
// still signals instead of going silent.
func (s *dxgiAdapterEvents) rearm() {
	factory, cookie, err := registerAdapterEvent(s.event)
	if err != nil {
		slog.Warn("re-arming the DXGI adapters-changed event failed; keeping the previous registration", "err", err)
		return
	}
	unregisterAdapterEvent(s.factory, s.cookie)
	s.factory, s.cookie = factory, cookie
	slog.Debug("DXGI adapters-changed event re-armed", "cookie", cookie)
}

func (s *dxgiAdapterEvents) Close() {
	_ = windows.SetEvent(s.stopEvent)
	<-s.done
	unregisterAdapterEvent(s.factory, s.cookie)
	s.factory = nil
	_ = windows.CloseHandle(s.event)
	_ = windows.CloseHandle(s.stopEvent)
}
