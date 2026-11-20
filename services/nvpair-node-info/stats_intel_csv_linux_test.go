// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"reflect"
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
