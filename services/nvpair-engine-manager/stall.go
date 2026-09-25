// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Transfer budgets. A fixed wall-clock budget failed real installs on slow
// links (a 680 MB CUDA runtime at 0.3 MB/s needs 38 minutes), so long
// transfers are bounded by the absence of progress instead: the operation is
// cancelled when nothing has moved for llamaStallIdle, and unconditionally at
// llamaTransferMax. Vars so tests can shrink them.
var (
	llamaStallIdle   = 10 * time.Minute
	llamaTransferMax = 6 * time.Hour
	stallPollEvery   = 15 * time.Second
)

type stallKey struct{}

// stallContext is a context that is cancelled when no progress has been
// reported for idle, or after max in total. Progress is reported with Touch;
// the download and installer paths call touchStall(ctx) so any context that
// carries a stallContext keeps its transfer alive while bytes still move.
type stallContext struct {
	context.Context
	cancel context.CancelCauseFunc
	mu     sync.Mutex
	last   time.Time
	idle   time.Duration
	stop   chan struct{}
	once   sync.Once
}

func newStallContext(parent context.Context, idle, max time.Duration) *stallContext {
	ctx, cancel := context.WithCancelCause(parent)
	s := &stallContext{cancel: cancel, last: time.Now(), idle: idle, stop: make(chan struct{})}
	s.Context = context.WithValue(ctx, stallKey{}, s)
	started := time.Now()
	go func() {
		t := time.NewTicker(stallPollEvery)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.mu.Lock()
				last := s.last
				s.mu.Unlock()
				if now.Sub(last) > s.idle {
					cancel(fmt.Errorf("no download progress for %s", s.idle.Truncate(time.Second)))
					return
				}
				if now.Sub(started) > max {
					cancel(fmt.Errorf("transfer did not finish within %s", max.Truncate(time.Second)))
					return
				}
			}
		}
	}()
	return s
}

// Touch records progress.
func (s *stallContext) Touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

// Stop ends the watcher and releases the context.
func (s *stallContext) Stop() {
	s.once.Do(func() {
		close(s.stop)
		s.cancel(nil)
	})
}

// touchStall reports progress to the stall context carried by ctx, if any.
func touchStall(ctx context.Context) {
	if s, ok := ctx.Value(stallKey{}).(*stallContext); ok {
		s.Touch()
	}
}

// hasStall reports whether ctx is bounded by a stall context, in which case
// callers skip their own fixed wall-clock timeout for the same transfer.
func hasStall(ctx context.Context) bool {
	_, ok := ctx.Value(stallKey{}).(*stallContext)
	return ok
}

// transferError explains a command or download failure that happened because
// ctx ended with a recorded cause (a stall), and returns err unchanged when
// the context is still live or ended for an ordinary reason.
func transferError(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	if cause := context.Cause(ctx); cause != nil && cause != ctx.Err() {
		return fmt.Errorf("%w (%v)", cause, err)
	}
	return err
}

// boundedCapture keeps the first and the last limit bytes written to it, the
// same shape os/exec keeps for ExitError.Stderr, so a diagnostic that arrives
// after a long progress stream is still there to classify.
type boundedCapture struct {
	limit int
	head  []byte
	tail  []byte
	total int
}

func newBoundedCapture(limit int) *boundedCapture { return &boundedCapture{limit: limit} }

func (b *boundedCapture) Write(p []byte) (int, error) {
	b.total += len(p)
	if room := b.limit - len(b.head); room > 0 {
		b.head = append(b.head, p[:min(room, len(p))]...)
	}
	b.tail = append(b.tail, p...)
	if len(b.tail) > b.limit {
		b.tail = append(b.tail[:0:0], b.tail[len(b.tail)-b.limit:]...)
	}
	return len(p), nil
}

// Bytes returns everything when it fits, or head and tail joined without
// repeating the bytes they share.
func (b *boundedCapture) Bytes() []byte {
	switch {
	case b.total <= b.limit:
		return b.head
	case b.total <= 2*b.limit:
		return append(append([]byte{}, b.head...), b.tail[len(b.tail)-(b.total-b.limit):]...)
	}
	return append(append(append([]byte{}, b.head...), "\n[...]\n"...), b.tail...)
}

// watchTreeGrowth reports progress while files beneath root keep growing. The
// vendor installer runs curl silently, so the bytes it writes into its staging
// home are the only progress signal; onGrowth also lets the caller emit a
// heartbeat so a UI waiting on the install does not time out. It returns when
// ctx ends or stop is closed.
func watchTreeGrowth(ctx context.Context, root string, every time.Duration, onGrowth func(bytes int64), stop <-chan struct{}) {
	var last int64 = -1
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			var total int64
			_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if info, statErr := d.Info(); statErr == nil {
						total += info.Size()
					}
				}
				return nil
			})
			if total != last {
				last = total
				touchStall(ctx)
				if onGrowth != nil {
					onGrowth(total)
				}
			}
		}
	}
}
