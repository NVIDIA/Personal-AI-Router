// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestCanonicalPCIAddress_NormalizesDiscoveryAddresses(t *testing.T) {
	for input, address := range map[string]string{
		"0000:03:00.1": "0000:03:00.1", "03:00.0": "0000:03:00.0",
		"00000000:AB:0D.0": "0000:ab:0d.0", "garbage": "", "3-00-0": "",
	} {
		assert.Equal(t, address, canonicalPCIAddress(input), input)
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

func TestIntelRenderNodeForBDF_UsesFixtureDevice(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHW_CHROOT", root)
	const address = "0000:03:00.0"
	drm := filepath.Join(root, "sys", "bus", "pci", "devices", address, "drm", "renderD128")
	node := filepath.Join(root, "dev", "dri", "renderD128")
	require.NoError(t, os.MkdirAll(drm, 0700))
	require.NoError(t, os.MkdirAll(filepath.Dir(node), 0700))
	require.NoError(t, os.WriteFile(node, nil, 0600))
	assert.Equal(t, node, intelRenderNodeForBDF(address))
}

func TestDetectGPUs_ExcludesIntelVirtualFunctionsWithoutXpuSmi(t *testing.T) {
	root := newIntelDRMFixture(t)
	physical := addIntelDRMCard(t, root, "card0", "0000:03:00.0")
	virtual := addIntelDRMCard(t, root, "card1", "0000:03:00.1")
	require.NoError(t, os.Symlink(physical, filepath.Join(virtual, "physfn")))

	inventory := detectGPUs()
	require.Len(t, inventory, 1)
	assert.Equal(t, "0000:03:00.0", inventory[0].pciAddress)
	sources := discoverIntelStatsSources(t.Context())
	require.Len(t, sources, 1)
	assert.Equal(t, "0000:03:00.0", sources[0].pciAddress)
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
	sampleIntelStats(t.Context(), discoverIntelStatsSources(t.Context()), out)
	response := buildResponse(inventory, nil, 0, statsSnapshot{GPU: out}, "", nil)
	require.Len(t, response.GPUs, 2)
	second := response.GPUs[1]
	assert.Equal(t, "0000:07:00.0", second.pciAddress)
	assert.Equal(t, uint64(16384), second.VramBytes)
	assert.Equal(t, uint64(2048), second.VramUsedBytes)
}

func TestDiscoverIntelStatsSources_UsesDriverCapacityWhenCLIHasNone(t *testing.T) {
	root := newIntelDRMFixture(t)
	addIntelDRMCard(t, root, "card0", "0000:03:00.0")
	const script = `#!/bin/sh
echo '{"device_list":[{"device_id":0,"device_name":"Arc fixture","pci_bdf_address":"03:00.0"}]}'
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(script), 0700))
	sources := discoverIntelStatsSources(t.Context())
	require.Len(t, sources, 1)
	assert.Equal(t, "Arc fixture", sources[0].Name)
	assert.EqualValues(t, 16384, sources[0].VramBytes)
}

func TestStatsCollector_RecoversIntelDeviceIDsAfterTransientDiscoveryFailure(t *testing.T) {
	root := newIntelDRMFixture(t)
	addIntelDRMCard(t, root, "card0", "0000:03:00.0")
	t.Setenv("FIXTURE", root)
	const script = `#!/bin/sh
if [ "$1" = discovery ]; then
  echo discovery >> "$FIXTURE/calls"
  [ -f "$FIXTURE/ready" ] || exit 1
  echo '{"device_list":[{"device_id":0,"pci_bdf_address":"03:00.0"}]}'
else
  echo 'DeviceId,GPU utilization (%)'
  echo '0,73'
fi
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(script), 0700))
	now := time.Now()
	c := statsCollector{intelSources: discoverIntelStatsSources(context.Background()), nextIntelDiscovery: now.Add(intelDiscoveryRetryInterval)}
	out := map[string]gpuStat{}
	assert.Zero(t, sampleIntelStats(context.Background(), c.intelSources, out))
	assert.Equal(t, uint64(2048), out["intel-pci-0000:03:00.0"].VRAMUsed)
	c.refreshIntelSources(context.Background(), now.Add(intelDiscoveryRetryInterval)) // still unavailable

	require.NoError(t, os.WriteFile(filepath.Join(root, "ready"), nil, 0600))
	c.refreshIntelSources(context.Background(), now.Add(intelDiscoveryRetryInterval+time.Second))
	assert.Zero(t, sampleIntelStats(context.Background(), c.intelSources, out))
	c.refreshIntelSources(context.Background(), now.Add(2*intelDiscoveryRetryInterval))
	assert.Equal(t, 1, sampleIntelStats(context.Background(), c.intelSources, out))
	assert.Equal(t, uint32(73), out["intel-pci-0000:03:00.0"].UtilizationPct)
	c.refreshIntelSources(context.Background(), now.Add(3*intelDiscoveryRetryInterval))
	calls, err := os.ReadFile(filepath.Join(root, "calls"))
	require.NoError(t, err)
	assert.Equal(t, "discovery\ndiscovery\ndiscovery\n", string(calls))
}

func TestCollectIntelXpuSmiDevices_MissingIDDoesNotAliasDeviceZero(t *testing.T) {
	root := newIntelDRMFixture(t)
	const script = `#!/bin/sh
echo '{"device_list":[{"pci_bdf_address":"07:00.0"},{"device_id":0,"pci_bdf_address":"03:00.0"}]}'
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(script), 0700))

	sources := discoverIntelStatsSources(context.Background())
	require.Len(t, sources, 2)
	samples := parseIntelXpuSmiCSV([]byte("DeviceId,GPU utilization (%)\n0,73\n"), sources)
	require.Len(t, samples, 1)
	assert.Equal(t, uint32(73), samples["intel-pci-0000:03:00.0"].util)
	for _, src := range sources {
		assert.Equal(t, src.pciAddress == "0000:03:00.0", src.hasDevID)
	}
}

// TestSampleIntelStatsHandlesEmptySources leaves output unchanged.
func TestSampleIntelStatsHandlesEmptySources(t *testing.T) {
	out := map[string]gpuStat{}
	if got := sampleIntelStats(context.Background(), nil, out); got != 0 {
		t.Errorf("sampleIntelStats with no sources returned %d samples, want 0", got)
	}
	if len(out) != 0 {
		t.Errorf("sampleIntelStats with no sources mutated out: %v", out)
	}
}
