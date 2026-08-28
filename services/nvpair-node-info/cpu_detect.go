// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log"
	"strings"

	"github.com/jaypipes/ghw"
)

// detectCPU returns the node-level CPU identity (model name + physical
// core count) via ghw. Dynamic utilization is not part of this path —
// it's refreshed separately by the stats collector and merged in by
// buildResponse.
//
// ghw is already a nvpair-node-info dependency (see gpu_other.go) and
// works on every platform we build for, so one implementation covers
// Windows, Linux, and macOS. On failure (ghw couldn't introspect the
// host, typically on stripped / sandboxed environments) we return nil
// and the caller drops the CPU object from JSON via omitempty on the
// top-level NodeInfoResponse.CPU pointer.
//
// Core-count rationale: TotalCores is the count of physical cores
// across all sockets. We deliberately don't report TotalThreads (SMT
// logical processors) because "cores" in a casual monitoring context
// is usually understood as the physical number — the logical count
// varies with BIOS settings and is the denominator PDH's
// Processor Information already normalizes against when producing
// % Processor Time, so surfacing it separately would only invite
// confusion about which number matches the utilization %.
//
// Model name is pulled from the first processor. On asymmetric / big-
// LITTLE configurations ghw returns multiple Processor entries with
// distinct Model strings; we pick the first consistently. A future
// refinement could surface a slice, but that's not warranted until we
// actually have UI that would display it.
func detectCPU() *CPUInfo {
	info, err := ghw.CPU()
	if err != nil {
		log.Printf("CPU detection error: %v", err)
		return nil
	}
	if info == nil || len(info.Processors) == 0 {
		log.Print("CPU detection returned no processors")
		return nil
	}
	name := strings.TrimSpace(info.Processors[0].Model)
	if name == "" {
		name = strings.TrimSpace(info.Processors[0].Vendor)
	}
	return &CPUInfo{
		Name:  name,
		Cores: info.TotalCores,
	}
}
