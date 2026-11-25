// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixed block every shipped installer, firewall rule, and desktop pairing
// expects. This test is the guard against a reordered offset table.
func TestServicePort_CanonicalBaseReproducesTheFixedBlock(t *testing.T) {
	cases := []struct {
		key  ServiceKey
		want int
	}{
		{ServiceNodeInfo, 14318},
		{ServiceErrors, 14319},
		{ServiceWorkload, 14320},
		{ServiceCluster, 14321},
		{ServiceEngineManager, 14322},
		{ServiceEngineControl, 14323},
	}
	for _, c := range cases {
		t.Run(string(c.key), func(t *testing.T) {
			got, ok := ServicePort(DefaultPortBase, c.key)
			require.True(t, ok, "%s must be part of the port block", c.key)
			assert.Equal(t, c.want, got)
		})
	}
}

// A shifted deployment moves the whole block and keeps every relative offset,
// so a peer that learns the base can derive all six ports from any one of them.
func TestServicePort_ShiftedBaseKeepsOffsets(t *testing.T) {
	const shiftedBase = 20000
	block := []ServiceKey{
		ServiceNodeInfo, ServiceErrors, ServiceWorkload,
		ServiceCluster, ServiceEngineManager, ServiceEngineControl,
	}
	for i, key := range block {
		t.Run(fmt.Sprintf("%s_at_offset_%d", key, i), func(t *testing.T) {
			got, ok := ServicePort(shiftedBase, key)
			require.True(t, ok, "%s must be part of the port block", key)
			assert.Equal(t, shiftedBase+i, got)

			canonical, ok := ServicePort(DefaultPortBase, key)
			require.True(t, ok)
			assert.Equal(t, canonical-DefaultPortBase, got-shiftedBase,
				"offset from the base must not change with the base")
		})
	}
}

// Engine facade ports (ol/lm/lc/vl) are assigned per engine and have no fixed
// relationship to the block, so a caller must not be able to derive one from it.
func TestServicePort_EngineFacadeKeysAreNotInTheBlock(t *testing.T) {
	for _, key := range []ServiceKey{ServiceOllama, ServiceLMStudio, ServiceLlamaCPP, ServiceVLLM} {
		t.Run(string(key), func(t *testing.T) {
			for _, base := range []int{DefaultPortBase, 20000, MinPortBase} {
				got, ok := ServicePort(base, key)
				assert.False(t, ok, "%s has no block port, but ServicePort returned %d", key, got)
				assert.Zero(t, got, "a key outside the block must return port 0")
			}
		})
	}
}

// Any base whose six consecutive ports stay in the unprivileged port space is
// usable; anything that would push the block off the edge or onto a privileged
// port is refused with a message stating the allowed range.
func TestValidatePortBase_AcceptsRangesThatFitTheBlock(t *testing.T) {
	for _, base := range []int{MinPortBase, DefaultPortBase, 65536 - PortBlockSize} {
		t.Run(fmt.Sprintf("accepts_%d", base), func(t *testing.T) {
			require.NoError(t, ValidatePortBase(base))
		})
	}

	for _, base := range []int{MinPortBase - 1, 65536 - PortBlockSize + 1, 65535, 0, -1} {
		t.Run(fmt.Sprintf("rejects_%d", base), func(t *testing.T) {
			err := ValidatePortBase(base)
			require.Error(t, err, "base %d must be rejected", base)
			assert.Contains(t, err.Error(), fmt.Sprintf("%d..%d",
				MinPortBase, 65536-PortBlockSize), "error must name the allowed range")
			assert.True(t, strings.Contains(err.Error(), fmt.Sprintf("%d", base)),
				"error must name the rejected base: %v", err)
		})
	}
}
