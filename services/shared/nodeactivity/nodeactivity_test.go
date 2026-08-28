// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeactivity

import (
	"sync"
	"testing"
	"time"
)

// fixedClock lets a test move time without sleeping.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestReporter(interval time.Duration) (*Reporter, *fixedClock) {
	clock := &fixedClock{now: time.Unix(1700000000, 0)}
	r := NewReporter(interval)
	r.now = clock.read
	return r, clock
}

func TestFirstReportIsDueAndTheNextIsNot(t *testing.T) {
	r, _ := newTestReporter(2 * time.Second)
	if !r.Due("node-a") {
		t.Fatal("first report for a node must be due")
	}
	if r.Due("node-a") {
		t.Fatal("a second report inside the interval must be coalesced")
	}
}

func TestReportIsDueAgainAfterTheInterval(t *testing.T) {
	r, clock := newTestReporter(2 * time.Second)
	r.Due("node-a")
	clock.advance(2 * time.Second)
	if !r.Due("node-a") {
		t.Fatal("report must be due once the interval has elapsed")
	}
}

// A busy node and an idle one share the reporter, so throttling must be
// per-node: coalescing one node's chunks must not silence another's first
// report, which is the one that keeps it from being evicted.
func TestThrottleIsPerNode(t *testing.T) {
	r, _ := newTestReporter(2 * time.Second)
	if !r.Due("node-a") {
		t.Fatal("node-a first report must be due")
	}
	if !r.Due("node-b") {
		t.Fatal("node-b must not be throttled by node-a's report")
	}
}

// A node with no id cannot be vouched for, and reporting it would credit
// whichever node happened to be keyed by the empty string.
func TestBlankNodeIsNeverDue(t *testing.T) {
	r, _ := newTestReporter(2 * time.Second)
	if r.Due("") {
		t.Fatal("a blank node id must never be reported")
	}
}

func TestForgetLetsANodeReportImmediately(t *testing.T) {
	r, _ := newTestReporter(2 * time.Second)
	r.Due("node-a")
	r.Forget("node-a")
	if !r.Due("node-a") {
		t.Fatal("a forgotten node must report again without waiting out the interval")
	}
}

// Write is called from the reverse proxy's copy goroutine, one per in-flight
// request, so concurrent Due calls for the same node must neither race nor let
// more than one report through per interval.
func TestConcurrentDueLetsExactlyOneThrough(t *testing.T) {
	r, _ := newTestReporter(time.Minute)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.Due("node-a") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 1 {
		t.Fatalf("%d concurrent reports got through; want exactly 1", allowed)
	}
}
