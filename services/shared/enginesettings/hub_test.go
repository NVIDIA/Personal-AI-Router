// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enginesettings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFullBaselineCoalescingAndLimits(t *testing.T) {
	h := &Hub{}
	h.Publish([]Snapshot{{Revision: 1}})
	ch, closeSub, ok := h.Subscribe()
	require.True(t, ok, "baseline missing")
	assert.Equal(t, uint64(1), (<-ch)[0].Revision, "baseline missing")
	for i := uint64(2); i < 1000; i++ {
		h.Publish([]Snapshot{{Revision: i}})
	}
	assert.Equal(t, uint64(999), (<-ch)[0].Revision, "slow subscriber missed latest full state")
	closeSub()
	var closeAll []func()
	for i := 0; i < 64; i++ {
		_, close, ok := h.Subscribe()
		require.True(t, ok, "early subscriber cap")
		closeAll = append(closeAll, close)
	}
	{
		_, _, ok := h.Subscribe()
		require.False(t, ok, "unbounded subscribers")
	}
	for _, close := range closeAll {
		close()
	}
	if _, close, ok := h.Subscribe(); !ok {
		require.FailNow(t, "test expectation failed", "cleanup leaked capacity")
	} else {
		close()
	}
}
