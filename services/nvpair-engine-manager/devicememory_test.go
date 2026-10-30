// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"
)

func withDeviceMemoryProviders(t *testing.T, native func() (int64, int64, bool), probe func(string) (deviceMemoryProbe, error)) {
	t.Helper()
	oldNative := nativeDeviceMemory
	oldProbe := probeVLLMDeviceMemory
	nativeDeviceMemory = native
	probeVLLMDeviceMemory = probe
	t.Cleanup(func() {
		nativeDeviceMemory = oldNative
		probeVLLMDeviceMemory = oldProbe
	})
}

func TestDetectDeviceMemory_PrefersNativeTelemetry(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return 48 * gib, 32 * gib, true },
		func(string) (deviceMemoryProbe, error) {
			t.Fatal("the vLLM probe must not run when native telemetry works")
			return deviceMemoryProbe{}, nil
		},
	)

	total, free, ok := detectDeviceMemory("/opt/vllm/bin/vllm")
	if !ok || total != 48*gib || free != 32*gib {
		t.Fatalf("got total=%d free=%d ok=%v, want native telemetry", total, free, ok)
	}
}

func TestDetectDeviceMemory_FallsBackToTheSelectedVLLMBackend(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return 0, 0, false },
		func(bin string) (deviceMemoryProbe, error) {
			if bin != "/opt/vllm/bin/vllm" {
				t.Fatalf("probe bin = %q", bin)
			}
			return deviceMemoryProbe{Total: 64 * gib, Free: 40 * gib}, nil
		},
	)

	total, free, ok := detectDeviceMemory("/opt/vllm/bin/vllm")
	if !ok || total != 64*gib || free != 40*gib {
		t.Fatalf("got total=%d free=%d ok=%v, want the vLLM backend reading", total, free, ok)
	}
}

func TestDetectDeviceMemory_ReservesStartupHeadroomOnSharedMemory(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return 0, 0, false },
		func(string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: 64 * gib, Free: 20 * gib, Shared: true}, nil
		},
	)

	_, free, ok := detectDeviceMemory("vllm")
	if !ok || free != 20*gib-unifiedMemoryHeadroom {
		t.Fatalf("free = %d ok=%v, want shared memory less startup headroom", free, ok)
	}
}

func TestDetectDeviceMemory_LeavesSizingToVLLMWhenEveryProviderFails(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return 0, 0, false },
		func(string) (deviceMemoryProbe, error) { return deviceMemoryProbe{}, errors.New("unsupported backend") },
	)

	if _, _, ok := detectDeviceMemory("vllm"); ok {
		t.Fatal("a made-up memory limit is less safe than leaving sizing to vLLM")
	}
}

func TestParseDeviceMemoryProbe_ReadsTheLastOutputLine(t *testing.T) {
	reading, err := parseDeviceMemoryProbe([]byte("vLLM startup log\n{\"total\":68719476736,\"free\":42949672960,\"shared\":true}\n"))
	if err != nil || reading.Total != 64*gib || reading.Free != 40*gib || !reading.Shared {
		t.Fatalf("reading = %+v err=%v", reading, err)
	}
}

func TestParseDeviceMemoryProbe_RejectsImpossibleReadings(t *testing.T) {
	for _, output := range []string{"", "not json", `{"total":0,"free":0}`, `{"total":10,"free":11}`} {
		if _, err := parseDeviceMemoryProbe([]byte(output)); err == nil {
			t.Errorf("accepted %q", output)
		}
	}
}
