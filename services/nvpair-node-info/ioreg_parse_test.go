// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

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
	if err != nil {
		t.Fatalf("parseIORegistryGPUs() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}

	apple := records[0]
	if apple.statsKey != "ioreg:2a" || apple.name != "Apple M3 Max" {
		t.Fatalf("unexpected Apple identity: %+v", apple)
	}
	if apple.vramTotal != systemMemory || apple.vramUsed != 8<<30 {
		t.Fatalf("unexpected Apple memory: %+v", apple)
	}
	if apple.utilizationPct != 100 || !apple.utilizationValid {
		t.Fatalf("Apple utilization = %d valid:%v, want 100/true",
			apple.utilizationPct, apple.utilizationValid)
	}

	discrete := records[1]
	if discrete.statsKey != "ioreg:63" || discrete.name != "AMD Radeon Pro" {
		t.Fatalf("unexpected discrete identity: %+v", discrete)
	}
	if discrete.vramTotal != 8<<30 || discrete.vramUsed != 2<<30 {
		t.Fatalf("unexpected discrete memory: %+v", discrete)
	}
	if discrete.utilizationPct != 25 || !discrete.utilizationValid {
		t.Fatalf("discrete utilization = %d valid:%v, want 25/true",
			discrete.utilizationPct, discrete.utilizationValid)
	}
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
	if err != nil {
		t.Fatalf("parseIORegistryGPUs() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	got := records[0]
	if got.name != "IntelAccelerator" || got.statsKey != "ioreg:7" {
		t.Fatalf("unexpected fallback identity: %+v", got)
	}
	if got.vramTotal != 0 || got.vramUsed != 0 || got.utilizationPct != 0 ||
		got.utilizationValid {
		t.Fatalf("missing metrics should remain zero: %+v", got)
	}
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
	if err != nil {
		t.Fatalf("parseIORegistryGPUs() error = %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if records[0].vramTotal != 0 || records[0].vramUsed != 1<<30 {
		t.Fatalf("free-only counters inferred false capacity: %+v", records[0])
	}
	if records[0].utilizationValid {
		t.Fatalf("missing utilization reported valid: %+v", records[0])
	}
	if records[1].vramTotal != 8<<30 || records[1].vramUsed != 0 ||
		records[1].utilizationPct != 0 || !records[1].utilizationValid {
		t.Fatalf("explicit zero used counter was not preserved: %+v", records[1])
	}
	if records[2].vramTotal != 8<<30 || records[2].vramUsed != 3<<30 ||
		records[2].utilizationPct != 67 || !records[2].utilizationValid {
		t.Fatalf("alias counters were not normalized: %+v", records[2])
	}
}

func TestParseIORegistryGPUsRejectsMalformedPlist(t *testing.T) {
	if _, err := parseIORegistryGPUs([]byte("<plist>"), 0); err == nil {
		t.Fatal("malformed plist returned nil error")
	}
}
