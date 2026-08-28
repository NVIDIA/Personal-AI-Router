// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package workloadstore is the broker's authoritative, in-memory index of
// cluster workloads (current + historic). It replaces the broker's former
// live-only, arrival-order map with an order-independent merge so a workload's
// display state converges no matter what order the underlying lifecycle events
// arrive in.
//
// A workload is keyed globally by (originatedFrom, engine, runId, id). The bare
// request id isn't unique — each engine proxy counts from 1 and resets on
// restart — so engine plus the proxy's per-process runId nonce make the identity
// unique across concurrent engines and successive runs. The merge rule (see
// apply) delivers:
//
//   - Order-independence: within one identity, state only moves forward
//     (queued < running < terminal), so a stale "running" arriving after a
//     "failed" is rejected rather than resurrecting the workload.
//   - Provenance: an authoritative event (from the origin) overrides a locally
//     inferred guess (the node-loss sweep), while an inferred guess may only
//     fail an authoritative non-terminal record — this lets a returning node
//     reconcile away a wrongly-inferred "failed".
//   - Generation gate: createdAt orders events for one identity (defensive;
//     with runId in the key an identity has a single createdAt), rejecting a
//     stale generation before provenance/rank are considered.
//
// All events for a given identity are minted by the same node's proxy, so
// createdAt comparisons never cross clocks. The store is authoritative for this
// node's local display/merge only; it is not a licence to broadcast another
// node's workloads (the origin remains the single writer on the wire).
//
// The store is safe for concurrent use: the broker applies events from the
// proxy- and workload-manager-reader goroutines and sweeps from the
// scanner-event goroutine.
package workloadstore

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Key identifies a workload cluster-wide: its origin node and the
// origin-assigned id. Two nodes can independently mint id "1", so the origin is
// part of the key.
type Key struct {
	Origin string
	Engine string
	RunID  string
	ID     string
}

// Record is the store's entry for one workload. Info is the full-fidelity
// workloadInfo JSON exactly as received, so the store never duplicates (and
// risks drifting from) the Workload schema — the projected fields below are
// only what the merge rule and the node-loss sweep need.
type Record struct {
	Origin      string
	ID          string
	State       string
	CreatedAt   int64
	CompletedAt int64
	ScheduledOn string
	Terminal    bool
	// Inferred marks a record set by a local guess (the broker's node-loss
	// sweep) rather than an authoritative event from the origin. An inferred
	// state always yields to the origin's authoritative truth on reconciliation,
	// and is never persisted to disk.
	Inferred bool
	Info     json.RawMessage
	// LastUpdated is the store's own clock (ms) at the last accepted change —
	// distinct from the workload's createdAt/completedAt, which are the origin
	// proxy's clock.
	LastUpdated int64
	// LastSeen is when the ORIGIN last asserted anything about this workload,
	// including a re-assertion that changed nothing. It is deliberately not
	// LastUpdated: the origin's anti-entropy heartbeat re-asserts each of its
	// non-terminal workloads on every interval, and those re-assertions are no-op
	// merges, so LastUpdated cannot tell "the origin still says this is running"
	// apart from "the origin has gone quiet about it". That distinction is what
	// makes a stale non-terminal record detectable. Only authoritative events
	// refresh it — a local inferred guess must never make a record look freshly
	// confirmed.
	//
	// It is a time.Time, not an epoch-ms int64 like every other timestamp here,
	// because it is the only one measured as an ELAPSED interval rather than
	// stamped onto the wire. Subtracting two time.Time values uses their
	// monotonic readings, so the staleness decision is immune to a wall-clock
	// step — an NTP correction or a resume larger than the staleness budget would
	// otherwise mark every remote workload stale at once. It can safely differ in
	// type precisely because it is internal: unlike CreatedAt, CompletedAt and
	// LastUpdated it is neither persisted nor carried in any payload.
	LastSeen time.Time
}

// Incoming is a parsed workload lifecycle upsert handed to Apply. Info carries
// the full workloadInfo so an accepted event can be forwarded/persisted whole.
type Incoming struct {
	Origin      string
	Engine      string
	RunID       string
	ID          string
	State       string
	CreatedAt   int64
	CompletedAt int64
	ScheduledOn string
	Info        json.RawMessage
}

// Store is the in-memory workload index. Persistence and RPC wiring live in
// separate files; this file is pure state + the merge rule.
type Store struct {
	mu      sync.Mutex
	records map[Key]Record
	// now is the store clock; overridable in tests. It returns a time.Time rather
	// than epoch ms so elapsed-interval comparisons (LastSeen) can use monotonic
	// readings; the epoch-ms fields derive from it with UnixMilli at the wire and
	// persistence boundaries.
	now func() time.Time

	// Persistence config + state (see persistence.go). Zero-valued when
	// persistence is disabled (path == ""). dirty is set whenever the historic
	// (terminal) set changes, so the coalescing flusher only writes on a real
	// change.
	path       string
	rotations  int
	historyCap int
	maxAgeMs   int64
	dirty      bool
}

// New returns an empty store.
func New() *Store {
	return &Store{
		records: make(map[Key]Record),
		now:     time.Now,
	}
}

// ParseIncoming extracts the merge-relevant fields from a workloadInfo object,
// copying the raw bytes so the caller's buffer isn't aliased. It reports false
// for a payload missing the required identity fields (id, originatedFrom).
func ParseIncoming(info json.RawMessage) (Incoming, bool) {
	var hdr struct {
		ID             string `json:"id"`
		Engine         string `json:"engine"`
		RunID          string `json:"runId"`
		State          string `json:"state"`
		OriginatedFrom string `json:"originatedFrom"`
		ScheduledOn    string `json:"scheduledOn"`
		CreatedAt      int64  `json:"createdAt"`
		CompletedAt    *int64 `json:"completedAt"`
	}
	if err := json.Unmarshal(info, &hdr); err != nil || hdr.ID == "" || hdr.OriginatedFrom == "" {
		return Incoming{}, false
	}
	var completedAt int64
	if hdr.CompletedAt != nil {
		completedAt = *hdr.CompletedAt
	}
	return Incoming{
		Origin:      hdr.OriginatedFrom,
		Engine:      hdr.Engine,
		RunID:       hdr.RunID,
		ID:          hdr.ID,
		State:       hdr.State,
		CreatedAt:   hdr.CreatedAt,
		CompletedAt: completedAt,
		ScheduledOn: hdr.ScheduledOn,
		Info:        append(json.RawMessage(nil), info...),
	}, true
}

// Apply merges an incoming lifecycle event into the store and reports whether
// it was a real, forward change worth propagating (to clients, the scheduler,
// and — for a terminal — persistence). A false return means the event was a
// stale/regressive/no-op duplicate and must be dropped, which is exactly what
// prevents an out-of-order "running" from resurrecting a finished workload.
//
// Apply merges an authoritative event (a real transition from the origin) and
// reports whether it was a forward change worth propagating.
func (s *Store) Apply(in Incoming) bool { return s.apply(in, false) }

// ApplyInferred merges a locally-inferred event — the broker's node-loss sweep
// guessing a departed node's jobs failed. An inferred state always yields to the
// origin's authoritative truth (see apply), so a returning node reconciles it
// away.
func (s *Store) ApplyInferred(in Incoming) bool { return s.apply(in, true) }

// ApplyInferredUnchangedSince is ApplyInferred with the staleness sweep's
// precondition: the guess is merged only if the origin has still said nothing
// about this workload since seenAt, the sighting the sweep based its decision on.
//
// The sweep necessarily selects its candidates and then applies them as two
// steps. Without this check, an origin heartbeat landing in between would be
// overwritten by a guess that was already known to be wrong, briefly showing a
// genuinely running workload as failed. Re-reading LastSeen under the same lock
// that the merge takes closes that window.
func (s *Store) ApplyInferredUnchangedSince(in Incoming, seenAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.records[keyOf(in)]
	if !ok {
		// The record was retired between selection and now. A guess about a
		// workload the store no longer holds must not re-create it — applyLocked
		// would take its "new record" branch and resurrect it as a synthesized
		// terminal, which then reaches clients and the scheduler.
		return false
	}
	if !cur.LastSeen.Equal(seenAt) {
		return false
	}
	return s.applyLocked(in, true)
}

// apply is the provenance-aware merge. The rule: provenance first — an
// authoritative event overrides any inferred guess (this is what lets a
// returning node un-stick a peer's wrongly-inferred "failed"), and an inferred
// guess may only mark an authoritative *non-terminal* record failed (never
// override the origin's terminal or otherwise rewrite its truth). Within the
// same provenance: newer generation (createdAt) replaces, older is rejected, and
// within a generation higher state rank wins, a lower rank is rejected, and an
// equal rank is applied only on a meaningful delta (today: a changed
// scheduledOn, e.g. a failover re-point).
func (s *Store) apply(in Incoming, inferred bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(in, inferred)
}

// applyLocked is apply's body. Caller holds s.mu.
func (s *Store) applyLocked(in Incoming, inferred bool) bool {
	if in.ID == "" || in.Origin == "" {
		return false
	}

	key := keyOf(in)
	cur, ok := s.records[key]
	if !ok {
		s.putLocked(key, in, inferred)
		return true
	}

	// An authoritative event for a current generation is proof the origin still
	// holds an opinion about this workload, whether or not the merge below
	// changes anything. Record the sighting first, so the origin's heartbeat
	// re-assertion (a no-op merge) is still evidence of liveness and only a
	// genuinely silent origin lets a non-terminal record go stale. A stale
	// generation is not a sighting, and neither is a local inferred guess.
	if !inferred && in.CreatedAt >= cur.CreatedAt {
		cur.LastSeen = s.now()
		s.records[key] = cur
	}

	// Generation gate first: a newer generation replaces, an older one is
	// rejected — regardless of provenance, so a stale event can never win.
	// (With runId in the key one identity has a single createdAt, so this is
	// normally a tie; it stays as a defensive guard.)
	switch {
	case in.CreatedAt > cur.CreatedAt:
		s.putLocked(key, in, inferred)
		return true
	case in.CreatedAt < cur.CreatedAt:
		return false
	}

	// Same generation: provenance takes precedence over rank. An authoritative
	// event overrides any inferred guess (un-sticks a wrongly-inferred failed),
	// and an inferred guess may only fail an authoritative non-terminal record.
	if !inferred && cur.Inferred {
		s.putLocked(key, in, false)
		return true
	}
	if inferred && !cur.Inferred {
		if cur.Terminal {
			return false
		}
		s.putLocked(key, in, true)
		return true
	}

	// Same generation + provenance: order by state rank.
	switch ri, rc := rank(in.State), rank(cur.State); {
	case ri > rc:
		s.putLocked(key, in, inferred)
		return true
	case ri < rc:
		return false
	default:
		if in.ScheduledOn != cur.ScheduledOn {
			s.putLocked(key, in, inferred)
			return true
		}
		return false
	}
}

// Remove drops any workload matching (origin, id) — a workloads:remove /
// retirement. The removal wire carries only workloadId + originatedFrom (no
// engine/runId), so it targets by that pair and drops every generation/engine
// sharing it. Reports whether anything was removed.
func (s *Store) Remove(origin, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := false
	for k, rec := range s.records {
		if rec.Origin != origin || rec.ID != id {
			continue
		}
		delete(s.records, k)
		if rec.Terminal && !rec.Inferred {
			s.dirty = true
		}
		removed = true
	}
	return removed
}

// Get returns a copy of a record matching (origin, id), if present. The full
// key also includes engine + runId; this convenience matches on the (origin,id)
// pair (used by tests, which use unique ids) and returns the first match.
func (s *Store) Get(origin, id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if r.Origin == origin && r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// Len returns the number of records currently held.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// Snapshot returns the full-fidelity Info of every record, ordered by
// (createdAt, origin, id) for a stable, deterministic baseline (backs
// workloads:get-initial).
func (s *Store) Snapshot() []json.RawMessage {
	s.mu.Lock()
	recs := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		recs = append(recs, r)
	}
	s.mu.Unlock()

	return snapshotRecords(recs)
}

// ActiveSnapshot returns the full-fidelity Info of every non-terminal record in
// the same deterministic order as Snapshot. It seeds a restarted scheduler
// without replaying historic completed/failed work.
func (s *Store) ActiveSnapshot() []json.RawMessage {
	s.mu.Lock()
	recs := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		if !r.Terminal {
			recs = append(recs, r)
		}
	}
	s.mu.Unlock()

	return snapshotRecords(recs)
}

func snapshotRecords(recs []Record) []json.RawMessage {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].CreatedAt != recs[j].CreatedAt {
			return recs[i].CreatedAt < recs[j].CreatedAt
		}
		if recs[i].Origin != recs[j].Origin {
			return recs[i].Origin < recs[j].Origin
		}
		return recs[i].ID < recs[j].ID
	})

	out := make([]json.RawMessage, len(recs))
	for i, r := range recs {
		out[i] = r.Info
	}
	return out
}

// ActiveForNode returns copies of the non-terminal records whose origin or
// executor is node — the set a node-loss sweep must transition to failed when
// node drops out of discovery.
func (s *Store) ActiveForNode(node string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.records {
		if r.Terminal {
			continue
		}
		if r.Origin == node || r.ScheduledOn == node {
			out = append(out, r)
		}
	}
	return out
}

// StaleForeign returns copies of the non-terminal records whose origin is not
// selfNode and which that origin has not asserted for at least staleAfter.
//
// It is the absence-based half of workload convergence. Every other mechanism is
// push-only: the origin sends transitions, retains a terminal briefly, and a
// peer that misses one has no way to learn it happened. The origin's heartbeat
// does, however, re-assert each of its still-active workloads indefinitely, so
// silence about a workload we believe is running is itself information — either
// the origin finished it and we lost every copy of the terminal, or the origin
// is no longer running at all. Both mean the record must not keep displaying as
// in-flight forever.
//
// Local-origin records are skipped, and only those: this node's own proxies are
// the authority for them and never re-assert into this store, so their silence
// carries no information. Records this node merely EXECUTES are NOT skipped, even
// though failing one feels like contradicting our own engine. Lifecycle events
// are produced by the originating proxy, never by the destination engine, so an
// executing node holds no independent signal about the workload — excluding it
// would not preserve truth, it would just guarantee the record is never cleared
// on the one node that cannot learn otherwise.
//
// A candidate is also skipped when another non-terminal record shares its
// (Origin, ID) pair. That pair is the coarser identity the desktop and the
// removal wire key on, so a synthesized terminal for one generation would be
// applied to whichever generation currently occupies that key — potentially a
// live job. Suppressing the ambiguous case means two colliding generations that
// both go silent are never swept; that is the safe direction, and the durable fix
// is to carry engine and runId on the client contract (see open-issues N93).
//
// The caller applies the result as INFERRED, so the origin's next authoritative
// event reconciles it back if the guess was wrong.
func (s *Store) StaleForeign(selfNode string, staleAfter time.Duration) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	// How many live generations share each client-visible (Origin, ID) key.
	liveByClientKey := make(map[[2]string]int, len(s.records))
	for _, r := range s.records {
		if !r.Terminal && r.Origin != "" {
			liveByClientKey[[2]string{r.Origin, r.ID}]++
		}
	}

	var out []Record
	for _, r := range s.records {
		if r.Terminal || r.Origin == "" {
			continue
		}
		if r.Origin == selfNode {
			continue
		}
		if liveByClientKey[[2]string{r.Origin, r.ID}] > 1 {
			continue
		}
		// Elapsed time, not a wall-clock cutoff: now.Sub uses the monotonic
		// readings both values carry, so a clock step cannot make every record
		// stale at once. A zero LastSeen (which recordFrom and putLocked never
		// produce) reads as ancient and is swept, which is the safe direction —
		// the alternative, exempting it, would hide a record forever.
		if now.Sub(r.LastSeen) < staleAfter {
			continue
		}
		out = append(out, r)
	}
	return out
}

// ReplayForNode returns copies of the records a (re)started workload-manager
// should re-assert for node: every non-terminal local-origin record, plus
// authoritative terminal local-origin records whose last update is within
// terminalWindowMs of now. Origin-only (never a peer's — the origin is the
// single writer) and never an inferred guess. Including recent terminals keeps
// the manager's two-interval terminal re-sync window alive across a restart.
func (s *Store) ReplayForNode(node string, terminalWindowMs int64) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Epoch ms here, not elapsed time: the comparison is against completionOrder,
	// which comes from the workload's own wire timestamps.
	cutoff := s.now().UnixMilli() - terminalWindowMs
	var out []Record
	for _, r := range s.records {
		if r.Origin != node {
			continue
		}
		// Terminal freshness derives from the workload's completion time
		// (completionOrder: completedAt when known, persisted in Info and
		// restored by Load), NOT the store clock. Load stamps LastUpdated=now on
		// every restored record, so keying the window on LastUpdated would treat
		// the entire persisted history (up to the 7-day / 10k cap) as just-
		// finished and rebroadcast it all on the first post-restart respawn.
		if r.Terminal && (r.Inferred || completionOrder(r) < cutoff) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// putLocked writes the record for key with the given provenance and marks the
// store dirty when the *persistable* set changed — i.e. an authoritative
// terminal was written, or one was replaced. Only authoritative terminals are
// persisted, so an inferred record never dirties the disk snapshot. Caller
// holds s.mu.
func (s *Store) putLocked(key Key, in Incoming, inferred bool) {
	prevPersistable := false
	var prevLastSeen time.Time
	if prev, ok := s.records[key]; ok {
		prevPersistable = prev.Terminal && !prev.Inferred
		prevLastSeen = prev.LastSeen
	}
	rec := recordFrom(in, s.now())
	rec.Inferred = inferred
	// LastSeen tracks the origin's own assertions, so a local inferred guess
	// keeps whatever the origin last told us rather than posing as a sighting.
	if inferred && !prevLastSeen.IsZero() {
		rec.LastSeen = prevLastSeen
	}
	s.records[key] = rec
	if (rec.Terminal && !rec.Inferred) || prevPersistable {
		s.dirty = true
	}
}

// keyOf builds the full identity key for an incoming event.
func keyOf(in Incoming) Key {
	return Key{Origin: in.Origin, Engine: in.Engine, RunID: in.RunID, ID: in.ID}
}

func recordFrom(in Incoming, now time.Time) Record {
	return Record{
		Origin:      in.Origin,
		ID:          in.ID,
		State:       in.State,
		CreatedAt:   in.CreatedAt,
		CompletedAt: in.CompletedAt,
		ScheduledOn: in.ScheduledOn,
		Terminal:    isTerminal(in.State),
		Info:        in.Info,
		LastUpdated: now.UnixMilli(),
		LastSeen:    now,
	}
}

// isTerminal reports whether a state ends a workload's life.
func isTerminal(state string) bool {
	return state == "completed" || state == "failed"
}

// rank orders lifecycle states so state only moves forward within a
// generation: queued < running < {completed, failed}. "initializing" and any
// unknown value rank below queued (they're never transmitted; see spec §4).
func rank(state string) int {
	switch state {
	case "queued":
		return 1
	case "running":
		return 2
	case "completed", "failed":
		return 3
	default:
		return 0
	}
}
