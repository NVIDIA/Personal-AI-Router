// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The vendor's `llama cli --list-devices` prints one row per backend device:
//
//	Available devices:
//	  CUDA0: NVIDIA GB10 (122564 MiB, 512 MiB free)
//	  Vulkan1: Intel(R) Arc(TM) B580 Graphics (12116 MiB, 11347 MiB free)
//	  MTL0: Apple M1 (5461 MiB, 5460 MiB free)
//	  BLAS: Accelerate (0 MiB, 0 MiB free)
//	  (none)
//
// The backend name is the alphabetic prefix of the row label, as ggml names
// its devices (CUDA, ROCm, MTL, Vulkan, SYCL, OpenCL, CANN, MUSA). A row's
// presence says which backends the installed build can use on this host; it
// does not say which device a given model will land on.
var llamaDeviceRow = regexp.MustCompile(`(?m)^[\t ]*([A-Za-z]+)[0-9]*:[\t ]+(\S.*?)[\t \r]*$`)

// llamaDevicePolicies maps a lower-cased ggml device prefix to the policy name
// the receipt records; the names follow the vendor installer's variants
// (cuda, rocm, vulkan, cpu) plus the backends it has no variant for. BLAS and
// CPU rows are not accelerators.
var llamaDevicePolicies = map[string]string{
	"cuda": "cuda", "rocm": "rocm", "hip": "rocm", "mtl": "metal", "metal": "metal",
	"vulkan": "vulkan", "sycl": "sycl", "opencl": "opencl", "cann": "cann", "musa": "musa",
}

// llamaAccelerationPolicies orders backends from the most to the least
// preferred accelerator. The policy recorded for a build is the first one
// that enumerated a device; a build that lists nothing usable is "cpu".
var llamaAccelerationPolicies = []string{"cuda", "rocm", "metal", "vulkan", "sycl", "opencl", "cann", "musa"}

// llamaAcceleration classifies a `--list-devices` listing into the
// acceleration policy the receipt records and the device rows behind it.
func llamaAcceleration(listing string) (policy string, devices []string) {
	seen := map[string]bool{}
	for _, row := range llamaDeviceRow.FindAllStringSubmatch(listing, -1) {
		backend := strings.ToLower(row[1])
		if backend == "available" {
			continue // the header line
		}
		seen[llamaDevicePolicies[backend]] = true
		devices = append(devices, strings.TrimSpace(row[0]))
	}
	for _, candidate := range llamaAccelerationPolicies {
		if seen[candidate] {
			return candidate, devices
		}
	}
	return "cpu", devices
}

// llamaListDevices runs the installed executable's device enumeration with a
// bounded budget. A failure is reported to the caller; every install path
// decides for itself whether the absence of a listing is fatal.
func (e *Executor) llamaListDevices(ctx context.Context, bin string, env map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return e.runCommandOutput(ctx, []string{bin, "cli", "--list-devices"}, env)
}

// llamaReceiptAcceleration reads the acceleration facts a managed install
// recorded, for engine:status. Missing or unreadable receipts report nothing:
// an adopted external runtime or a pre-receipt install has no recorded policy.
func llamaReceiptAcceleration(installDir string) (policy string, devices []string) {
	data, err := os.ReadFile(filepath.Join(installDir, "runtime", "pair-install.json"))
	if err != nil {
		return "", nil
	}
	var receipt struct {
		Policy  string   `json:"acceleration_policy"`
		Devices []string `json:"devices"`
	}
	if json.Unmarshal(data, &receipt) != nil {
		return "", nil
	}
	return receipt.Policy, receipt.Devices
}
