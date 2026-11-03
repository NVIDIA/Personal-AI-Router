// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"strings"
	"testing"

	"nvpair-shared/noderec"

	"github.com/stretchr/testify/assert"
)

func TestHardwareSummary_ShowsAgeOfValidGPUUtilization(t *testing.T) {
	test := func(name string, age int64, want string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			info := noderec.NodeInfo{
				GPUs:           []noderec.GPUInfo{{Name: "GPU", VramBytes: 1 << 30, UtilizationPercent: 55}},
				TelemetryValid: true, MSSince: age,
			}

			line := strings.Join(hardwareSummary(info), "\n")

			assert.Contains(t, line, want)
			assert.Contains(t, line, "GPU")
			assert.Contains(t, line, "1.0 GiB")
		})
	}
	test("new sample", 0, "55% (0s ago)")
	test("negative age is clamped", -1, "55% (0s ago)")
	test("sample ten minutes old", 600000, "55% (10m ago)")
	test("sample two hours old", 7200000, "55% (2h ago)")
	test("sample two days old", 172800000, "55% (2d ago)")
}

func TestHardwareSummary_OmitsUtilizationWhenNoValidGPUSampleExists(t *testing.T) {
	info := noderec.NodeInfo{
		GPUs:           []noderec.GPUInfo{{Name: "GPU", VramBytes: 1 << 30}},
		TelemetryValid: false,
	}

	line := strings.Join(hardwareSummary(info), "\n")

	assert.Contains(t, line, "--%")
	assert.NotContains(t, line, "0%")
	assert.NotContains(t, line, "ago")
	assert.Contains(t, line, "GPU")
	assert.Contains(t, line, "1.0 GiB")
}

func TestHardwareSummary_HandlesMissingMetrics(t *testing.T) {
	assert.Empty(t, hardwareSummary(noderec.NodeInfo{}))
}

func TestHumanBytes_FormatsByteCounts(t *testing.T) {
	test := func(name string, in uint64, want string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, humanBytes(in))
		})
	}
	test("zero bytes", 0, "0 B")
	test("bytes below one KiB", 512, "512 B")
	test("one KiB", 1024, "1.0 KiB")
	test("one GiB", 1<<30, "1.0 GiB")
	test("eight GiB", 8*(1<<30), "8.0 GiB")
	test("thirty-two GiB", 32*(1<<30), "32.0 GiB")
	test("three digits omit the decimal", 128*(1<<30), "128 GiB")
	test("four TiB", 4*(1<<40), "4.0 TiB")
}
