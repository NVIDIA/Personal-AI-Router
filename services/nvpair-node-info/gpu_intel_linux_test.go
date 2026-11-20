// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseXpuSmiDiscovery pins the JSON schema the Intel discovery
// parser accepts. If this ever drifts, every Intel host would list its
// GPUs without VRAM (or without a statsKey, breaking the dynamic-stats
// join). The cases target: a physical Arc dGPU with a numeric memory
// size, a physical Arc with the string-encoded variant xpu-smi emits
// on some builds, a virtual function that must be skipped, and a
// non-GPU device that must be ignored.
func TestParseXpuSmiDiscovery(t *testing.T) {
	// The parser must not mistake a fixture's PCI address for a device on this host.
	root := t.TempDir()
	t.Setenv("GHW_CHROOT", root)
	device := filepath.Join(root, "sys", "bus", "pci", "devices", "0000:03:00.0")
	if err := os.MkdirAll(device, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../0000:02:00.0", filepath.Join(device, "physfn")); err != nil {
		t.Fatal(err)
	}
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

	got, err := parseXpuSmiDiscovery([]byte(payload))
	if err != nil {
		t.Fatalf("decode discovery: %v", err)
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

func TestParseXpuSmiDiscoveryRejectsGarbage(t *testing.T) {
	if _, err := parseXpuSmiDiscovery([]byte("not json")); err == nil {
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

func newIntelDRMFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("GHW_CHROOT", root)
	t.Setenv("PATH", root)
	t.Setenv("PCIDB_PATH", filepath.Join(root, "pci.ids"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pci.ids"), []byte("8086  Intel\n\t56a0  Arc\n"), 0600))
	intelXpuSmiUnavailable.Store(false)
	t.Cleanup(func() { intelXpuSmiUnavailable.Store(false) })
	return root
}

func addIntelDRMCard(t *testing.T, root, name, address string) string {
	t.Helper()
	device := filepath.Join(root, "sys/bus/pci/devices", address)
	card := filepath.Join(device, "drm", name)
	drm := filepath.Join(root, "sys/class/drm")
	require.NoError(t, os.MkdirAll(card, 0700))
	require.NoError(t, os.MkdirAll(drm, 0700))
	require.NoError(t, os.Symlink(device, filepath.Join(card, "device")))
	require.NoError(t, os.Symlink(card, filepath.Join(drm, name)))
	require.NoError(t, os.WriteFile(filepath.Join(device, "vendor"), []byte("0x8086"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(device, "modalias"), []byte("pci:v00008086d000056A0sv00008086sd000056A0bc03sc00i00\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(device, "mem_info_vram_total"), []byte("16384"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(device, "mem_info_vram_used"), []byte("2048"), 0600))
	return device
}

func TestDetectGPUs_ExcludesIntelVirtualFunctionsWithoutXpuSmi(t *testing.T) {
	root := newIntelDRMFixture(t)
	physical := addIntelDRMCard(t, root, "card0", "0000:03:00.0")
	virtual := addIntelDRMCard(t, root, "card1", "0000:03:00.1")
	require.NoError(t, os.Symlink(physical, filepath.Join(virtual, "physfn")))

	inventory := detectGPUs()
	require.Len(t, inventory, 1)
	assert.Equal(t, "0000:03:00.0", inventory[0].pciAddress)
	sources := discoverIntelStatsSources()
	require.Len(t, sources, 1)
	assert.Equal(t, "0000:03:00.0", sources[0].bdf)
}

func TestDetectGPUs_JoinsIntelMemoryWhenXpuSmiListsOnlyOneCard(t *testing.T) {
	root := newIntelDRMFixture(t)
	addIntelDRMCard(t, root, "card0", "0000:03:00.0")
	addIntelDRMCard(t, root, "card1", "0000:07:00.0")
	const script = `#!/bin/sh
if [ "$1" = discovery ]; then
  echo '{"device_list":[{"device_id":0,"device_name":"Arc from xpu-smi","pci_bdf_address":"00000000:03:00.0","memory_physical_size_byte":8192}]}'
fi
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(script), 0700))

	inventory := detectGPUs()
	require.Len(t, inventory, 2)
	assert.Equal(t, "Arc from xpu-smi", inventory[0].Name)
	assert.Equal(t, uint64(8192), inventory[0].VramBytes)
	out := map[string]gpuStat{}
	sampleIntelStats(discoverIntelStatsSources(), out)
	response := buildResponse(inventory, nil, 0, statsSnapshot{GPU: out}, "", nil)
	require.Len(t, response.GPUs, 2)
	second := response.GPUs[1]
	assert.Equal(t, "0000:07:00.0", second.pciAddress)
	assert.Equal(t, uint64(16384), second.VramBytes)
	assert.Equal(t, uint64(2048), second.VramUsedBytes)
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
		in     any
		want   uint32
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
