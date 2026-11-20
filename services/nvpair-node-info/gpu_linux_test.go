// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAMDUtilizationAvailability(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "0000:c5:00.0")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("vendor", "0x1002\n")
	if got := amdStatsKey(root, "c5:00.0"); got != "amd:0000:c5:00.0" {
		t.Fatal(got)
	}
	readings := map[string]uint32{"0": 0, "83": 83, "100": 100}
	for _, value := range []string{"0", "83", "100", "101", "-1", "N/A"} {
		write("gpu_busy_percent", value)
		out := map[string]gpuStat{"NVIDIA-existing": {UtilizationPct: 7}}
		valid := decodeAMDUtilization(root, out)
		wantPercent, want := readings[value]
		if valid != want {
			t.Fatalf("%q valid=%v", value, valid)
		}
		sample, exists := out["amd:0000:c5:00.0"]
		if exists != want {
			t.Fatalf("%q fabricated or lost sample", value)
		}
		if exists && sample.UtilizationPct != wantPercent {
			t.Fatalf("%q produced %d%% utilization, want %d%%", value, sample.UtilizationPct, wantPercent)
		}
		if out["NVIDIA-existing"].UtilizationPct != 7 {
			t.Fatal("NVIDIA overwritten")
		}
	}
	if err := os.Remove(filepath.Join(dir, "gpu_busy_percent")); err != nil {
		t.Fatal(err)
	}
	if decodeAMDUtilization(root, map[string]gpuStat{}) {
		t.Fatal("missing counter marked valid")
	}
	write("vendor", "0x8086")
	write("gpu_busy_percent", "70")
	if decodeAMDUtilization(root, map[string]gpuStat{}) {
		t.Fatal("non-AMD adapter accepted")
	}
}

func TestDecodeGPU_ReportsNvidiaQueryFailures(t *testing.T) {
	test := func(name string, tool string, wantLevel string) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PATH", root)
			if tool != "" {
				if err := os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte(tool), 0700); err != nil {
					t.Fatal(err)
				}
			}

			var logs bytes.Buffer
			originalLogger := slog.Default()
			originalOutput, originalFlags := log.Writer(), log.Flags()
			t.Cleanup(func() {
				slog.SetDefault(originalLogger)
				log.SetOutput(originalOutput)
				log.SetFlags(originalFlags)
			})
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

			collector := statsCollector{}
			if collector.decodeGPU(make(map[string]gpuStat)) {
				t.Fatal("failed query reported a valid sample")
			}
			if !strings.Contains(logs.String(), "level="+wantLevel) {
				t.Fatalf("expected %s for query failure, got %q", wantLevel, logs.String())
			}
		})
	}
	test("optional CLI absent", "", "DEBUG")
	test("installed CLI query fails", "#!/bin/sh\nexit 1\n", "WARN")
}
