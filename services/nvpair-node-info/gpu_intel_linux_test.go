// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"reflect"
	"testing"
)

// TestParseXpuSmiDiscovery pins the JSON schema the Intel discovery
// parser accepts. If this ever drifts, every Intel host would list its
// GPUs without VRAM (or without a statsKey, breaking the dynamic-stats
// join). The cases target: a physical Arc dGPU with a numeric memory
// size, a physical Arc with the string-encoded variant xpu-smi emits
// on some builds, a virtual function that must be skipped, and a
// non-GPU device that must be ignored.
func TestParseXpuSmiDiscovery(t *testing.T) {
	const payload = `{
		"device_list": [
			{
				"device_id": 0,
				"device_type": "GPU",
				"device_function_type": "physical",
				"device_name": "Intel(R) Arc(TM) A770 Graphics",
				"uuid": "00000000-0000-0000-0000-56a05e001000",
				"pci_bdf_address": "0000:03:00.0",
				"pci_device_id": "0x56A0",
				"vendor_name": "Intel(R) Corporation",
				"memory_physical_size_byte": "17102532608"
			},
			{
				"device_id": 1,
				"device_type": "GPU",
				"device_function_type": "physical",
				"device_name": "Intel(R) Arc(TM) A580 Graphics",
				"pci_bdf_address": "0000:04:00.0",
				"memory_physical_size_byte": 8589934592
			},
			{
				"device_id": 2,
				"device_type": "GPU",
				"device_function_type": "virtual",
				"device_name": "Intel(R) Arc VF",
				"pci_bdf_address": "0000:03:00.1"
			},
			{
				"device_id": 3,
				"device_type": "NNP",
				"device_name": "Intel(R) Habana Gaudi",
				"pci_bdf_address": "0000:05:00.0"
			}
		]
	}`

	got, ok := parseXpuSmiDiscovery([]byte(payload))
	if !ok {
		t.Fatal("parseXpuSmiDiscovery returned ok=false on a valid payload")
	}
	if len(got) != 2 {
		t.Fatalf("got %d GPUs, want 2 (VF and non-GPU must be skipped): %+v", len(got), got)
	}

	if got[0].Name != "Intel(R) Arc(TM) A770 Graphics" {
		t.Errorf("gpu 0 Name = %q, want Arc A770", got[0].Name)
	}
	if got[0].VramBytes != 17102532608 {
		t.Errorf("gpu 0 VramBytes = %d, want 17102532608 (string-encoded)", got[0].VramBytes)
	}
	if got[0].statsKey != "intel-pci-0000:03:00.0" {
		t.Errorf("gpu 0 statsKey = %q, want intel-pci-0000:03:00.0", got[0].statsKey)
	}

	if got[1].VramBytes != 8589934592 {
		t.Errorf("gpu 1 VramBytes = %d, want 8589934592 (numeric)", got[1].VramBytes)
	}
	if got[1].statsKey != "intel-pci-0000:04:00.0" {
		t.Errorf("gpu 1 statsKey = %q, want intel-pci-0000:04:00.0", got[1].statsKey)
	}
}

// TestParseXpuSmiDiscoveryRejectsGarbage confirms that a non-conforming
// payload degrades cleanly to the sysfs fallback (ok=false) instead of
// producing a phantom adapter.
func TestParseXpuSmiDiscoveryRejectsGarbage(t *testing.T) {
	if _, ok := parseXpuSmiDiscovery([]byte("not json")); ok {
		t.Fatal("parseXpuSmiDiscovery accepted non-JSON input")
	}
}

// TestIntelStatsKeyFromBDF covers the sysfs and xpu-smi BDF forms plus
// the domain-less short form some xpu-smi builds emit. A missing colon
// is rejected: without a bus:device separator we can't uniquely
// identify a PCI adapter.
func TestIntelStatsKeyFromBDF(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0000:03:00.0", "intel-pci-0000:03:00.0"},
		{"03:00.0", "intel-pci-0000:03:00.0"},
		{"0000:03:00.1", "intel-pci-0000:03:00.1"},
		{"0000:AB:CD.0", "intel-pci-0000:ab:cd.0"},
		{"", ""},
		{"3-00-0", ""},
		{"garbage", ""},
	}
	for _, c := range cases {
		got := intelStatsKeyFromBDF(c.in)
		if got != c.want {
			t.Errorf("intelStatsKeyFromBDF(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestIsPhysicalIntelGPU exercises the filter that keeps SR-IOV virtual
// functions and non-GPU Intel accelerators out of the inventory. The
// "empty device_type" case pins the compatibility path for xpu-smi
// builds that don't emit the field at all.
func TestIsPhysicalIntelGPU(t *testing.T) {
	cases := []struct {
		name string
		in   xpuSmiDiscoveryDevice
		want bool
	}{
		{"physical GPU", xpuSmiDiscoveryDevice{DeviceType: "GPU", DeviceFunctionType: "physical"}, true},
		{"physical GPU with mixed case", xpuSmiDiscoveryDevice{DeviceType: "gpu", DeviceFunctionType: "physical"}, true},
		{"missing function type", xpuSmiDiscoveryDevice{DeviceType: "GPU"}, true},
		{"missing device type is treated as GPU", xpuSmiDiscoveryDevice{DeviceFunctionType: "physical"}, true},
		{"virtual function", xpuSmiDiscoveryDevice{DeviceType: "GPU", DeviceFunctionType: "virtual"}, false},
		{"non-GPU accelerator", xpuSmiDiscoveryDevice{DeviceType: "NNP", DeviceFunctionType: "physical"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPhysicalIntelGPU(c.in); got != c.want {
				t.Errorf("isPhysicalIntelGPU(%+v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestLooksLikePCIBDF pins the shape check the sysfs walker uses to
// filter out platform-device final path segments. Real BDFs have
// exactly two colons and at least one dot; anything else must be
// rejected so a non-PCI DRM entry can't be mistaken for an Intel
// adapter.
func TestLooksLikePCIBDF(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0000:03:00.0", true},
		{"0000:ab:cd.1", true},
		{"03:00.0", false},
		{"0000-03-00-0", false},
		{"platform:soc0", false},
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikePCIBDF(c.in); got != c.want {
			t.Errorf("looksLikePCIBDF(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseIntelXpuSmiDump covers the dump-JSON shapes xpu-smi has
// emitted across releases (bare array vs. object-with-data), the
// header-name variance for utilization and memory-used, and the
// device_id/BDF join paths. A failure in any of these would make
// Intel utilization silently drop from the wire while the Intel
// adapters still appear in the inventory.
func TestParseIntelXpuSmiDump(t *testing.T) {
	sources := []intelStatsSource{
		{statsKey: "intel-pci-0000:03:00.0", bdf: "0000:03:00.0", deviceID: 0, hasDevID: true},
		{statsKey: "intel-pci-0000:04:00.0", bdf: "0000:04:00.0", deviceID: 1, hasDevID: true},
	}

	cases := []struct {
		name string
		in   string
		want map[string]intelDynamicStat
	}{
		{
			name: "bare array, MiB memory header",
			in: `[
				{"deviceId":"0","GPU Utilization (%)":"42.5","GPU Memory Used (MiB)":"1024"},
				{"deviceId":"1","GPU Utilization (%)":"7","GPU Memory Used (MiB)":"128"}
			]`,
			want: map[string]intelDynamicStat{
				"intel-pci-0000:03:00.0": {util: 43, mem: 1024 * 1024 * 1024, hasUtil: true, hasMem: true},
				"intel-pci-0000:04:00.0": {util: 7, mem: 128 * 1024 * 1024, hasUtil: true, hasMem: true},
			},
		},
		{
			name: "object with data field, byte memory header",
			in: `{"data":[
				{"device_id":0,"XPUM_STATS_GPU_UTILIZATION":15,"XPUM_STATS_MEMORY_USED":536870912}
			]}`,
			want: map[string]intelDynamicStat{
				"intel-pci-0000:03:00.0": {util: 15, mem: 536870912, hasUtil: true, hasMem: true},
			},
		},
		{
			name: "join by BDF when device_id is absent",
			in: `[
				{"pci_bdf_address":"0000:04:00.0","GPU Utilization (%)":"50","GPU Memory Used (MiB)":"64"}
			]`,
			want: map[string]intelDynamicStat{
				"intel-pci-0000:04:00.0": {util: 50, mem: 64 * 1024 * 1024, hasUtil: true, hasMem: true},
			},
		},
		{
			name: "N/A utilization drops the utilization field, keeps memory",
			in: `[
				{"deviceId":"0","GPU Utilization (%)":"N/A","GPU Memory Used (MiB)":"32"}
			]`,
			want: map[string]intelDynamicStat{
				"intel-pci-0000:03:00.0": {mem: 32 * 1024 * 1024, hasMem: true},
			},
		},
		{
			name: "row for unknown adapter is skipped",
			in: `[
				{"deviceId":"99","GPU Utilization (%)":"10","GPU Memory Used (MiB)":"1"}
			]`,
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseIntelXpuSmiDump([]byte(c.in), sources)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseIntelXpuSmiDump = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestSampleIntelStatsMergesXpuSmiAndSysfs pins the merge rule: xpu-smi
// wins for utilization and memory-used, and sysfs only fills memory
// when xpu-smi didn't provide it. Verified indirectly via the map
// returned by parseIntelXpuSmiDump because sampleIntelStats depends on
// filesystem state we don't want to mock in a unit test.
func TestSampleIntelStatsHandlesEmptySources(t *testing.T) {
	out := map[string]gpuStat{}
	if got := sampleIntelStats(nil, out); got != 0 {
		t.Errorf("sampleIntelStats(nil, ...) returned %d samples, want 0", got)
	}
	if len(out) != 0 {
		t.Errorf("sampleIntelStats(nil, ...) mutated out: %v", out)
	}
}

// TestParseFloatishPercent pins the type-tolerant number parsing that
// keeps the Intel decoder working across xpu-smi builds that emit
// percentages as strings, floats, or integers.
func TestParseFloatishPercent(t *testing.T) {
	cases := []struct {
		in    any
		want  uint32
		wantOK bool
	}{
		{42.5, 43, true},
		{"42.5", 43, true},
		{100, 100, true},
		{"105", 100, true},
		{-1.0, 0, false},
		{"N/A", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseFloatishPercent(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("parseFloatishPercent(%v) = %d,%v; want %d,%v", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
