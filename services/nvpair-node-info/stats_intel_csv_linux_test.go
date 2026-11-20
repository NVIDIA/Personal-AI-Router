// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIntelCSVDocumentedContracts(t *testing.T) {
	sources := []intelStatsSource{
		{statsKey: "intel-a", deviceID: 0, hasDevID: true},
		{statsKey: "intel-b", deviceID: 1, hasDevID: true},
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
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestIntelDumpUsesVersionCompatibleFlags(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		calls        int
	}{
		{"v2", `printf '%s\n' "$*" >> "$CALL_LOG"
printf 'Timestamp,DeviceId,utilization.gpu (%%),memory.used (MiB)\n12:00,0,42,1\n'`, 1},
		{"v1", `printf '%s\n' "$*" >> "$CALL_LOG"
case " $* " in *" --number "*) printf '%s\n' 'The following argument was not expected: --number' >&2; exit 2;; esac
printf 'Timestamp,DeviceId,Average %% utilization of all GPU Engines,GPU Memory Used (MiB)\n12:00,0,42,1\n'`, 2},
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
			sources := []intelStatsSource{{statsKey: "intel-a", deviceID: 0, hasDevID: true}}
			got := sampleIntelXpuSmi(sources)
			if got["intel-a"] != (intelDynamicStat{util: 42, mem: 1048576, hasUtil: true, hasMem: true}) {
				t.Fatalf("unexpected stats %+v", got)
			}
			if tc.name == "v1" {
				_ = sampleIntelXpuSmi(sources)
				tc.calls++ // the remembered legacy form takes one call next time
			}
			raw, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) != tc.calls || !strings.Contains(lines[len(lines)-1], "--metrics 0,18") {
				t.Fatalf("calls %q, want %d correct invocations", lines, tc.calls)
			}
			if tc.name == "v1" && !strings.Contains(lines[len(lines)-1], " -n 1") {
				t.Fatalf("legacy flag not remembered: %q", lines)
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
	if got := sampleIntelXpuSmi([]intelStatsSource{{statsKey: "intel-a", deviceID: 0, hasDevID: true}}); len(got) != 0 {
		t.Fatalf("temporary failure returned data: %+v", got)
	}
	raw, err := os.ReadFile(calls)
	if err != nil || string(raw) != "called\n" {
		t.Fatalf("calls %q, error %v; want one", raw, err)
	}
	got := sampleIntelXpuSmi([]intelStatsSource{{statsKey: "intel-a", deviceID: 0, hasDevID: true}})
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
	sources := []intelStatsSource{{statsKey: "intel-a", deviceID: 0, hasDevID: true}}
	_ = sampleIntelXpuSmi(sources)
	_ = sampleIntelXpuSmi(sources)
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
	_ = sampleIntelXpuSmi([]intelStatsSource{{statsKey: "intel-a", deviceID: 0, hasDevID: true}})
	if !intelXpuSmiUnavailable.Load() {
		t.Fatal("missing xpu-smi was not latched")
	}
}
