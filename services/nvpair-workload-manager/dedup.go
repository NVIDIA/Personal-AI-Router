// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/list"
	"sync"
)

// defaultDedupCapacity is the bounded size of the dedup index. Sized for
// session-scoped volume at ~dozen-node scale (spec §5).
const defaultDedupCapacity = 10000

// dedupIndex is a bounded LRU set of keys. It answers a single question:
// "have I seen this key before?" and records it if not, evicting the
// least-recently-seen key once capacity is exceeded. It is safe for
// concurrent use — the inter-node HTTP handler runs one goroutine per
// request.
//
// Keys are opaque strings built by the caller: lifecycle events key on
// nodeId + Workload.id + state, removals key on workloadId (see keyLifecycle
// / keyRemove). nodeId is part of the lifecycle key because Workload.id is
// only unique per-node (spec §11) — without it, the same id from two nodes
// would collide. Keying on (nodeId, id, state) means a re-broadcast carrying
// the same triple but updated metadata is treated as a duplicate and dropped,
// not merged — a known and accepted granularity trade-off (spec §4).
type dedupIndex struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List               // front = most recently seen
	items    map[string]*list.Element // key -> element in ll
}

func newDedupIndex(capacity int) *dedupIndex {
	if capacity <= 0 {
		capacity = defaultDedupCapacity
	}
	return &dedupIndex{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[string]*list.Element, capacity),
	}
}

// seenOrAdd returns true if the key was already present (a duplicate). On a
// first sighting it records the key and returns false. Either way the key is
// promoted to most-recently-seen.
func (d *dedupIndex) seenOrAdd(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if el, ok := d.items[key]; ok {
		d.ll.MoveToFront(el)
		return true
	}

	el := d.ll.PushFront(key)
	d.items[key] = el
	if d.ll.Len() > d.capacity {
		oldest := d.ll.Back()
		if oldest != nil {
			d.ll.Remove(oldest)
			delete(d.items, oldest.Value.(string))
		}
	}
	return false
}

// keyLifecycle builds the dedup key for a lifecycle event. Workload.id is only
// a per-process counter, and both engine proxies count from 1 and reset on
// restart, so the full identity is (originatedFrom, engine, runId, id): nodeId
// disambiguates nodes, engine + the per-process runId nonce disambiguate the
// two local engines and successive proxy runs, and state completes the
// lifecycle-step key. Dropping any component would collapse distinct workloads
// (e.g. a concurrent Ollama + LM Studio job both id "1") and silently discard a
// legitimate peer event.
func keyLifecycle(w *Workload) string {
	return "wl\x00" + w.OriginatedFrom + "\x00" + w.Engine + "\x00" + w.RunID + "\x00" + w.ID + "\x00" + string(w.State)
}

// keyRemove builds the dedup key for a removal: nodeId + workloadId. As with
// keyLifecycle, nodeId disambiguates the per-node workloadId (spec §11). When
// a legacy sender omits nodeId the empty segment still yields a stable key,
// degrading gracefully to id-only behavior.
func keyRemove(nodeID, workloadID string) string {
	return "rm\x00" + nodeID + "\x00" + workloadID
}
