// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestVLLMRankDevicePolicyBindsSelectedGPUAndExactUverbs(t *testing.T) {
	root := t.TempDir()
	gpuRoot, ibRoot, devRoot := filepath.Join(root, "proc"), filepath.Join(root, "sys"), filepath.Join(root, "dev")
	if err := os.MkdirAll(filepath.Join(gpuRoot, "0000_01_00.0"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gpuRoot, "0000_01_00.0", "information"), []byte("GPU UUID: GPU-11111111-1111-1111-1111-111111111111\nDevice Minor: 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for index, hca := range []string{"mlx5_0", "mlx5_1", "rocep1s0f0", "rocep1s0f1"} {
		if err := os.MkdirAll(filepath.Join(ibRoot, hca, "device", "infiniband_verbs", "uverbs"+string(rune('0'+index))), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range []struct{ hcas, verbs [2]string }{
		{[2]string{"mlx5_0", "mlx5_1"}, [2]string{"uverbs0", "uverbs1"}},
		{[2]string{"rocep1s0f0", "rocep1s0f1"}, [2]string{"uverbs2", "uverbs3"}},
	} {
		lanes := []vllmGroupRDMALane{{RDMADevice: pair.hcas[0]}, {RDMADevice: pair.hcas[1]}}
		devices, err := vllmRankDevicePathsAt("GPU-11111111-1111-1111-1111-111111111111", lanes, gpuRoot, ibRoot, devRoot, func(string) bool { return true })
		want := []string{filepath.Join(devRoot, "infiniband", pair.verbs[0]), filepath.Join(devRoot, "infiniband", pair.verbs[1]), filepath.Join(devRoot, "nvidia-modeset"), filepath.Join(devRoot, "nvidia-uvm"), filepath.Join(devRoot, "nvidia-uvm-tools"), filepath.Join(devRoot, "nvidia0"), filepath.Join(devRoot, "nvidiactl")}
		slices.Sort(want)
		if err != nil || !slices.Equal(devices, want) {
			t.Fatalf("device policy differs for %v: %v %v", pair.hcas, devices, err)
		}
		if _, err := vllmRankDevicePathsAt("GPU-22222222-2222-2222-2222-222222222222", lanes, gpuRoot, ibRoot, devRoot, func(string) bool { return true }); err == nil {
			t.Fatal("unmatched GPU was accepted")
		}
		if _, err := vllmRankDevicePathsAt("GPU-11111111-1111-1111-1111-111111111111", lanes, gpuRoot, ibRoot, devRoot, func(name string) bool { return filepath.Base(name) != pair.verbs[1] }); err == nil {
			t.Fatal("missing uverbs character device was accepted")
		}
	}
}

func TestVLLMExactCharacterDeviceRejectsRedirects(t *testing.T) {
	root := t.TempDir()
	target, redirected := filepath.Join(root, "target"), filepath.Join(root, "redirected")
	if err := os.WriteFile(target, []byte("not a device"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, redirected); err != nil {
		t.Skipf("symlink unavailable on this host: %v", err)
	}
	if vllmExactCharacterDevice(target) || vllmExactCharacterDevice(redirected) {
		t.Fatal("regular or redirected path was accepted as an exact character device")
	}
}
