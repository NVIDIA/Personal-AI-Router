// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const ioRegistryGPUFixture = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><array>
<dict>
  <key>IORegistryEntryID</key><integer>42</integer>
  <key>IORegistryEntryName</key><string>AGXAccelerator</string>
  <key>IOObjectClass</key><string>AGXAcceleratorG15X</string>
  <key>model</key><string>Apple M3 Max</string>
  <key>PerformanceStatistics</key><dict>
    <key>Device Utilization %</key><integer>137</integer>
    <key>Alloc system memory</key><integer>8589934592</integer>
    <key>In use system memory</key><integer>4294967296</integer>
  </dict>
</dict>
<dict>
  <key>IORegistryEntryID</key><integer>99</integer>
  <key>IORegistryEntryName</key><string>AMD Radeon Pro</string>
  <key>IOObjectClass</key><string>AMDRadeonX6000</string>
  <key>VRAM,totalMB</key><integer>8192</integer>
  <key>PerformanceStatistics</key><dict>
    <key>Device Utilization %</key><integer>25</integer>
    <key>vramUsedBytes</key><integer>2147483648</integer>
    <key>vramFreeBytes</key><integer>6442450944</integer>
  </dict>
</dict>
</array></plist>`

func TestParseIORegistryGPUs(t *testing.T) {
	const systemMemory = uint64(36 << 30)
	records, err := parseIORegistryGPUs([]byte(ioRegistryGPUFixture), systemMemory)
	require.NoError(t, err, "parseIORegistryGPUs() error")
	require.Len(t, records, 2)

	apple := records[0]
	require.Equal(t, "ioreg:2a", apple.statsKey, "unexpected Apple identity (%v)", apple)
	require.Equal(t, "Apple M3 Max", apple.name, "unexpected Apple identity (%v)", apple)
	require.Equal(t, systemMemory, apple.vramTotal, "unexpected Apple memory (%v)", apple)
	require.Equal(t, uint64(8<<30), apple.vramUsed, "unexpected Apple memory (%v)", apple)
	require.Equal(t, uint32(100), apple.utilizationPct, "Apple utilization")
	require.True(t, apple.utilizationValid, "Apple utilization")

	discrete := records[1]
	require.Equal(t, "ioreg:63", discrete.statsKey, "unexpected discrete identity (%v)", discrete)
	require.Equal(t, "AMD Radeon Pro", discrete.name, "unexpected discrete identity (%v)", discrete)
	require.Equal(t, uint64(8<<30), discrete.vramTotal, "unexpected discrete memory (%v)", discrete)
	require.Equal(t, uint64(2<<30), discrete.vramUsed, "unexpected discrete memory (%v)", discrete)
	require.Equal(t, uint32(25), discrete.utilizationPct, "discrete utilization")
	require.True(t, discrete.utilizationValid, "discrete utilization")
}

func TestParseIORegistryGPUsRecursesAndOmitsMissingMetrics(t *testing.T) {
	const fixture = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><array><dict>
<key>IORegistryEntryChildren</key><array><dict>
  <key>IORegistryEntryID</key><integer>7</integer>
  <key>IOObjectClass</key><string>IntelAccelerator</string>
</dict></array>
</dict></array></plist>`
	records, err := parseIORegistryGPUs([]byte(fixture), 16<<30)
	require.NoError(t, err, "parseIORegistryGPUs() error")
	require.Len(t, records, 1)
	got := records[0]
	require.Equal(t, "IntelAccelerator", got.name, "unexpected fallback identity (%v)", got)
	require.Equal(t, "ioreg:7", got.statsKey, "unexpected fallback identity (%v)", got)
	require.Equal(t, uint64(0), got.vramTotal, "missing metrics should remain zero (%v)", got)
	require.Equal(t, uint64(0), got.vramUsed, "missing metrics should remain zero (%v)", got)
	require.Equal(t, uint32(0), got.utilizationPct, "missing metrics should remain zero (%v)", got)
	require.False(t, got.utilizationValid, "missing metrics should remain zero (%v)", got)
}

func TestParseIORegistryGPUsPreservesDedicatedCounterPresence(t *testing.T) {
	const fixture = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><array>
<dict>
  <key>IORegistryEntryID</key><integer>8</integer>
  <key>IORegistryEntryName</key><string>Free Only</string>
  <key>PerformanceStatistics</key><dict>
    <key>vramFreeBytes</key><integer>6442450944</integer>
    <key>Alloc system memory</key><integer>1073741824</integer>
  </dict>
</dict>
<dict>
  <key>IORegistryEntryID</key><integer>9</integer>
  <key>IORegistryEntryName</key><string>Idle Dedicated</string>
  <key>PerformanceStatistics</key><dict>
    <key>Device Utilization %</key><integer>0</integer>
    <key>vramUsedBytes</key><integer>0</integer>
    <key>vramFreeBytes</key><integer>8589934592</integer>
    <key>Alloc system memory</key><integer>2147483648</integer>
  </dict>
</dict>
<dict>
  <key>IORegistryEntryID</key><integer>10</integer>
  <key>IORegistryEntryName</key><string>AMD Alias Counters</string>
  <key>PerformanceStatistics</key><dict>
    <key>GPU Activity(%)</key><integer>67</integer>
    <key>inUseVidMemoryBytes</key><integer>3221225472</integer>
    <key>vramFreeBytes</key><integer>5368709120</integer>
  </dict>
</dict>
</array></plist>`
	records, err := parseIORegistryGPUs([]byte(fixture), 0)
	require.NoError(t, err, "parseIORegistryGPUs() error")
	require.Len(t, records, 3)
	require.Equal(t, uint64(0), records[0].vramTotal, "free-only counters inferred false capacity")
	require.Equal(t, uint64(1<<30), records[0].vramUsed, "free-only counters inferred false capacity")
	require.False(t, records[0].utilizationValid, "missing utilization reported valid")
	require.Equal(t, uint64(8<<30), records[1].vramTotal, "explicit zero used counter was not preserved")
	require.Equal(t, uint64(0), records[1].vramUsed, "explicit zero used counter was not preserved")
	require.Equal(t, uint32(0), records[1].utilizationPct, "explicit zero used counter was not preserved")
	require.True(t, records[1].utilizationValid, "explicit zero used counter was not preserved")
	require.Equal(t, uint64(8<<30), records[2].vramTotal, "alias counters were not normalized")
	require.Equal(t, uint64(3<<30), records[2].vramUsed, "alias counters were not normalized")
	require.Equal(t, uint32(67), records[2].utilizationPct, "alias counters were not normalized")
	require.True(t, records[2].utilizationValid, "alias counters were not normalized")
}

func TestParseIORegistryGPUsRejectsMalformedPlist(t *testing.T) {
	{
		_, err := parseIORegistryGPUs([]byte("<plist>"), 0)
		require.Error(t, err, "malformed plist returned nil error")
	}
}
