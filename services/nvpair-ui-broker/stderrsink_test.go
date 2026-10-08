// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingWriter models the parent that has stopped reading its end of the
// stderr pipe: the first write parks until released, exactly as a write into a
// full kernel pipe buffer does.
type blockingWriter struct {
	release chan struct{}

	mu   sync.Mutex
	sunk []byte
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sunk = append(w.sunk, p...)
	return len(p), nil
}

// TestStderrSinkNeverBlocksAndKeepsOverflow is the regression guard for the
// shutdown deadlock: a parent that stops reading must cost log lines a detour to
// disk, never a writer's progress. A worker blocked in write() can never reach
// process exit, and teardown waits for that exit.
func TestStderrSinkNeverBlocksAndKeepsOverflow(t *testing.T) {
	blocked := &blockingWriter{release: make(chan struct{})}
	defer close(blocked.release)

	spill := filepath.Join(t.TempDir(), "logs", stderrSpillName)
	sink := newStderrSink(blocked, spill)

	// Enough to exceed both the queue depth and the byte cap, so overflow has to
	// go somewhere other than memory.
	chunk := bytes.Repeat([]byte("x"), 1024)
	const writes = 6000

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < writes; i++ {
			if _, err := sink.Write(chunk); err != nil {
				assert.Fail(t, "test expectation failed", "Write returned an error: %v", err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "test expectation failed", "Write blocked while the reader was stalled; a worker in this state can never exit")
	}

	sink.Close()

	data, err := os.ReadFile(spill)
	require.NoError(t, err, "overflow was not preserved")
	require.NotEmpty(t, data, "spill file is empty; overflow was discarded rather than kept")
	require.NotEqual(t, int64(0), sink.spilledChunks.Load(), "no chunks recorded as spilled")
	{
		lost := sink.lostChunks.Load()
		require.Equal(t, int64(0), lost, "lost")
	}
}

// TestStderrSinkPassesThroughWhenDrained checks the ordinary path is unchanged:
// a reader that keeps up sees every byte and no spill file is created.
func TestStderrSinkPassesThroughWhenDrained(t *testing.T) {
	open := &blockingWriter{release: make(chan struct{})}
	close(open.release)

	dir := t.TempDir()
	spill := filepath.Join(dir, "logs", stderrSpillName)
	sink := newStderrSink(open, spill)

	want := []byte("broker line\n")
	for i := 0; i < 100; i++ {
		{
			_, err := sink.Write(want)
			require.NoError(t, err, "Write returned an error")
		}
	}
	sink.Close()

	open.mu.Lock()
	assert.Len(t, open.sunk, len(want)*100, "forwarded")
	open.mu.Unlock()

	{
		_, err := os.Stat(spill)
		require.ErrorIs(t, err, os.ErrNotExist, "spill file was created for a reader that kept up")
	}
}
