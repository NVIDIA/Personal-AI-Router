// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func node(id string, txt ...string) Node {
	return Node{ID: id, Host: id + ".local.", Port: 14318, Addresses: []string{"192.168.1.10"}, TXT: txt}
}

func seenSet(ns ...Node) map[string]Node {
	m := make(map[string]Node, len(ns))
	for _, n := range ns {
		m[n.ID] = n
	}
	return m
}

// eventsByType tallies events for concise assertions.
func eventsByType(evs []Event) map[string]int {
	m := map[string]int{}
	for _, e := range evs {
		m[e.Type]++
	}
	return m
}

func TestReconcileDiscoveredUpdatedRemoved(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(3))

	if got := eventsByType(b.reconcile(seenSet(node("a")))); got[Discovered] != 1 {
		t.Fatalf("first scan: got %v, want 1 discovered", got)
	}
	// Same node, no change → no event.
	if evs := b.reconcile(seenSet(node("a"))); len(evs) != 0 {
		t.Fatalf("unchanged scan emitted %v", evs)
	}
	// Changed TXT → updated.
	if got := eventsByType(b.reconcile(seenSet(node("a", "v=2")))); got[Updated] != 1 {
		t.Fatalf("changed scan: got %v, want 1 updated", got)
	}
	// Absent for < threshold: no removal yet.
	for i := 0; i < 2; i++ {
		if evs := b.reconcile(seenSet()); len(evs) != 0 {
			t.Fatalf("miss %d emitted %v, want none before threshold", i+1, evs)
		}
	}
	// Third consecutive miss reaches the threshold → removed.
	if got := eventsByType(b.reconcile(seenSet())); got[Removed] != 1 {
		t.Fatalf("threshold miss: got %v, want 1 removed", got)
	}
	if len(b.Nodes()) != 0 {
		t.Fatalf("node not evicted: %v", b.Nodes())
	}
}

// TestDefaultMissThresholdOutlastsASaturatedNode pins the eviction window a
// browser gets when it does not choose one. A node saturated by its own
// inference load stops answering mDNS for as long as the load lasts, and the
// window that shipped (three scans, 15s) was short enough to read that as a
// departure — so a working node was evicted mid-request. Nothing else in the
// package asserts the default, and the whole point of the constant is the
// elapsed time it buys.
func TestDefaultMissThresholdOutlastsASaturatedNode(t *testing.T) {
	b := New("_nvpair-test._tcp", "local")

	if window := time.Duration(b.opt.missThreshold) * b.opt.interval; window < time.Minute {
		t.Fatalf("default eviction window is %s; too short to outlast a load burst", window)
	}

	b.reconcile(seenSet(node("a")))
	for i := 1; i < missThresholdDefault; i++ {
		if evs := b.reconcile(seenSet()); len(evs) != 0 {
			t.Fatalf("miss %d of %d emitted %v; the node must be held until the threshold", i, missThresholdDefault, evs)
		}
	}
	if got := eventsByType(b.reconcile(seenSet())); got[Removed] != 1 {
		t.Fatalf("miss %d: got %v, want 1 removed", missThresholdDefault, got)
	}
}

// A longer window must not mean a blind one. Probing and giving up are separate
// decisions: the probe starts a quarter of the way in and runs every scan, so the
// minute is spent gathering evidence rather than waiting, and eviction rests on a
// run of failed probes instead of the one that lands on the deadline.
func TestLivenessProbeRunsThroughoutTheWindowNotJustAtTheEnd(t *testing.T) {
	var probes atomic.Int32
	b := New("_nvpair-test._tcp", "local", WithLivenessProbe(func(Node) bool {
		probes.Add(1)
		return false
	}))
	b.reconcile(seenSet(node("a")))

	for i := 1; i < probeAfterMisses; i++ {
		b.reconcile(seenSet())
	}
	if got := probes.Load(); got != 0 {
		t.Fatalf("probe ran %d times before probeAfterMisses; want 0", got)
	}

	// From probeAfterMisses onward every scan asks again, and no failed answer may
	// evict before the threshold.
	for i := probeAfterMisses; i < missThresholdDefault; i++ {
		if evs := b.reconcile(seenSet()); len(evs) != 0 {
			t.Fatalf("miss %d emitted %v; a failed probe must not evict before the threshold", i, evs)
		}
	}
	asked := probes.Load()
	if want := int32(missThresholdDefault - probeAfterMisses); asked != want {
		t.Fatalf("probe ran %d times across the window; want %d (once per scan from probeAfterMisses)", asked, want)
	}
	if got := eventsByType(b.reconcile(seenSet()))[Removed]; got != 1 {
		t.Fatalf("node not evicted at the threshold after %d failed probes (removed=%d)", asked, got)
	}
}

// The reason for probing early is rescue: one answer clears the miss counter, so
// a peer whose multicast is being dropped while it remains perfectly reachable
// never reaches the threshold at all.
func TestAProbeThatAnswersMidWindowFullyRecoversTheNode(t *testing.T) {
	alive := false
	b := New("_nvpair-test._tcp", "local", WithLivenessProbe(func(Node) bool { return alive }))
	b.reconcile(seenSet(node("a")))

	for i := 1; i < missThresholdDefault; i++ {
		b.reconcile(seenSet())
	}
	// One answer, one scan short of eviction.
	alive = true
	if evs := b.reconcile(seenSet()); len(evs) != 0 {
		t.Fatalf("a node that answered its probe emitted %v", evs)
	}
	if b.misses["a"] != 0 {
		t.Fatalf("miss counter = %d after a successful probe; want 0", b.misses["a"])
	}
	// And the full window is available again from scratch.
	alive = false
	for i := 1; i < missThresholdDefault; i++ {
		if evs := b.reconcile(seenSet()); len(evs) != 0 {
			t.Fatalf("miss %d after recovery emitted %v; the window must restart", i, evs)
		}
	}
}

// A browser that picks a threshold shorter than probeAfterMisses must still get
// its probe: one that first ran after the point of no return could never rescue
// anything, and the node would be evicted without ever being asked.
func TestAShortThresholdStillProbesBeforeEvicting(t *testing.T) {
	var probes atomic.Int32
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(1), WithLivenessProbe(func(Node) bool {
		probes.Add(1)
		return true
	}))
	b.reconcile(seenSet(node("a")))

	if evs := b.reconcile(seenSet()); len(evs) != 0 {
		t.Fatalf("a reachable node was evicted at a threshold of 1: %v", evs)
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("probe ran %d times; want 1", got)
	}
}

// TestEmptyScanDoesNotEvictTheWholeFleet is the regression that motivated the
// guard. A machine saturated by its own inference load could not drain its
// multicast socket inside scanTimeout, so its scans came back completely empty
// and every known node took a miss on the same pass — including idle peers doing
// no work at all. Three such passes evicted the entire cluster in one reconcile.
func TestEmptyScanDoesNotEvictTheWholeFleet(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a"), node("b"), node("c")))

	for i := 1; i <= emptyScanGrace; i++ {
		if evs := b.reconcile(seenSet()); len(evs) != 0 {
			t.Fatalf("empty scan %d emitted %v; an empty scan is evidence about this browser, not the fleet", i, evs)
		}
	}
	if got := len(b.Nodes()); got != 3 {
		t.Fatalf("%d of 3 nodes survived %d empty scans; want all 3", got, emptyScanGrace)
	}
	for _, k := range []string{"a", "b", "c"} {
		if b.misses[k] != 0 {
			t.Fatalf("node %q took %d misses from empty scans; want 0", k, b.misses[k])
		}
	}
}

// The guard is a grace, not a veto: a whole fleet can genuinely go — a switch
// losing power, the last peers shutting down together — and those records still
// have to age out.
func TestSustainedEmptyScansStillEvictEventually(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a"), node("b")))

	removed := 0
	// Past the grace, empty scans count normally, so the nodes need the full miss
	// threshold on top before they go.
	for i := 0; i < emptyScanGrace+missThresholdDefault+1 && removed < 2; i++ {
		removed += eventsByType(b.reconcile(seenSet()))[Removed]
	}
	if removed != 2 {
		t.Fatalf("%d of 2 nodes aged out after %d empty scans; want both",
			removed, emptyScanGrace+missThresholdDefault+1)
	}
}

// The suppression is justified by several independent machines going quiet at
// once, so it does not apply when there is only one node to lose. Losing a lone
// peer looks identical whether it left or we stopped listening, and delaying that
// eviction would buy nothing — the miss threshold and the liveness probe already
// cover it.
func TestALoneNodeIsNotShieldedByTheEmptyScanGuard(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(2), WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a")))

	b.reconcile(seenSet())
	if got := eventsByType(b.reconcile(seenSet()))[Removed]; got != 1 {
		t.Fatalf("a single known node was not evicted at its threshold (removed=%d)", got)
	}
}

// One node answering proves the receive path works, so the run of excuses ends
// there — otherwise a browser that alternated one good scan with six empty ones
// would never evict anything.
func TestAnySeenNodeResetsTheEmptyRun(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a"), node("b")))

	for i := 0; i < emptyScanGrace; i++ {
		b.reconcile(seenSet())
	}
	if b.emptyScans != emptyScanGrace {
		t.Fatalf("empty run = %d, want %d", b.emptyScans, emptyScanGrace)
	}
	// "b" alone answers: partial, but proof the socket is being drained.
	b.reconcile(seenSet(node("b")))
	if b.emptyScans != 0 {
		t.Fatalf("empty run = %d after a node answered; want 0", b.emptyScans)
	}
}

// The guard must not soften the ordinary case. A scan that returns some nodes and
// not others says something real about the ones that are missing.
func TestPartialScanStillPenalizesTheMissingNode(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(2), WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a"), node("gone")))

	b.reconcile(seenSet(node("a")))
	if got := eventsByType(b.reconcile(seenSet(node("a"))))[Removed]; got != 1 {
		t.Fatalf("a node missing from scans that returned other nodes was not evicted (removed=%d)", got)
	}
}

// A browser that knows about nobody is not failing when it hears nobody, so it
// must not bank excuses it would spend later against nodes it has only just
// discovered.
func TestEmptyScanWithNoKnownNodesBanksNothing(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithLivenessProbe(func(Node) bool { return false }))
	for i := 0; i < emptyScanGrace*2; i++ {
		b.reconcile(seenSet())
	}
	if b.emptyScans != 0 {
		t.Fatalf("empty run = %d with no nodes known; want 0", b.emptyScans)
	}

	// And the excuses it did not bank are not spent on the nodes that arrive next.
	b.reconcile(seenSet(node("a"), node("b")))
	if evs := b.reconcile(seenSet()); len(evs) != 0 {
		t.Fatalf("first empty scan after discovery emitted %v; the grace should start fresh", evs)
	}
	if b.emptyScans != 1 {
		t.Fatalf("empty run = %d after one empty scan; want 1", b.emptyScans)
	}
}

func TestReconcileOrderInsensitive(t *testing.T) {
	b := New("_nvpair-test._tcp", "local")
	a1 := Node{ID: "a", Host: "a.local.", Port: 1, Addresses: []string{"10.0.0.1", "10.0.0.2"}, TXT: []string{"x=1", "y=2"}}
	a2 := Node{ID: "a", Host: "a.local.", Port: 1, Addresses: []string{"10.0.0.2", "10.0.0.1"}, TXT: []string{"y=2", "x=1"}}
	b.reconcile(seenSet(a1))
	if evs := b.reconcile(seenSet(a2)); len(evs) != 0 {
		t.Fatalf("reordered addresses/TXT emitted a spurious event %v", evs)
	}
}

func TestNoEviction(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithNoEviction(), WithMissThreshold(1))
	b.reconcile(seenSet(node("a")))
	for i := 0; i < 5; i++ {
		if evs := b.reconcile(seenSet()); len(evs) != 0 {
			t.Fatalf("no-evict browser emitted %v on miss %d", evs, i+1)
		}
	}
	if len(b.Nodes()) != 1 {
		t.Fatalf("no-evict browser dropped the node: %v", b.Nodes())
	}
}

func TestLivenessProbeRetainsReachable(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(2), WithLivenessProbe(func(Node) bool { return true }))
	b.reconcile(seenSet(node("a")))
	// Two misses cross the threshold, but the probe says the node is still up.
	b.reconcile(seenSet())
	if evs := b.reconcile(seenSet()); len(evs) != 0 {
		t.Fatalf("reachable threshold-missed node was evicted: %v", evs)
	}
	if len(b.Nodes()) != 1 {
		t.Fatalf("reachable node dropped: %v", b.Nodes())
	}
}

func TestLivenessProbeEvictsUnreachable(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(2), WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a")))
	b.reconcile(seenSet())
	if got := eventsByType(b.reconcile(seenSet())); got[Removed] != 1 {
		t.Fatalf("unreachable threshold-missed node not evicted: %v", got)
	}
	if len(b.Nodes()) != 0 {
		t.Fatalf("unreachable node retained: %v", b.Nodes())
	}
}

// TestLivenessProbesOverlap pins the concurrency bound: one probe stuck on an
// unreachable address must not hold up the rest. Each probe blocks until
// probeConcurrency of them are in flight, which can only complete if they run
// together.
func TestLivenessProbesOverlap(t *testing.T) {
	const nodes = probeConcurrency
	release := make(chan struct{})
	var started atomic.Int32
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(1), WithLivenessProbe(func(Node) bool {
		if started.Add(1) == nodes {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			// Serial probes never overlap, so release is never closed and each
			// one waits out this deadline instead of hanging the test.
		}
		return true
	}))

	seed := make([]Node, 0, nodes)
	for i := range nodes {
		seed = append(seed, node(fmt.Sprintf("n%d", i)))
	}
	b.reconcile(seenSet(seed...))

	start := time.Now()
	b.reconcile(seenSet())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("probes did not overlap: %d nodes took %v", nodes, elapsed)
	}
	if len(b.Nodes()) != nodes {
		t.Fatalf("reachable nodes evicted: %d of %d retained", len(b.Nodes()), nodes)
	}
}

func TestKeyFunc(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithKeyFunc(func(n Node) string { return UUIDFromTXT(n.TXT) }))
	if got := b.key(node("host", "uuid=abc")); got != "abc" {
		t.Errorf("key with uuid = %q, want abc", got)
	}
	// Falls back to instance name when the key func returns "".
	if got := b.key(node("host")); got != "host" {
		t.Errorf("key without uuid = %q, want host (fallback)", got)
	}
}

func TestUUIDFromTXT(t *testing.T) {
	if got := UUIDFromTXT([]string{"v=1", "uuid=xyz", "ip=1.2.3.4"}); got != "xyz" {
		t.Errorf("UUIDFromTXT = %q, want xyz", got)
	}
	if got := UUIDFromTXT([]string{"v=1"}); got != "" {
		t.Errorf("UUIDFromTXT with no uuid = %q, want empty", got)
	}
}

func TestSeed(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithKeyFunc(func(n Node) string { return UUIDFromTXT(n.TXT) }))
	b.Seed(node("host-a", "uuid=a"), node("host-b", "uuid=b"))
	if len(b.Nodes()) != 2 {
		t.Fatalf("Seed: got %d nodes, want 2", len(b.Nodes()))
	}
	// A subsequent scan that omits a seeded node must not immediately drop it
	// (miss counting starts fresh); and re-seeding replaces in place.
	b.Seed(node("host-a", "uuid=a", "ip=1.2.3.4"))
	if len(b.Nodes()) != 2 {
		t.Fatalf("re-seed changed node count: %d", len(b.Nodes()))
	}
}

func TestPollReturnsSnapshot(t *testing.T) {
	b := New("_nvpair-test._tcp", "local")
	b.browseFunc = func(context.Context) map[string]Node { return seenSet(node("a"), node("b")) }
	got := b.Poll(context.Background())
	if len(got) != 2 {
		t.Fatalf("Poll returned %d nodes, want 2", len(got))
	}
}

func TestRunEmitsAndCloses(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithInterval(5*time.Millisecond), WithMissThreshold(1))
	// First scan sees "a"; every subsequent scan is empty so it's removed.
	var calls int
	b.browseFunc = func(context.Context) map[string]Node {
		calls++
		if calls == 1 {
			return seenSet(node("a"))
		}
		return seenSet()
	}

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan Event, 16)
	done := make(chan struct{})
	go func() { b.Run(ctx, events); close(done) }()

	got := map[string]int{}
	timeout := time.After(2 * time.Second)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before observing discovered+removed")
			}
			got[e.Type]++
			if got[Discovered] >= 1 && got[Removed] >= 1 {
				cancel()
				<-done // Run must close the channel on return
				return
			}
		case <-timeout:
			cancel()
			t.Fatalf("timed out; events seen: %v", got)
		}
	}
}

// TestSendFailuresNeedARunAndClearOnRecovery: this feeds address selection, so a
// single blip must not move a host's canonical address, and one success must undo
// the suppression immediately.
func TestSendFailuresNeedARunAndClearOnRecovery(t *testing.T) {
	b := New("_x._tcp", "local")
	for range sendFailureThreshold - 1 {
		b.recordSendOutcomes(map[string]bool{"eth0": false})
	}
	if got := b.SendFailures(); got["eth0"] {
		t.Fatalf("eth0 reported failed after %d misses, want a full run of %d first",
			sendFailureThreshold-1, sendFailureThreshold)
	}
	b.recordSendOutcomes(map[string]bool{"eth0": false})
	if got := b.SendFailures(); !got["eth0"] {
		t.Fatalf("eth0 not reported failed after %d consecutive misses", sendFailureThreshold)
	}
	b.recordSendOutcomes(map[string]bool{"eth0": true})
	if got := b.SendFailures(); got["eth0"] {
		t.Fatal("one successful send must clear the suppression outright")
	}
}

// TestSendFailuresForgetAnInterfaceThatIsGone: a VPN adapter that comes and goes
// would otherwise leave a run of failures behind that suppresses an address on an
// interface no longer present, and grow the counter map for the process's life.
func TestSendFailuresForgetAnInterfaceThatIsGone(t *testing.T) {
	b := New("_x._tcp", "local")
	for range sendFailureThreshold {
		b.recordSendOutcomes(map[string]bool{"eth0": false, "tun0": false})
	}
	if got := b.SendFailures(); !got["tun0"] {
		t.Fatalf("tun0 not reported failed after a full run: %v", got)
	}

	// tun0 is gone: the next scan attempts eth0 only.
	b.recordSendOutcomes(map[string]bool{"eth0": false})
	got := b.SendFailures()
	if got["tun0"] {
		t.Error("an interface that is no longer attempted must not stay asserted as failed")
	}
	if !got["eth0"] {
		t.Error("eth0 is still being attempted and still failing; its run must survive")
	}
	b.mu.RLock()
	_, stale := b.sendMisses["tun0"]
	b.mu.RUnlock()
	if stale {
		t.Error("the counter for a departed interface was retained")
	}
}
