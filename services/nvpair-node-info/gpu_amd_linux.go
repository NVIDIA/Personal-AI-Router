// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Linux AMD GPU inventory, read straight from the amdgpu driver's sysfs nodes.
//
// The NVIDIA path (gpu_linux.go) gets name, capacity and a join key from
// nvidia-smi. AMD ships no equivalent tool in a default install, so a Radeon
// host used to fall through to ghw, which reports the PCI database's codename
// ("Barcelo") and nothing else: no capacity, no join key, and therefore no
// dynamic VRAM / utilization either.
//
// Everything needed is already in sysfs, world-readable, with no daemon and no
// cgo:
//
//	/sys/class/drm/card<N>/device/
//	    vendor, device              PCI ids ("0x1002", "0x15e7")
//	    uevent                      DRIVER=, PCI_SLOT_NAME=
//	    mem_info_vram_total         dedicated VRAM, bytes
//	    mem_info_gtt_total          GTT (system memory the GPU may map), bytes
//	    mem_info_vram_used          \ sampled every collector tick
//	    mem_info_gtt_used           /
//	    gpu_busy_percent            0..100 busy fraction
//
// On an APU the "VRAM" figure is only the carve-out the firmware reserved
// (512 MiB on the Ryzen 5 5625U this was measured on) while the real ceiling
// for a model is that carve-out plus the GTT aperture, so capacity is reported
// as vram_total + gtt_total for those parts and as vram_total alone for a
// discrete card. That pool is shared with system RAM, but it is NOT the same
// thing as GPUInfo.usesSystemMemoryUsage, which substitutes whole-system RAM
// usage for a GPU whose driver cannot report its own: amdgpu reports its own
// usage precisely, so unified rows carry their own flag and their used figure
// stays the driver's (vram_used + gtt_used).
//
// Sampling lives in stats_amd_linux.go. Both halves derive their join key and
// their unified/discrete verdict from listAMDCards, so a row and its samples
// can never disagree.

const (
	drmClassDir = "/sys/class/drm"

	// amdPCIVendor is AMD/ATI's PCI vendor id as the sysfs attribute spells it.
	amdPCIVendor = "0x1002"

	// amdStatsKeyPrefix namespaces the statsKey so it can never collide with an
	// nvidia-smi UUID or an "apex:" accelerator key. The "amd:<pci address>"
	// form is shared with the upstream amdgpu inventory work so both
	// implementations join against the same key.
	amdStatsKeyPrefix = "amd:"

	// amdAPUVRAMCeiling is the dedicated-VRAM figure below which a card absent
	// from amdModels is treated as an APU carve-out rather than a discrete
	// card's real memory. No discrete Radeon ships with under 1 GiB; every APU
	// carve-out observed is 512 MiB or less.
	amdAPUVRAMCeiling = 1 << 30
)

// amdModel is one known PCI device id: the name to publish, and whether the
// part is an APU (a VRAM carve-out plus GTT) rather than a discrete card.
type amdModel struct {
	name string
	apu  bool
}

// amdModels maps a PCI device id (lowercase hex, no "0x") to its marketing
// name. It exists because the codename sysfs and the PCI database report
// ("Barcelo", "Cezanne") is not something a user can match to their machine.
// An unlisted id still produces a row - see amdModelName - so a part released
// after this table was written is never dropped from the inventory.
var amdModels = map[string]amdModel{
	"15e7": {"AMD Radeon Graphics (Vega 7, Barcelo)", true},
	"1638": {"AMD Radeon Graphics (Cezanne)", true},
	"164c": {"AMD Radeon Graphics (Lucienne)", true},
	"1636": {"AMD Radeon Graphics (Renoir)", true},
	"15d8": {"AMD Radeon Graphics (Picasso)", true},
	"15dd": {"AMD Radeon Graphics (Raven)", true},
	"1681": {"AMD Radeon Graphics (Rembrandt)", true},
	"15bf": {"AMD Radeon Graphics (Phoenix)", true},
	"15c8": {"AMD Radeon Graphics (Phoenix2)", true},
	"150e": {"AMD Radeon Graphics (Strix)", true},
}

// amdCard is one enumerated AMD adapter: where its attributes live, what it
// is, and the key its samples are published under.
type amdCard struct {
	card      string // DRM node name, e.g. "card1"
	index     int    // the N of card<N>, for a stable numeric inventory order
	deviceDir string // <drmRoot>/card<N>/device
	deviceID  string // PCI device id, lowercase hex without "0x", e.g. "15e7"

	// unifiedPool marks a row whose capacity is the VRAM carve-out plus the
	// GTT aperture (an APU) rather than dedicated memory (a discrete card).
	// Deliberately separate from GPUInfo.usesSystemMemoryUsage: this pool's
	// usage comes from the driver, not from /proc/meminfo.
	unifiedPool bool

	// statsKey is "amd:<pci address>", the join key between the static row and
	// the collector's per-tick sample.
	statsKey string
}

// amdDRMUnreadable logs the "no /sys/class/drm at all" case once. A host with
// no amdgpu (an NVIDIA-only box, a container without sysfs) must stay silent
// tick after tick.
var amdDRMUnreadable sync.Once

// detectAMDGPUs returns one inventory row per AMD adapter under drmRoot, in
// card-number order. Capacity follows the unified/discrete rule described at
// the top of this file; the dynamic fields are left zero for buildResponse to
// fill from the collector's snapshot under statsKey.
func detectAMDGPUs(drmRoot string) []GPUInfo {
	var out []GPUInfo
	for _, c := range listAMDCards(drmRoot) {
		out = append(out, GPUInfo{
			Name:      amdModelName(c.deviceID),
			VramBytes: amdCapacityBytes(c),
			statsKey:  c.statsKey,
		})
	}
	return out
}

// detectAMDOrGHWGPUs is the inventory path for a host where nvidia-smi
// produced nothing. amdgpu's sysfs nodes carry a name, a real capacity and a
// join key, so they supersede the ghw adapter list whenever any AMD card is
// present; a host with no AMD card keeps the previous ghw behavior (names
// only, no dynamic stats).
func detectAMDOrGHWGPUs() []GPUInfo {
	if amd := detectAMDGPUs(drmClassDir); len(amd) > 0 {
		return amd
	}
	return detectGPUsGHW()
}

// listAMDCards enumerates the AMD adapters under drmRoot. Only card<N>
// directories are considered: the DRM class also holds one card<N>-<CONNECTOR>
// entry per output (card1-DP-1, card1-HDMI-A-1, ...) plus renderD<N> nodes,
// all of which point back at the same device and would otherwise list the same
// GPU several times. Non-AMD vendors are skipped, so this is safe to call on
// any host.
func listAMDCards(drmRoot string) []amdCard {
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		amdDRMUnreadable.Do(func() {
			slog.Debug("DRM class dir unreadable; no AMD GPU inventory", "dir", drmRoot, "err", err)
		})
		return nil
	}
	var cards []amdCard
	for _, e := range entries {
		index, ok := amdCardIndex(e.Name())
		if !ok {
			continue
		}
		deviceDir := filepath.Join(drmRoot, e.Name(), "device")
		if !strings.EqualFold(sysfsField(filepath.Join(deviceDir, "vendor")), amdPCIVendor) {
			continue
		}
		c := amdCard{
			card:      e.Name(),
			index:     index,
			deviceDir: deviceDir,
			deviceID:  amdDeviceID(deviceDir),
		}
		c.unifiedPool = amdUnifiedPool(c)
		c.statsKey = amdStatsKey(deviceDir, e.Name())
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].index < cards[j].index })
	return cards
}

// amdCardIndex reports the N of a "card<N>" DRM node name. It rejects the
// connector directories ("card1-DP-1") and every other class entry.
func amdCardIndex(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, "card")
	if !ok || digits == "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// amdDeviceID returns the PCI device id as lowercase hex without the "0x"
// prefix ("15e7"), or "" when the attribute is missing or malformed.
func amdDeviceID(deviceDir string) string {
	id, ok := strings.CutPrefix(strings.ToLower(sysfsField(filepath.Join(deviceDir, "device"))), "0x")
	if !ok || !isHex(id) {
		return ""
	}
	return id
}

// amdModelName resolves a device id to a display name. An unknown id keeps the
// id in the name rather than being dropped or published as a bare codename,
// which is meaningless to a user reading a node list.
func amdModelName(deviceID string) string {
	if m, ok := amdModels[deviceID]; ok {
		return m.name
	}
	if deviceID == "" {
		return "AMD Radeon Graphics"
	}
	return "AMD Radeon Graphics (0x" + deviceID + ")"
}

// amdUnifiedPool reports whether this card's usable memory is the VRAM
// carve-out plus the GTT aperture. A listed part is taken at its word; an
// unlisted one is judged by its own numbers, because no discrete Radeon
// exposes under 1 GiB of VRAM.
func amdUnifiedPool(c amdCard) bool {
	if m, ok := amdModels[c.deviceID]; ok {
		return m.apu
	}
	vram, haveVRAM := amdSysfsUint(c.deviceDir, "mem_info_vram_total")
	_, haveGTT := amdSysfsUint(c.deviceDir, "mem_info_gtt_total")
	return haveVRAM && haveGTT && vram < amdAPUVRAMCeiling
}

// amdCapacityBytes is the card's reportable vram_bytes: carve-out + GTT on a
// unified part, dedicated VRAM on a discrete card. Zero when the driver
// reports neither, which omitempty then drops from the wire.
func amdCapacityBytes(c amdCard) uint64 {
	vram, _ := amdSysfsUint(c.deviceDir, "mem_info_vram_total")
	if !c.unifiedPool {
		return vram
	}
	gtt, _ := amdSysfsUint(c.deviceDir, "mem_info_gtt_total")
	return vram + gtt
}

// amdStatsKey builds the "amd:<pci address>" join key. The class entry's
// `device` is a symlink into /sys/bus/pci/devices, so the resolved directory
// name is the address; uevent's PCI_SLOT_NAME is the fallback for a sysfs view
// where the link cannot be resolved, and the DRM node name is the last resort
// so a card is never published without a key (which would cost it every
// dynamic field). Both the inventory and the sampler call this, so they always
// agree.
func amdStatsKey(deviceDir, card string) string {
	if resolved, err := filepath.EvalSymlinks(deviceDir); err == nil {
		if base := filepath.Base(resolved); isPCIAddress(base) {
			return amdStatsKeyPrefix + base
		}
	}
	if slot := ueventValue(readSysfs(filepath.Join(deviceDir, "uevent")), "PCI_SLOT_NAME"); isPCIAddress(slot) {
		return amdStatsKeyPrefix + slot
	}
	return amdStatsKeyPrefix + card
}

// ueventValue pulls one KEY=value line out of a sysfs uevent file.
func ueventValue(uevent, key string) string {
	for _, line := range strings.Split(uevent, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return v
		}
	}
	return ""
}

// isPCIAddress reports whether s has the domain:bus:device.function shape
// ("0000:04:00.0"). Checked so an unexpected sysfs layout produces the
// card-name fallback instead of a nonsense key.
func isPCIAddress(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return false
	}
	devFn := strings.Split(parts[2], ".")
	if len(devFn) != 2 {
		return false
	}
	for _, field := range []string{parts[0], parts[1], devFn[0], devFn[1]} {
		if !isHex(field) {
			return false
		}
	}
	return true
}

// isHex reports whether s is a non-empty run of hexadecimal digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// sysfsField reads a sysfs attribute and trims the trailing newline the kernel
// appends to every one of them.
func sysfsField(path string) string {
	return strings.TrimSpace(readSysfs(path))
}

// amdSysfsUint reads a decimal unsigned attribute from a card's device
// directory. ok is false when the file is missing, empty or not a number, so
// callers can tell "zero" from "unknown".
func amdSysfsUint(deviceDir, attr string) (uint64, bool) {
	v, err := strconv.ParseUint(sysfsField(filepath.Join(deviceDir, attr)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readSysfs returns a sysfs attribute's contents, or "" when unreadable.
func readSysfs(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
