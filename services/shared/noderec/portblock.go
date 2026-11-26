// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import "fmt"

const (
	// DefaultPortBase is the canonical first port of the six-port service block
	// (ni, er, wl, cl, em, ec) and what every installer firewall rule, desktop
	// pairing flow, and standalone worker default assumes. A shifted block —
	// e.g. a test harness running beside a live deployment — keeps the offsets.
	DefaultPortBase = 14318
	// PortBlockSize is the number of consecutive ports the block occupies.
	PortBlockSize = 6
	// MinPortBase is the lowest base that keeps every port of the block
	// unprivileged.
	MinPortBase = 1024
)

// blockOffsets is the fixed offset of each block service from the base. The
// engine facade keys (ol/lm/lc/vl) are absent: their ports are assigned per
// engine and have no fixed relationship to the block.
var blockOffsets = map[ServiceKey]int{
	ServiceNodeInfo:      0,
	ServiceErrors:        1,
	ServiceWorkload:      2,
	ServiceCluster:       3,
	ServiceEngineManager: 4,
	ServiceEngineControl: 5,
}

// ServicePort returns the port for key when the service block starts at base,
// and reports whether key is part of the block. The engine facade keys report
// false with port 0.
func ServicePort(base int, key ServiceKey) (int, bool) {
	offset, ok := blockOffsets[key]
	if !ok {
		return 0, false
	}
	return base + offset, true
}

// ValidatePortBase rejects a base whose block would run off the end of the
// 16-bit port space or onto a privileged port, naming the accepted range so a
// caller can report the fix without consulting documentation.
func ValidatePortBase(base int) error {
	if base < MinPortBase || base+PortBlockSize-1 > 65535 {
		return fmt.Errorf("port base %d outside %d..%d (the block needs %d consecutive ports)",
			base, MinPortBase, 65536-PortBlockSize, PortBlockSize)
	}
	return nil
}
