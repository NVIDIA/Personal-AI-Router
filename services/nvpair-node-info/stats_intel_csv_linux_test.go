// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIntelCSVDocumentedContracts(t *testing.T) {
	sources := []intelStatsSource{
		{GPUInfo: GPUInfo{statsKey: "intel-a"}, deviceID: 0, hasDevID: true},
		{GPUInfo: GPUInfo{statsKey: "intel-b"}, deviceID: 1, hasDevID: true},
	}
	cases := []struct {
		name, input string
		want        map[string]intelDynamicStat
	}{
		{"v1 multiple devices", "Timestamp, DeviceId, Average % utilization of all GPU Engines, GPU Memory Used (MiB)\n12:00:00.000, 0, 42.5, 1024\n12:00:00.000, 1, N/A, 0.5\n", map[string]intelDynamicStat{
			"intel-a": {util: 43, mem: 1073741824, hasUtil: true, hasMem: true},
			"intel-b": {mem: 524288, hasMem: true},
		}},
		{"v2 header order", "Timestamp,DeviceId,memory.used (MiB),utilization.gpu (%)\n12:00,1,16,0\n", map[string]intelDynamicStat{
			"intel-b": {util: 0, mem: 16777216, hasUtil: true, hasMem: true},
		}},
		{"memory percentage is not used bytes", "Timestamp,DeviceId,GPU Memory Utilization (%)\n12:00,0,50\n", nil},
		{"unknown and invalid", "Timestamp,DeviceId,utilization.gpu (%),memory.used (MiB)\n12:00,0,NaN,Inf\n12:00,1,101,17592186044416\n12:00,7,5,20\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIntelXpuSmiCSV([]byte(tc.input), sources)
			if !maps.Equal(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIntelDumpUsesVersionCompatibleFlags(t *testing.T) {
	const modern = "dump --device -1 --metrics 0,18 --number 1"
	const legacy = "dump --device -1 --metrics 0,18 -n 1"
	for _, tc := range []struct {
		name, script string
		want         []string
	}{
		{"v2", `printf '%s\n' "$*" >> "$CALL_LOG"
printf 'Timestamp,DeviceId,utilization.gpu (%%),memory.used (MiB)\n12:00,0,42,1\n'`, []string{modern}},
		{"v1", `printf '%s\n' "$*" >> "$CALL_LOG"
case " $* " in *" --number "*) printf '%s\n' 'The following argument was not expected: --number' >&2; exit 2;; esac
printf 'Timestamp,DeviceId,Average %% utilization of all GPU Engines,GPU Memory Used (MiB)\n12:00,0,42,1\n'`, []string{modern, legacy, legacy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			t.Setenv("PATH", dir)
			t.Setenv("CALL_LOG", calls)
			if err := os.WriteFile(filepath.Join(dir, "xpu-smi"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			intelXpuSmiUnavailable.Store(false)
			intelXpuSmiLegacyCount.Store(false)
			t.Cleanup(func() { intelXpuSmiUnavailable.Store(false); intelXpuSmiLegacyCount.Store(false) })
			sources := []intelStatsSource{{GPUInfo: GPUInfo{statsKey: "intel-a"}, deviceID: 0, hasDevID: true}}
			got := sampleIntelXpuSmi(context.Background(), sources)
			if got["intel-a"] != (intelDynamicStat{util: 42, mem: 1048576, hasUtil: true, hasMem: true}) {
				t.Fatalf("unexpected stats %+v", got)
			}
			if tc.name == "v1" {
				_ = sampleIntelXpuSmi(context.Background(), sources)
			}
			raw, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if !slices.Equal(lines, tc.want) {
				t.Fatalf("calls %q, want %q", lines, tc.want)
			}
		})
	}
}

func TestIntelTransientFailureIsNotRetriedAsAnotherVersion(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("PATH", dir)
	t.Setenv("CALL_LOG", calls)
	script := `#!/bin/sh
printf '%s\n' called >> "$CALL_LOG"
if [ ! -f "$CALL_LOG.first" ]; then
  printf '%s\n' first > "$CALL_LOG.first"
	  printf '%s\n' 'temporary device error' >&2
  exit 1
fi
printf 'Timestamp,DeviceId,utilization.gpu (%%),memory.used (MiB)\n12:00,0,42,1\n'
`
	if err := os.WriteFile(filepath.Join(dir, "xpu-smi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	intelXpuSmiUnavailable.Store(false)
	intelXpuSmiLegacyCount.Store(false)
	t.Cleanup(func() { intelXpuSmiUnavailable.Store(false); intelXpuSmiLegacyCount.Store(false) })
	if got := sampleIntelXpuSmi(context.Background(), []intelStatsSource{{GPUInfo: GPUInfo{statsKey: "intel-a"}, deviceID: 0, hasDevID: true}}); len(got) != 0 {
		t.Fatalf("temporary failure returned data: %+v", got)
	}
	raw, err := os.ReadFile(calls)
	if err != nil || string(raw) != "called\n" {
		t.Fatalf("calls %q, error %v; want one", raw, err)
	}
	got := sampleIntelXpuSmi(context.Background(), []intelStatsSource{{GPUInfo: GPUInfo{statsKey: "intel-a"}, deviceID: 0, hasDevID: true}})
	if got["intel-a"] != (intelDynamicStat{util: 42, mem: 1048576, hasUtil: true, hasMem: true}) {
		t.Fatalf("later sample did not recover: %+v", got)
	}
}

func TestIntelAccessRefusalIsLatchedWithoutRetry(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	t.Setenv("PATH", dir)
	t.Setenv("CALL_LOG", calls)
	script := "#!/bin/sh\nprintf '%s\\n' called >> \"$CALL_LOG\"\nprintf '%s\\n' 'permission denied; The following argument was not expected: --number' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "xpu-smi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	intelXpuSmiUnavailable.Store(false)
	intelXpuSmiLegacyCount.Store(false)
	t.Cleanup(func() { intelXpuSmiUnavailable.Store(false); intelXpuSmiLegacyCount.Store(false) })
	sources := []intelStatsSource{{GPUInfo: GPUInfo{statsKey: "intel-a"}, deviceID: 0, hasDevID: true}}
	_ = sampleIntelXpuSmi(context.Background(), sources)
	_ = sampleIntelXpuSmi(context.Background(), sources)
	raw, err := os.ReadFile(calls)
	if err != nil || string(raw) != "called\n" {
		t.Fatalf("calls %q, error %v; want one after access refusal", raw, err)
	}
}

func TestMissingIntelUtilityIsLatched(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	intelXpuSmiUnavailable.Store(false)
	intelXpuSmiLegacyCount.Store(false)
	t.Cleanup(func() { intelXpuSmiUnavailable.Store(false); intelXpuSmiLegacyCount.Store(false) })
	_ = sampleIntelXpuSmi(context.Background(), []intelStatsSource{{GPUInfo: GPUInfo{statsKey: "intel-a"}, deviceID: 0, hasDevID: true}})
	if !intelXpuSmiUnavailable.Load() {
		t.Fatal("missing xpu-smi was not latched")
	}
}

// A measured zero suppresses fallback. Missing memory still uses the driver.
func TestIntelMemoryFallbackPreservesReportedZero(t *testing.T) {
	for _, test := range []struct {
		reading string
		want    uint64
	}{
		{reading: "0", want: 0},
		{reading: "N/A", want: 2048},
	} {
		t.Run(test.reading, func(t *testing.T) {
			root := newIntelDRMFixture(t)
			addIntelDRMCard(t, root, "card0", "0000:03:00.0")
			t.Setenv("FIXTURE_READING", test.reading)
			script := "#!/bin/sh\nprintf 'DeviceId,GPU utilization (%%),GPU Memory Used (MiB)\\n0,42,%s\\n' \"$FIXTURE_READING\"\n"
			if err := os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			sources := collectIntelSysfsCards()
			if len(sources) != 1 {
				t.Fatalf("unexpected sources: %+v", sources)
			}
			sources[0].deviceID, sources[0].hasDevID = 0, true
			stats := make(map[string]gpuStat)
			if got := sampleIntelStats(context.Background(), sources, stats); got != 1 {
				t.Fatalf("got %d utilization samples, want one", got)
			}
			got := stats[sources[0].statsKey]
			if got.UtilizationPct != 42 || got.VRAMUsed != test.want {
				t.Fatalf("got %+v, want utilization 42 and VRAM used %d", got, test.want)
			}
		})
	}
}
