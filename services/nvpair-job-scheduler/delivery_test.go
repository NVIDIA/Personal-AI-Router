// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"testing"
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
		if status.LastEmittedAt != 0 || len(status.Emitted) != 0 {
			t.Fatalf("failed notification was recorded as emitted: %+v", status)
		}
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
				if len(orders) != 1 {
					t.Fatalf("%s received %d frames, want one successful delivery", engine, len(orders))
				}
				assertStrs(t, orders[0], []string{"test-node-a", "test-node-b"})
				if m.status().Engines[engine].LastEmittedAt == 0 {
					t.Fatal("successful delivery was not recorded")
				}
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
	if !equalRanks(current.Emitted, previous.Emitted) || current.LastEmittedAt != previous.LastEmittedAt {
		t.Fatal("failed update replaced the last successfully delivered snapshot")
	}
	m.recomputeAll(false)
	orders := writer.orders(engine)
	if len(orders) != 2 {
		t.Fatalf("received %d snapshots, want initial and recovered update", len(orders))
	}
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
	if got := m.status().Engines[engine].LastEmittedAt; got != 1 {
		t.Fatalf("failed forced delivery advanced timestamp to %d", got)
	}
	m.recomputeAll(false)
	if got := len(writer.orders(engine)); got != 1 {
		t.Fatalf("previously delivered unchanged ranks were emitted %d times", got)
	}
}
