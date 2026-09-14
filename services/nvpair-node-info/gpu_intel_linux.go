// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jaypipes/ghw"
)

// Intel discrete and integrated GPU detection on Linux.
//
// Data sources, in preference order:
//
//  1. xpu-smi (Intel's official CLI, part of the oneAPI Level Zero
//     userspace stack). When installed it returns a stable JSON view of
//     every Intel GPU on the host — dGPU (Arc, Data Center Max/Flex) or
//     iGPU — with a PCI BDF address we key on and, where the driver
//     reports it, a total-VRAM figure.
//
//  2. sysfs (/sys/class/drm/card*). Present on every host with an Intel
//     GPU regardless of extra software. We enumerate cards whose vendor
//     is Intel (0x8086) and pull the marketing name via ghw, plus any
//     mem_info_vram_total the DRM driver exposes (Arc dGPUs report it;
//     iGPUs share system RAM and do not).
//
// The dynamic counterparts — VRAM used and busy percent — live in
// stats_linux.go under the same two-tier preference: xpu-smi for full
// telemetry, sysfs mem_info_vram_used as a memory-only fallback. When
// neither source can be read the Intel GPU still appears in the
// inventory, just without live stats — mirroring the same "unknown
// means omitted" convention nvidia-smi degradation follows.
//
// Every Intel adapter is stamped with a PCI BDF-derived statsKey
// ("intel-pci-0000:03:00.0") so the stats collector can join dynamic
// samples back to the static GPUInfo the same way nvidia-smi joins by
// UUID. The prefix keeps the key space disjoint from nvidia-smi UUIDs.

const (
	// xpuSmiTimeout caps how long we wait for one xpu-smi invocation.
	// The tool is usually well under 200 ms, but a wedged Level Zero
	// driver can hang; a hard ceiling keeps the per-tick stats collector
	// from blocking indefinitely.
	xpuSmiTimeout = 3 * time.Second

	// intelPCIVendorID is the PCI vendor ID assigned to Intel. Every
	// entry under /sys/class/drm/card*/device/vendor reports this exact
	// literal (lowercase, "0x" prefix) for an Intel GPU.
	intelPCIVendorID = "0x8086"

	// intelStatsKeyPrefix disambiguates Intel adapter keys from nvidia-smi
	// UUIDs in the shared stats map. Keep any prefix change in lock-step
	// with the stats collector.
	intelStatsKeyPrefix = "intel-pci-"
)

// xpuSmiDiscoveryDevice mirrors the subset of xpu-smi's `discovery --json`
// entries we consume. Only the fields relevant to inventory are pulled;
// extra keys the tool may add in future versions are ignored by
// encoding/json.
//
// memory_physical_size_byte is present on discrete Intel GPUs (Arc,
// Data Center) and absent (or zero) on iGPUs that share system memory.
// It arrives as either a numeric or string on different xpu-smi builds,
// so we parse it defensively in intelMemoryBytes.
type xpuSmiDiscoveryDevice struct {
	DeviceID                int             `json:"device_id"`
	DeviceName              string          `json:"device_name"`
	DeviceType              string          `json:"device_type"`
	DeviceFunctionType      string          `json:"device_function_type"`
	UUID                    string          `json:"uuid"`
	PCIBDFAddress           string          `json:"pci_bdf_address"`
	PCIDeviceID             string          `json:"pci_device_id"`
	VendorName              string          `json:"vendor_name"`
	MemoryPhysicalSizeByte  json.RawMessage `json:"memory_physical_size_byte"`
}

type xpuSmiDiscoveryEnvelope struct {
	DeviceList []xpuSmiDiscoveryDevice `json:"device_list"`
}

// detectIntelGPUs returns every Intel GPU the host exposes, or nil when
// no Intel adapter can be identified. Prefers xpu-smi, falls back to a
// sysfs walk. A nil return means "no Intel GPU found by any source"; an
// empty slice is never returned.
func detectIntelGPUs() []GPUInfo {
	if gpus, ok := detectIntelViaXpuSmi(); ok {
		return gpus
	}
	return detectIntelViaSysfs()
}

// detectIntelViaXpuSmi runs `xpu-smi discovery --json` and folds the
// device list into GPUInfo. Returns ok=false when the binary is not on
// PATH, the invocation fails or times out, or the payload is empty —
// callers then fall through to the sysfs walk.
//
// Physical adapters only: xpu-smi enumerates SR-IOV virtual functions
// alongside their parent under the same PCI device, and we skip those
// via device_function_type so a single physical Arc doesn't appear
// multiple times in the inventory.
func detectIntelViaXpuSmi() ([]GPUInfo, bool) {
	out, err := xpuSmiJSON(context.Background(), "discovery", "--json")
	if err != nil {
		slog.Debug("xpu-smi discovery unavailable", "err", err)
		return nil, false
	}
	gpus, ok := parseXpuSmiDiscovery(out)
	if !ok || len(gpus) == 0 {
		return nil, false
	}
	return gpus, true
}

// parseXpuSmiDiscovery decodes the JSON envelope. Returns ok=false when
// the payload does not conform to the discovery shape — a version of
// xpu-smi that emits an unrelated JSON document should degrade to the
// sysfs fallback rather than surface as "no Intel GPUs".
func parseXpuSmiDiscovery(out []byte) ([]GPUInfo, bool) {
	var env xpuSmiDiscoveryEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		slog.Debug("xpu-smi discovery JSON parse failed", "err", err)
		return nil, false
	}
	gpus := make([]GPUInfo, 0, len(env.DeviceList))
	for _, d := range env.DeviceList {
		if !isPhysicalIntelGPU(d) {
			continue
		}
		key := intelStatsKeyFromBDF(d.PCIBDFAddress)
		if key == "" {
			continue
		}
		name := strings.TrimSpace(d.DeviceName)
		if name == "" {
			name = "Intel GPU"
		}
		vram := intelMemoryBytes(d.MemoryPhysicalSizeByte)
		if vram == 0 {
			// Some xpu-smi builds omit memory_physical_size_byte for
			// discrete Arc adapters. Ask the DRM ioctl directly the same
			// way the sysfs path does — cheap, reliable, no extra deps.
			if node := intelRenderNodeForBDF(strings.TrimPrefix(key, intelStatsKeyPrefix)); node != "" {
				if total, _, ok := intelVRAMFromDRM(node); ok {
					vram = total
				}
			}
		}
		gpus = append(gpus, GPUInfo{
			Name:      name,
			VramBytes: vram,
			statsKey:  key,
		})
	}
	return gpus, true
}

// isPhysicalIntelGPU filters the xpu-smi device list to physical GPU
// entries. device_type distinguishes GPU from other Intel accelerators;
// device_function_type separates physical adapters from SR-IOV virtual
// functions ("virtual"). A missing device_function_type is treated as
// physical for compatibility with xpu-smi builds that don't emit it.
func isPhysicalIntelGPU(d xpuSmiDiscoveryDevice) bool {
	if !strings.EqualFold(strings.TrimSpace(d.DeviceType), "GPU") &&
		d.DeviceType != "" {
		return false
	}
	fn := strings.ToLower(strings.TrimSpace(d.DeviceFunctionType))
	switch fn {
	case "", "physical":
		return true
	default:
		return false
	}
}

// intelMemoryBytes decodes memory_physical_size_byte. xpu-smi has
// emitted this field as a number ("1699966976") on some builds and as
// a string ("\"1699966976\"") on others; a missing value or zero is
// treated as "unknown" (VramBytes stays 0, which the omitempty tag
// drops from the wire).
func intelMemoryBytes(raw json.RawMessage) uint64 {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return 0
	}
	trimmed = strings.Trim(trimmed, `"`)
	v, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// intelStatsKeyFromBDF normalizes a PCI BDF address to the statsKey the
// stats collector joins on. xpu-smi and sysfs both spell the address
// lowercase already, but we normalize defensively so a version that
// reports uppercase (or omits the domain prefix) still joins.
func intelStatsKeyFromBDF(bdf string) string {
	bdf = strings.ToLower(strings.TrimSpace(bdf))
	if bdf == "" {
		return ""
	}
	// Some xpu-smi builds omit the "0000:" domain segment.
	if !strings.Contains(bdf, ":") {
		return ""
	}
	if strings.Count(bdf, ":") == 1 {
		bdf = "0000:" + bdf
	}
	return intelStatsKeyPrefix + bdf
}

// xpuSmiJSON runs the xpu-smi CLI with the supplied argv and returns
// stdout. A missing binary (not on PATH) surfaces as an exec error,
// which callers treat as "no xpu-smi telemetry available" and degrade
// silently to sysfs.
func xpuSmiJSON(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, xpuSmiTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "xpu-smi", args...).Output()
}

// detectIntelViaSysfs enumerates /sys/class/drm/card* and returns one
// GPUInfo per Intel PCI device. Marketing names come from ghw (which
// uses the shared pcidb), so a card whose device ID is not in the
// bundled DB still appears — just with a generic name.
//
// mem_info_vram_total is present on Arc dGPUs (i915 and xe drivers).
// iGPUs share system RAM and don't report it, so VramBytes stays 0
// there and the omitempty tag drops the field from JSON, matching the
// existing behavior for unknown VRAM.
func detectIntelViaSysfs() []GPUInfo {
	cards, err := filepath.Glob("/sys/class/drm/card*")
	if err != nil {
		slog.Debug("sysfs drm scan failed", "err", err)
		return nil
	}
	// filepath.Glob returns entries in lexical order already, but be
	// explicit so cardN ordering is stable even if the pattern semantics
	// ever change.
	sort.Strings(cards)

	seen := make(map[string]struct{})
	var gpus []GPUInfo
	names := intelDeviceNamesFromGHW()

	for _, card := range cards {
		// Skip connector entries like /sys/class/drm/card0-DP-1: only
		// the bare cardN symlink has a device/ directory.
		base := filepath.Base(card)
		if strings.Contains(base, "-") {
			continue
		}
		vendor, err := os.ReadFile(filepath.Join(card, "device", "vendor"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(vendor)) != intelPCIVendorID {
			continue
		}
		bdf, err := readSysfsBDF(card)
		if err != nil || bdf == "" {
			continue
		}
		key := intelStatsKeyPrefix + bdf
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		name := names[bdf]
		if name == "" {
			name = "Intel GPU"
		}
		vram := readSysfsVRAMTotal(card)
		if vram == 0 {
			// Stock i915 builds don't expose mem_info_vram_total; fall back
			// to the DRM_IOCTL_I915_QUERY memory-regions query the way
			// intel_gpu_top does. Reports real Arc VRAM (6/8/16 GiB) even
			// when Resizable BAR is off and sysfs shows nothing.
			if node := intelRenderNode(card); node != "" {
				if total, _, ok := intelVRAMFromDRM(node); ok {
					vram = total
				}
			}
		}
		gpus = append(gpus, GPUInfo{
			Name:      name,
			VramBytes: vram,
			statsKey:  key,
		})
	}
	return gpus
}

// readSysfsBDF resolves the PCI BDF address a DRM card is attached to.
// The `device` entry under /sys/class/drm/cardN is a symlink into
// /sys/devices/pci*/..., whose final component is the BDF.
func readSysfsBDF(cardPath string) (string, error) {
	target, err := os.Readlink(filepath.Join(cardPath, "device"))
	if err != nil {
		return "", err
	}
	bdf := strings.ToLower(filepath.Base(target))
	if !looksLikePCIBDF(bdf) {
		return "", nil
	}
	return bdf, nil
}

// looksLikePCIBDF applies a cheap shape check so we don't accept a
// non-PCI final path segment (e.g. a platform device) as an adapter
// key. Real BDFs look like "0000:03:00.0" — four colons-and-dots
// separated hex fields. Full lexical validation would add no signal
// beyond the sysfs vendor filter that already gated us here.
func looksLikePCIBDF(s string) bool {
	if !strings.Contains(s, ":") || !strings.Contains(s, ".") {
		return false
	}
	return strings.Count(s, ":") == 2
}

// readSysfsVRAMTotal returns the DRM-reported total VRAM in bytes, or
// zero when the driver doesn't expose it (iGPU) or the read fails.
func readSysfsVRAMTotal(cardPath string) uint64 {
	data, err := os.ReadFile(filepath.Join(cardPath, "device", "mem_info_vram_total"))
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// readSysfsVRAMUsed returns the DRM-reported bytes of VRAM in use, or
// (0, false) when the driver doesn't expose it or the read fails. This
// is the sysfs counterpart to xpu-smi's dynamic stats and is consumed
// from stats_linux.go.
func readSysfsVRAMUsed(cardPath string) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(cardPath, "device", "mem_info_vram_used"))
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// intelDeviceNamesFromGHW returns a map from PCI BDF ("0000:03:00.0")
// to marketing name for every Intel graphics adapter ghw can see. ghw
// already parses the shared pcidb; we reuse that here rather than
// re-shipping the mapping ourselves. A ghw failure returns an empty
// map — callers substitute "Intel GPU" for any adapter whose name
// isn't found.
func intelDeviceNamesFromGHW() map[string]string {
	out := map[string]string{}
	gpu, err := ghw.GPU()
	if err != nil {
		slog.Debug("ghw graphics enumeration failed for Intel name lookup", "err", err)
		return out
	}
	for _, card := range gpu.GraphicsCards {
		if card.DeviceInfo == nil || card.DeviceInfo.Vendor == nil {
			continue
		}
		// pcidb reports vendor IDs as unprefixed lowercase hex ("8086");
		// sysfs and xpu-smi use "0x8086". Compare against the sysfs
		// form so both sides key on the same literal.
		vendorID := "0x" + strings.ToLower(strings.TrimSpace(card.DeviceInfo.Vendor.ID))
		if vendorID != intelPCIVendorID {
			continue
		}
		bdf := strings.ToLower(strings.TrimSpace(card.Address))
		if bdf == "" {
			continue
		}
		// ghw sometimes reports the BDF without the "0000:" PCI domain;
		// normalize to the same shape sysfs uses so the lookup joins.
		if strings.Count(bdf, ":") == 1 {
			bdf = "0000:" + bdf
		}
		if card.DeviceInfo.Product == nil || card.DeviceInfo.Product.Name == "" {
			continue
		}
		out[bdf] = card.DeviceInfo.Product.Name
	}
	return out
}
