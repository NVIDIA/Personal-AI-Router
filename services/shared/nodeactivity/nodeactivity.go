// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodeactivity rate-limits the liveness reports a proxy emits when a
// peer's engine returns inference response bytes.
//
// Both inference proxies see the same evidence and must report it the same way,
// so the decision of when a report is due lives here rather than being written
// twice. The evidence itself is worth relaying because it is the only liveness
// signal that improves as a node gets busier: a saturated peer cannot answer a
// control-plane probe, but it is demonstrably alive precisely because it is
// streaming a generation back.
//
// Bytes arrive far faster than anything needs to know about them — a streaming
// generation writes hundreds of chunks — so a report is emitted at most once per
// interval per node. The consumer treats a report as valid for far longer than
// that interval (see nvpair-node-scanner's activityFreshness), so nothing is
// lost by coalescing.
package nodeactivity

import (
	"sync"
	"time"
)

// Reporter decides when a node's activity is due to be reported again.
type Reporter struct {
	mu       sync.Mutex
	last     map[string]time.Time
	interval time.Duration
	// now reads the clock. Overridden in tests.
	now func() time.Time
}

// NewReporter builds a Reporter that lets through at most one report per
// interval per node.
func NewReporter(interval time.Duration) *Reporter {
	return &Reporter{
		last:     make(map[string]time.Time),
		interval: interval,
		now:      time.Now,
	}
}

// Due reports whether nodeID's activity should be reported now, and records the
// report when it says yes. A blank id is never due: an unidentified peer cannot
// be vouched for, and reporting it would credit the wrong node.
func (r *Reporter) Due(nodeID string) bool {
	if nodeID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if at, ok := r.last[nodeID]; ok && now.Sub(at) < r.interval {
		return false
	}
	r.last[nodeID] = now
	return true
}

// Forget drops a node's throttle state, so a node that returns after being
// removed from routing reports immediately rather than waiting out an interval
// measured from before it left.
func (r *Reporter) Forget(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.last, nodeID)
}
