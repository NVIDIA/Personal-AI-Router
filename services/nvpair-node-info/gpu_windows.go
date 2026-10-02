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
// enumerate is not settled at boot, so detection runs at startup and is then
// re-run on a timer by the stats collector (stats_windows.go).

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

var (
	modDXGI                = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1 = modDXGI.NewProc("CreateDXGIFactory1")
)

// COM vtable indices. Each interface inherits all methods from its parents,
// so the index is the cumulative count from IUnknown downward.
const (
	// IUnknown: QueryInterface(0), AddRef(1), Release(2)
	vtblIUnknownRelease = 2

	// IDXGIObject: SetPrivateData(3), SetPrivateDataInterface(4),
	//              GetPrivateData(5), GetParent(6)
	// IDXGIFactory: EnumAdapters(7), MakeWindowAssociation(8),
	//               GetWindowAssociation(9), CreateSwapChain(10),
	//               CreateSoftwareAdapter(11)
	// IDXGIFactory1: EnumAdapters1(12), IsCurrent(13)
	vtblIDXGIFactory1EnumAdapters1 = 12

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

// keepPhysicalAdapter applies the DirectX-registry LUID gate to a single
// adapter. When physical is nil or empty the gate is skipped (the name
// denylist still runs); otherwise only LUIDs present in the registry are
// kept. Callers go through selectPhysicalAdapters, which additionally
// refuses a gate that would empty the whole inventory.
func keepPhysicalAdapter(luid uint64, physical map[uint64]struct{}) bool {
	if len(physical) == 0 {
		return true
	}
	_, ok := physical[luid]
	return ok
}

// adapterCandidate is one DXGI adapter that has already passed the
// DXGI_ADAPTER_FLAG_SOFTWARE and virtual-display-name checks, i.e. everything
// that depends only on the adapter itself. Its LUID is carried in both forms
// a consumer may need: the packed QWORD the DirectX registry gate is keyed
// by, and the DWORD halves D3DKMT takes when a per-adapter query (a PCI
// address lookup for a vendor tool) resolves it.
type adapterCandidate struct {
	gpu      GPUInfo
	luid     uint64
	luidLow  uint32
	luidHigh int32
}

// One-shot warning latches. Adapter detection now re-runs on a timer
// (stats_windows.go), and each of these conditions persists until something
// outside this process changes, so a plain slog.Warn in the path would repeat
// the same line for the life of the service.
var (
	dxgiFactoryWarned        atomic.Bool
	luidGateUnreadableWarned atomic.Bool
	luidGateEmptyWarned      atomic.Bool
	staleLUIDGateWarned      atomic.Bool
)

// selectPhysicalAdapters applies the DirectX-registry LUID gate to a whole
// enumeration, and declines to apply it when it would leave nothing behind.
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
// keep them all and let a later refresh apply the gate normally once the keys
// catch up.
func selectPhysicalAdapters(candidates []adapterCandidate, physical map[uint64]struct{}) []adapterCandidate {
	if len(candidates) == 0 || len(physical) == 0 {
		return candidates
	}
	kept := make([]adapterCandidate, 0, len(candidates))
	for _, c := range candidates {
		if keepPhysicalAdapter(c.luid, physical) {
			kept = append(kept, c)
			continue
		}
		slog.Debug("skipping DXGI adapter absent from DirectX registry",
			"name", c.gpu.Name, "luid", fmt.Sprintf("0x%016x", c.luid))
	}
	if len(kept) == 0 {
		if staleLUIDGateWarned.CompareAndSwap(false, true) {
			slog.Warn("DirectX registry lists none of this boot's adapter LUIDs; treating it as stale and keeping every adapter",
				"candidates", len(candidates),
				"registry_luids", len(physical))
		}
		return candidates
	}
	return kept
}

// physicalAdapterGate loads the registry LUID set for selectPhysicalAdapters,
// downgrading every failure to "no gate" (nil, which keeps all adapters).
func physicalAdapterGate() map[uint64]struct{} {
	physical, err := loadPhysicalAdapterLUIDs()
	if err != nil {
		if luidGateUnreadableWarned.CompareAndSwap(false, true) {
			slog.Warn("DirectX registry LUID gate unavailable; RDP phantom clones may appear",
				"err", err)
		}
		return nil
	}
	if len(physical) == 0 {
		if luidGateEmptyWarned.CompareAndSwap(false, true) {
			slog.Warn("DirectX registry listed no AdapterLuid values; skipping LUID gate")
		}
		return nil
	}
	return physical
}

// enumerateAdapterCandidates walks DXGI once and returns every adapter that is
// neither a software renderer nor a Microsoft remoting/virtual display. The
// registry gate is left to the caller so every consumer of this list
// (detectGPUs today; any per-adapter join later) shares one enumeration and
// cannot drift apart on what counts as an adapter.
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
			luid:     luidUint64(desc.AdapterLuidLow, desc.AdapterLuidHigh),
			luidLow:  desc.AdapterLuidLow,
			luidHigh: desc.AdapterLuidHigh,
		})
	}
	return candidates
}

// detectGPUs enumerates the host's display adapters and returns the ones that
// should be reported as node GPUs. Safe to call repeatedly: the Windows stats
// collector re-runs it on a timer so an enumeration that came up empty at boot
// recovers without a restart (stats_windows.go).
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
