// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"os"
	"path/filepath"
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
	for _, value := range []string{"0", "83", "100", "101", "-1", "N/A"} {
		write("gpu_busy_percent", value)
		out := map[string]gpuStat{"NVIDIA-existing": {UtilizationPct: 7}}
		valid := decodeAMDUtilization(root, out)
		want := value == "0" || value == "83" || value == "100"
		if valid != want {
			t.Fatalf("%q valid=%v", value, valid)
		}
		_, exists := out["amd:0000:c5:00.0"]
		if exists != want {
			t.Fatalf("%q fabricated or lost sample", value)
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
