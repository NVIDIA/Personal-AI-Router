// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unavailableWriter is an in-memory test fixture; it opens no files or sockets.
type unavailableWriter struct{}

func (unavailableWriter) Read([]byte) (int, error)  { return 0, io.EOF }
func (unavailableWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestFailedNotificationDoesNotAdvanceStatus(t *testing.T) {
	m := mgrWith(unavailableWriter{}, []string{"test-node-a"})
	m.recomputeAll(false)
	for _, engine := range schedulerEngines {
		status := m.status().Engines[engine]
		assert.Equal(t, int64(0), status.LastEmittedAt, "failed notification was recorded as emitted (%v)", status)
		require.Empty(t, status.Emitted, "failed notification was recorded as emitted (%v)", status)
	}
}

// recoveringWriter simulates one failed local codec write, then records frames.
type recoveringWriter struct {
	capRW
	remaining  int
	shortWrite bool
}

func (w *recoveringWriter) Write(data []byte) (int, error) {
	if w.remaining > 0 {
		w.remaining--
		if w.shortWrite {
			return 0, nil
		}
		return 0, io.ErrClosedPipe
	}
	return w.capRW.Write(data)
}

func TestFailedNotificationRetriesWithoutChangingRanks(t *testing.T) {
	for _, shortWrite := range []bool{false, true} {
		name := "write-error"
		if shortWrite {
			name = "zero-byte-write"
		}
		t.Run(name, func(t *testing.T) {
			writer := &recoveringWriter{remaining: 1, shortWrite: shortWrite}
			m := mgrWith(writer, []string{"test-node-a", "test-node-b"})
			m.recomputeAll(false)
			m.recomputeAll(false)
			m.recomputeAll(false)
			for _, engine := range schedulerEngines {
				orders := writer.orders(engine)
				require.Len(t, orders, 1, " (%v)", engine)
				assertStrs(t, orders[0], []string{"test-node-a", "test-node-b"})
				assert.NotEqual(t, int64(0), m.status().Engines[engine].LastEmittedAt, "successful delivery was not recorded")
			}
		})
	}
}

func TestFailedChangedNotificationRetainsDeliveredRanks(t *testing.T) {
	writer := &recoveringWriter{}
	m := mgrWith(writer, []string{"test-node-a", "test-node-b"})
	m.recomputeAll(false)
	engine := schedulerEngines[0]
	previous := m.status().Engines[engine]
	writer.remaining = 1
	m.nodes["test-node-c"] = true
	m.recomputeAll(false)
	current := m.status().Engines[engine]
	assert.True(t, equalRanks(current.Emitted, previous.Emitted), "failed update replaced the last successfully delivered snapshot")
	assert.Equal(t, previous.LastEmittedAt, current.LastEmittedAt, "failed update replaced the last successfully delivered snapshot")
	m.recomputeAll(false)
	orders := writer.orders(engine)
	require.Len(t, orders, 2, "received")
	assertStrs(t, orders[1], []string{"test-node-a", "test-node-b", "test-node-c"})
}

func TestFailedForcedNotificationRetainsDeliveredTimestamp(t *testing.T) {
	writer := &recoveringWriter{}
	m := mgrWith(writer, []string{"test-node-a"})
	m.recomputeAll(false)
	engine := schedulerEngines[0]
	m.emitted[engine] = engineState{ranks: m.emitted[engine].ranks, lastEmittedAt: 1}
	writer.remaining = 1
	m.recomputeAll(true)
	{
		got := m.status().Engines[engine].LastEmittedAt
		assert.Equal(t, int64(1), got, "failed forced delivery advanced timestamp to")
	}
	m.recomputeAll(false)
	assert.Len(t, writer.orders(engine), 1, "previously delivered unchanged ranks were emitted")
}
