// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
)

// nodeInfoOriginGate serializes requests to one node-info origin before the
// http.Client starts its request timeout. The transport carries the same
// one-connection bound, but its internal queue starts Client.Timeout too early:
// a caller waiting behind a healthy slow response can otherwise expire before
// its own request reaches the peer.
//
// Origins are retained for the daemon lifetime. Their count is bounded by the
// discovered node endpoints seen during that process, and retaining the channel
// avoids a release/delete race with a concurrent waiter.
type nodeInfoOriginGate struct {
	mu      sync.Mutex
	origins map[string]chan struct{}
}

func (g *nodeInfoOriginGate) acquire(ctx context.Context, origin string) (func(), bool) {
	permit := g.permit(origin)
	select {
	case permit <- struct{}{}:
		return func() { <-permit }, true
	case <-ctx.Done():
		return nil, false
	}
}

// tryAcquire defers candidate authentication while an origin is busy, preventing
// copied announcements from queuing ahead of the verified peer's telemetry.
func (g *nodeInfoOriginGate) tryAcquire(origin string) (func(), bool) {
	permit := g.permit(origin)
	select {
	case permit <- struct{}{}:
		return func() { <-permit }, true
	default:
		return nil, false
	}
}

// permit returns the shared slot without holding the map lock while a caller waits.
func (g *nodeInfoOriginGate) permit(origin string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	permit := g.origins[origin]
	if permit == nil {
		if g.origins == nil {
			g.origins = make(map[string]chan struct{})
		}
		permit = make(chan struct{}, 1)
		g.origins[origin] = permit
	}
	return permit
}
