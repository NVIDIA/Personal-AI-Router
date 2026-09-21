// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/list"
	"strconv"
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
// nodeId + Workload.id + state + scheduledOn, removals key on workloadId (see
// keyLifecycle / keyRemove). nodeId is part of the lifecycle key because
// Workload.id is only unique per-node (spec §11) — without it, the same id
// from two nodes would collide. The key still omits the workload's other
// mutable metadata, so a re-broadcast carrying an updated field it does not
// cover — a corrected error string, say — is treated as a duplicate and
// dropped rather than merged. That remains a known granularity trade-off
// (spec §4).
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
// restart, so the workload's identity is (originatedFrom, engine, runId, id):
// nodeId disambiguates nodes, and engine plus the per-process runId nonce
// disambiguate the two local engines and successive proxy runs. Dropping any
// component would collapse distinct workloads (e.g. a concurrent Ollama + LM
// Studio job both id "1") and silently discard a legitimate peer event.
//
// seq then completes the key, and it has to be the producer's event sequence
// rather than any property of the workload's current shape. This index is a
// permanent set, so anything derived from shape collides as soon as a workload
// revisits a shape it already had — and a retry does that routinely: queued on
// A, placement cleared between attempts, then queued on A again. Keyed on
// (state, scheduledOn) the third event matched the first and every peer
// dropped it, leaving their brokers holding the interim unplaced record while
// the job actually ran on A, so their schedulers stopped counting it against
// the node doing the work.
//
// state and scheduledOn stay in the key as well. They cost nothing, and they
// keep the key meaningful for a producer that has not stamped a sequence.
//
// This does not weaken what the index is for: a broadcast retry resends an
// identical frame, sequence included, so a true redelivery still dedups — and
// that is what protects a consumer from an out-of-order retry reverting a
// placement. A re-sync heartbeat bypasses this index altogether (see
// isResyncFrame).
func keyLifecycle(w *Workload) string {
	return "wl\x00" + w.OriginatedFrom + "\x00" + w.Engine + "\x00" + w.RunID + "\x00" + w.ID +
		"\x00" + string(w.State) + "\x00" + w.ScheduledOn +
		"\x00" + strconv.FormatInt(w.Seq, 10)
}

// keyRemove builds the dedup key for a removal: nodeId + workloadId. As with
// keyLifecycle, nodeId disambiguates the per-node workloadId (spec §11). When
// a legacy sender omits nodeId the empty segment still yields a stable key,
// degrading gracefully to id-only behavior.
func keyRemove(nodeID, workloadID string) string {
	return "rm\x00" + nodeID + "\x00" + workloadID
}
