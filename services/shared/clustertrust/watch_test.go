// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"context"
	"testing"
	"time"
)

// TestWatchReportsTransitionAConcurrentCallerAlreadyObserved is the regression
// guard for a membership change that no watcher ever heard about.
//
// Refresh used to answer "did this change?" by diffing against the Mesh's own
// clustered field and then advancing it, so the transition was a one-shot signal
// living in state every caller shares. Watch gated its callback on that answer.
// Any other refresh landing first — and every service refreshes this Mesh from
// its request path, its enrichment loop, or an identity reload — consumed the
// transition, and Watch then saw "no change" forever.
//
// That is not hypothetical: in the reported incident the node-scanner was the
// only one of five cluster-aware workers that never logged its own node joining
// a cluster, because a broker-triggered identity reload refreshed first.
//
// The test reproduces it directly: a competing caller hammers Refresh while the
// watch is running, and the callback must still fire.
func TestWatchReportsTransitionAConcurrentCallerAlreadyObserved(t *testing.T) {
	dir := t.TempDir()
	m := Open(dir)
	if m.Clustered() {
		t.Fatal("an empty cluster dir must start unclustered")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changed := make(chan bool, 1)
	go m.Watch(ctx, func(clustered bool) {
		select {
		case changed <- clustered:
		default:
		}
	})

	// The competing caller: a gate re-deriving membership on its own schedule,
	// far more often than the watch ticks, exactly as the scanner's browse,
	// model-fetch and identity paths do.
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.Refresh()
			}
		}
	}()

	// The cluster-manager lands a join while both are running.
	certPEM, keyPEM, _ := genLeaf(t, "uuid-self")
	writeIdentity(t, dir, certPEM, keyPEM)
	writeAdmission(t, dir, "cluster-abc", 1)

	select {
	case clustered := <-changed:
		if !clustered {
			t.Fatal("the watch reported a transition to unclustered, want clustered")
		}
	case <-time.After(4 * RefreshInterval):
		t.Fatal("the watch never reported the join: a concurrent Refresh consumed the transition")
	}
}
