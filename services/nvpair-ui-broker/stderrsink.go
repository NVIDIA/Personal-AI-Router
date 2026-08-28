// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/appdir"
)

// stderrOut is where this process and every worker it spawns send log output.
// main replaces it with a stderrSink; the default keeps direct spawns (tests)
// working unchanged.
var stderrOut io.Writer = os.Stderr

const (
	// stderrQueueDepth bounds how many pending chunks the sink holds in memory.
	stderrQueueDepth = 4096

	// stderrQueueMaxBytes bounds the memory those chunks may occupy, so a reader
	// that stops for good cannot grow this process without limit. Sized well past
	// observed volume — the busiest minute recorded on a clustered node at debug
	// level was about 19KB — so an ordinary stall never reaches the spill file.
	stderrQueueMaxBytes = 4 << 20

	// stderrDrainTimeout bounds how long Close waits for queued output to reach
	// the real stderr before sending the tail to the spill file instead. A parent
	// that has stopped reading will never accept it, and refusing to exit is the
	// one outcome worse than writing it somewhere else.
	stderrDrainTimeout = 2 * time.Second

	// stderrSpillName is the overflow log, written beside the app's other logs.
	stderrSpillName = "nvpair-broker-unsent.log"
)

// stderrSink decouples every log writer in this process tree from the parent's
// read rate.
//
// The parent is the only reader of this process's stderr pipe. Pointing each
// worker's cmd.Stderr straight at os.Stderr shared that one pipe across all
// twelve processes, so a parent that stopped reading filled its kernel buffer
// (64KiB on macOS) and left a worker blocked inside write(). A worker blocked in
// write() never reaches process exit, and teardown joins each worker's exit, so
// shutdown deadlocked until the parent force-killed this process — which
// orphaned whichever workers were still alive.
//
// Assigning a non-file writer here also makes os/exec give each worker its own
// pipe drained by a goroutine in this process, so no worker's write can reach
// the parent's buffer at all.
//
// Overflow goes to a file rather than being discarded: the lines that matter
// most are the ones written while something is going wrong, which is exactly
// when a reader falls behind. Output is only unrecoverable if there is no
// writable data directory to spill into, and that case is counted and reported.
type stderrSink struct {
	out         io.Writer
	queue       chan []byte
	queuedBytes atomic.Int64
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	closing     atomic.Bool

	spillPath       string
	spillMu         sync.Mutex
	spillFile       *os.File
	spillOpenFailed bool
	spilledChunks   atomic.Int64
	lostChunks      atomic.Int64

	// reportedSpill is touched only by the drain goroutine.
	reportedSpill int64
}

func newStderrSink(out io.Writer, spillPath string) *stderrSink {
	s := &stderrSink{
		out:       out,
		queue:     make(chan []byte, stderrQueueDepth),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		spillPath: spillPath,
	}
	go s.run()
	return s
}

// defaultStderrSpillPath resolves the overflow log beside the app's other logs.
// Returns "" when no per-user data directory can be resolved, which leaves the
// sink with nowhere durable to put overflow.
func defaultStderrSpillPath() string {
	p, err := appdir.Path("logs", stderrSpillName)
	if err != nil {
		return ""
	}
	return p
}

// Write never blocks and never fails: it reports the full length so neither slog
// nor os/exec's stderr copier treats overflow as a write error. The chunk is
// copied because os/exec reuses its copy buffer.
func (s *stderrSink) Write(p []byte) (int, error) {
	chunk := make([]byte, len(p))
	copy(chunk, p)

	if !s.closing.Load() && s.queuedBytes.Load()+int64(len(chunk)) <= stderrQueueMaxBytes {
		select {
		case s.queue <- chunk:
			s.queuedBytes.Add(int64(len(chunk)))
			return len(p), nil
		default:
		}
	}

	s.writeSpill(chunk)
	return len(p), nil
}

// run forwards queued chunks to the real stderr until Close, then flushes
// whatever is already buffered. The queue is deliberately never closed, so a
// worker's copier goroutine can keep writing during teardown without racing a
// send on a closed channel.
func (s *stderrSink) run() {
	defer close(s.done)
	for {
		select {
		case chunk := <-s.queue:
			s.consume(chunk)
		case <-s.stop:
			for {
				select {
				case chunk := <-s.queue:
					s.consume(chunk)
				default:
					return
				}
			}
		}
	}
}

func (s *stderrSink) consume(chunk []byte) {
	s.queuedBytes.Add(-int64(len(chunk)))
	s.reportSpill()
	_, _ = s.out.Write(chunk)
}

// reportSpill accounts for chunks diverted to the spill file since the last
// report, so a gap in the piped log names the file that holds the rest.
func (s *stderrSink) reportSpill() {
	n := s.spilledChunks.Load()
	if n == s.reportedSpill {
		return
	}
	s.reportedSpill = n

	if lost := s.lostChunks.Load(); lost > 0 {
		_, _ = fmt.Fprintf(s.out,
			"[nvpair-ui-broker] WARN log reader fell behind; %d chunk(s) overflowed, %d lost with no writable spill file\n",
			n, lost)
		return
	}
	_, _ = fmt.Fprintf(s.out,
		"[nvpair-ui-broker] WARN log reader fell behind; %d chunk(s) written to %s\n",
		n, s.spillPath)
}

func (s *stderrSink) writeSpill(chunk []byte) {
	s.spillMu.Lock()
	defer s.spillMu.Unlock()

	s.spilledChunks.Add(1)
	f := s.openSpillLocked()
	if f == nil {
		s.lostChunks.Add(1)
		return
	}
	if _, err := f.Write(chunk); err != nil {
		s.lostChunks.Add(1)
	}
}

// openSpillLocked opens the spill file on first overflow. It is truncated per
// run: the file exists to explain a gap in the current session's log, and an
// append that nothing ever reads would grow without bound.
func (s *stderrSink) openSpillLocked() *os.File {
	if s.spillFile != nil {
		return s.spillFile
	}
	if s.spillOpenFailed || s.spillPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.spillPath), 0o755); err != nil {
		s.spillOpenFailed = true
		return nil
	}
	f, err := os.OpenFile(s.spillPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		s.spillOpenFailed = true
		return nil
	}
	s.spillFile = f
	return f
}

// Close stops forwarding and gives queued output a bounded window to reach the
// real stderr. Anything still queued after that goes to the spill file, and
// later writes bypass the queue so nothing is stranded behind a reader that is
// never coming back.
func (s *stderrSink) Close() {
	s.closeOnce.Do(func() {
		close(s.stop)
		select {
		case <-s.done:
		case <-time.After(stderrDrainTimeout):
		}
		s.closing.Store(true)
		s.drainQueueToSpill()
		s.closeSpill()
	})
}

func (s *stderrSink) drainQueueToSpill() {
	for {
		select {
		case chunk := <-s.queue:
			s.queuedBytes.Add(-int64(len(chunk)))
			s.writeSpill(chunk)
		default:
			return
		}
	}
}

func (s *stderrSink) closeSpill() {
	s.spillMu.Lock()
	defer s.spillMu.Unlock()
	if s.spillFile == nil {
		return
	}
	_ = s.spillFile.Close()
	s.spillFile = nil
}
