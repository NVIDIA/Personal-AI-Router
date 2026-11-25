// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"nvpair-shared/noderec"
)

// The block production always resolves: these are the ports the installer's
// firewall rules, desktop pairing, and every shipped peer's discovery record
// already assume, so a regression here breaks existing clusters silently.
func TestPortsForBase_CanonicalBaseMatchesTheFixedBlock(t *testing.T) {
	ports := portsForBase(noderec.DefaultPortBase)

	assert.Equal(t, 14318, ports.nodeInfoHTTP, "ni")
	assert.Equal(t, 14319, ports.errorsHTTP, "er")
	assert.Equal(t, 14320, ports.workloadHTTP, "wl")
	assert.Equal(t, 14321, ports.clusterManagerHTTP, "cl")
	assert.Equal(t, 14322, ports.engineManagerHTTP, "em")
	assert.Equal(t, 14323, ports.engineControl, "ec")
}

// A shifted deployment moves all six ports by the same amount and keeps every
// offset, so a worker told its port by the broker and a peer that read the
// advertised ni port agree on the rest of the block.
func TestPortsForBase_ShiftedBaseMovesEveryField(t *testing.T) {
	const shiftedBase = 20000
	ports := portsForBase(shiftedBase)

	canonical := portsForBase(noderec.DefaultPortBase)
	cases := []struct {
		name      string
		shifted   int
		canonical int
	}{
		{"nodeInfoHTTP", ports.nodeInfoHTTP, canonical.nodeInfoHTTP},
		{"errorsHTTP", ports.errorsHTTP, canonical.errorsHTTP},
		{"workloadHTTP", ports.workloadHTTP, canonical.workloadHTTP},
		{"clusterManagerHTTP", ports.clusterManagerHTTP, canonical.clusterManagerHTTP},
		{"engineManagerHTTP", ports.engineManagerHTTP, canonical.engineManagerHTTP},
		{"engineControl", ports.engineControl, canonical.engineControl},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, shiftedBase+(c.canonical-noderec.DefaultPortBase), c.shifted,
				"%s must hold its offset from the base", c.name)
		})
	}
	assert.Len(t, ports.all(), noderec.PortBlockSize,
		"the block must cover exactly PortBlockSize ports")
}
