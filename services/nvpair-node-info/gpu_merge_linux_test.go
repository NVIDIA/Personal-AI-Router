// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMergeGPUInventoryUsesPCIIdentity(t *testing.T) {
	vendor := []GPUInfo{
		{Name: "Arc", statsKey: "intel-pci-0000:03:00.0", pciAddress: "0000:03:00.0", VramBytes: 16},
		{Name: "Arc", statsKey: "intel-pci-0000:04:00.0", pciAddress: "0000:04:00.0", VramBytes: 8},
		{Name: "NVIDIA", statsKey: "GPU-uuid", pciAddress: "00000000:05:00.0"},
	}
	generic := []GPUInfo{
		{Name: "AMD", pciAddress: "0000:06:00.0"},
		{Name: "Generic Arc", pciAddress: "03:00.0"},
		{Name: "Generic Arc", pciAddress: "0000:04:00.0"},
		{Name: "Generic NVIDIA", pciAddress: "0000:05:00.0"},
		{Name: "Unknown address"},
	}
	got := mergeLinuxGPUInventory(vendor[2:], vendor[:2], generic)
	if len(got) != 5 || got[1].VramBytes != 16 || got[2].VramBytes != 8 || got[3].Name != "AMD" || got[4].Name != "Unknown address" {
		t.Fatalf("merged inventory = %+v; want each vendor GPU plus AMD and unknown", got)
	}
	if got := mergeLinuxGPUInventory(nil, nil, generic[:1]); len(got) != 1 || got[0].Name != "AMD" {
		t.Fatalf("generic-only inventory = %+v", got)
	}
}

func TestMissingNvidiaBusIDDoesNotDuplicateGenericGPU(t *testing.T) {
	nvidia := []GPUInfo{{Name: "NVIDIA", statsKey: "GPU-uuid"}}
	generic := []GPUInfo{{Name: "Generic NVIDIA", pciAddress: "0000:05:00.0", pciVendor: "10de"}, {Name: "AMD", pciAddress: "0000:06:00.0", pciVendor: "1002"}}
	got := mergeLinuxGPUInventory(nvidia, nil, generic)
	if len(got) != 2 || got[0].Name != "NVIDIA" || got[1].Name != "AMD" {
		t.Fatalf("unmatched inventory = %+v; want vendor NVIDIA once and AMD", got)
	}
}

func TestIntelVirtualFunctionIsNotRestoredByGenericInventory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHW_CHROOT", root)
	device := filepath.Join(root, "sys", "bus", "pci", "devices", "0000:03:00.1")
	if err := os.MkdirAll(device, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../0000:03:00.0", filepath.Join(device, "physfn")); err != nil {
		t.Fatal(err)
	}
	intel := []GPUInfo{{Name: "Arc", pciAddress: "0000:03:00.0"}}
	generic := []GPUInfo{
		{Name: "Arc VF", pciAddress: "0000:03:00.1", pciVendor: "8086"},
		{Name: "AMD", pciAddress: "0000:04:00.0", pciVendor: "1002"},
	}
	got := mergeLinuxGPUInventory(nil, intel, generic)
	if len(got) != 2 || got[0].Name != "Arc" || got[1].Name != "AMD" {
		t.Fatalf("inventory = %+v; want physical Intel and AMD, no Intel VF", got)
	}
}

func TestParseNvidiaStaticKeepsPCIBusIdentity(t *testing.T) {
	got, _ := parseNvidiaStatic("GPU-uuid, NVIDIA, 8192, 00000000:05:00.0\n")
	if len(got) != 1 || got[0].pciAddress != "0000:05:00.0" || got[0].statsKey != "GPU-uuid" {
		t.Fatalf("NVIDIA identity = %+v", got)
	}
}

func TestDetectNvidiaLinux_RetriesOnlyUnsupportedPCIField(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		gpus    int
	}{
		{"unsupported field", "Field pci.bus_id is not a valid field to query", 1},
		{"driver unavailable", "Failed to initialize NVML: Driver not loaded", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			script := "#!/bin/sh\ncase \"$1\" in\n*pci.bus_id) echo '" + tc.message +
				"' >&2; exit 1 ;;\n*) echo 'GPU-uuid, NVIDIA, 8192' ;;\nesac\n"
			if err := os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if got := detectNvidiaLinux(); len(got) != tc.gpus {
				t.Fatalf("detected %v, want %d GPU(s)", got, tc.gpus)
			}
		})
	}
}

func TestMixedInventoryKeepsAMDAndIntel(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(root, "bin")
	write(filepath.Join(bin, "nvidia-smi"), "#!/bin/sh\nexit 1\n", 0700)
	write(filepath.Join(bin, "xpu-smi"), "#!/bin/sh\nprintf '%s\\n' '{\"device_list\":[{\"device_id\":0,\"device_type\":\"GPU\",\"device_name\":\"Intel fixture\",\"pci_bdf_address\":\"0000:00:02.0\",\"memory_physical_size_byte\":1},{\"device_id\":1,\"device_type\":\"GPU\",\"device_function_type\":\"virtual\",\"device_name\":\"Intel VF\",\"pci_bdf_address\":\"0000:00:02.1\"}]}'\n", 0700)
	t.Setenv("PATH", bin)
	t.Setenv("GHW_CHROOT", root)
	t.Setenv("PCIDB_PATH", filepath.Join(root, "pci.ids"))
	write(filepath.Join(root, "pci.ids"), "1002  AMD\n\t744c  AMD fixture\n8086  Intel\n\t7d67  Intel fixture\n\nC 03  Display controller\n\t00  VGA compatible controller\n\t\t00  VGA controller\n", 0600)
	write(filepath.Join(root, "sys/bus/pci/devices/0000:03:00.0/modalias"), "pci:v00001002d0000744Csv00001002sd0000744Cbc03sc00i00\n", 0600)
	write(filepath.Join(root, "sys/bus/pci/devices/0000:00:02.0/modalias"), "pci:v00008086d00007D67sv00008086sd00007D67bc03sc00i00\n", 0600)
	write(filepath.Join(root, "sys/bus/pci/devices/0000:00:02.1/modalias"), "pci:v00008086d00007D67sv00008086sd00007D67bc03sc00i00\n", 0600)
	if err := os.Symlink("../0000:00:02.0", filepath.Join(root, "sys/bus/pci/devices/0000:00:02.1/physfn")); err != nil {
		t.Fatal(err)
	}
	drm := filepath.Join(root, "sys/class/drm")
	if err := os.MkdirAll(drm, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/0000:03:00.0/drm/card0", filepath.Join(drm, "card0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/0000:00:02.0/drm/card1", filepath.Join(drm, "card1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/0000:00:02.1/drm/card2", filepath.Join(drm, "card2")); err != nil {
		t.Fatal(err)
	}
	if cards := detectGPUsGHW(); len(cards) != 3 {
		t.Fatalf("invalid fixture: generic inventory = %+v; want AMD, Intel PF and Intel VF", cards)
	}
	got := detectGPUs()
	if len(got) != 2 {
		t.Fatalf("mixed inventory = %+v; want AMD and Intel exactly once", got)
	}
	names := map[string]bool{}
	for _, gpu := range got {
		names[gpu.Name] = true
	}
	if !names["AMD fixture"] || !names["Intel fixture"] {
		t.Fatalf("mixed inventory = %+v", got)
	}
}
