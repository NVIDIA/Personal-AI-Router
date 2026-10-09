// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet(node("a"))))[Discovered], "first scan")
	// Same node, no change → no event.
	require.Empty(t, b.reconcile(seenSet(node("a"))), "unchanged scan emitted")
	// Changed TXT → updated.
	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet(node("a", "v=2"))))[Updated], "changed scan")
	// Absent for < threshold: no removal yet.
	for i := 0; i < 2; i++ {
		require.Empty(t, b.reconcile(seenSet()))
	}
	// Third consecutive miss reaches the threshold → removed.
	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet()))[Removed], "threshold miss")
	require.Empty(t, b.Nodes(), "node not evicted")
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

	assert.GreaterOrEqual(t, time.Duration(b.opt.missThreshold)*b.opt.interval, time.Minute, "default eviction window is")

	b.reconcile(seenSet(node("a")))
	for i := 1; i < missThresholdDefault; i++ {
		require.Empty(t, b.reconcile(seenSet()))
	}
	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet()))[Removed])
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
	assert.Equal(t, int32(0), probes.Load(), "probe ran")

	// From probeAfterMisses onward every scan asks again, and no failed answer may
	// evict before the threshold.
	for i := probeAfterMisses; i < missThresholdDefault; i++ {
		require.Empty(t, b.reconcile(seenSet()))
	}
	assert.Equal(t, int32(missThresholdDefault-probeAfterMisses), probes.Load(), "probe count")
	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet()))[Removed], "node not evicted at the threshold")
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
	require.Empty(t, b.reconcile(seenSet()), "a node that answered its probe emitted")
	assert.Equal(t, 0, b.misses["a"], "miss counter")
	// And the full window is available again from scratch.
	alive = false
	for i := 1; i < missThresholdDefault; i++ {
		require.Empty(t, b.reconcile(seenSet()))
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

	require.Empty(t, b.reconcile(seenSet()), "a reachable node was evicted at a threshold of 1")
	assert.Equal(t, int32(1), probes.Load(), "probe ran")
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
		require.Empty(t, b.reconcile(seenSet()), "empty scan")
	}
	assert.Len(t, b.Nodes(), 3)
	assert.Equal(t, 0, b.misses["a"])
	assert.Equal(t, 0, b.misses["b"])
	assert.Equal(t, 0, b.misses["c"])
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
	assert.Equal(t, 2, removed)
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
	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet()))[Removed], "a single known node was not evicted at its threshold")
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
	assert.Equal(t, emptyScanGrace, b.emptyScans, "empty run")
	// "b" alone answers: partial, but proof the socket is being drained.
	b.reconcile(seenSet(node("b")))
	assert.Equal(t, 0, b.emptyScans, "empty run")
}

// The guard must not soften the ordinary case. A scan that returns some nodes and
// not others says something real about the ones that are missing.
func TestPartialScanStillPenalizesTheMissingNode(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(2), WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a"), node("gone")))

	b.reconcile(seenSet(node("a")))
	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet(node("a"))))[Removed], "a node missing from scans that returned other nodes was not evicted")
}

// A browser that knows about nobody is not failing when it hears nobody, so it
// must not bank excuses it would spend later against nodes it has only just
// discovered.
func TestEmptyScanWithNoKnownNodesBanksNothing(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithLivenessProbe(func(Node) bool { return false }))
	for i := 0; i < emptyScanGrace*2; i++ {
		b.reconcile(seenSet())
	}
	assert.Equal(t, 0, b.emptyScans, "empty run")

	// And the excuses it did not bank are not spent on the nodes that arrive next.
	b.reconcile(seenSet(node("a"), node("b")))
	require.Empty(t, b.reconcile(seenSet()), "first empty scan after discovery emitted")
	assert.Equal(t, 1, b.emptyScans, "empty run")
}

func TestReconcileOrderInsensitive(t *testing.T) {
	b := New("_nvpair-test._tcp", "local")
	a1 := Node{ID: "a", Host: "a.local.", Port: 1, Addresses: []string{"10.0.0.1", "10.0.0.2"}, TXT: []string{"x=1", "y=2"}}
	a2 := Node{ID: "a", Host: "a.local.", Port: 1, Addresses: []string{"10.0.0.2", "10.0.0.1"}, TXT: []string{"y=2", "x=1"}}
	b.reconcile(seenSet(a1))
	require.Empty(t, b.reconcile(seenSet(a2)), "reordered addresses/TXT emitted a spurious event")
}

func TestNoEviction(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithNoEviction(), WithMissThreshold(1))
	b.reconcile(seenSet(node("a")))
	for i := 0; i < 5; i++ {
		require.Empty(t, b.reconcile(seenSet()), "no-evict browser emitted")
	}
	require.Len(t, b.Nodes(), 1, "no-evict browser dropped the node")
}

func TestLivenessProbeRetainsReachable(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(2), WithLivenessProbe(func(Node) bool { return true }))
	b.reconcile(seenSet(node("a")))
	// Two misses cross the threshold, but the probe says the node is still up.
	b.reconcile(seenSet())
	require.Empty(t, b.reconcile(seenSet()), "reachable threshold-missed node was evicted")
	require.Len(t, b.Nodes(), 1, "reachable node dropped")
}

func TestLivenessProbeEvictsUnreachable(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithMissThreshold(2), WithLivenessProbe(func(Node) bool { return false }))
	b.reconcile(seenSet(node("a")))
	b.reconcile(seenSet())
	assert.Equal(t, 1, eventsByType(b.reconcile(seenSet()))[Removed], "unreachable threshold-missed node not evicted")
	require.Empty(t, b.Nodes(), "unreachable node retained")
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
	assert.LessOrEqual(t, time.Since(start), time.Second, "probes did not overlap")
	require.Len(t, b.Nodes(), nodes, "reachable nodes evicted")
}

func TestKeyFunc(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithKeyFunc(func(n Node) string { return UUIDFromTXT(n.TXT) }))
	assert.Equal(t, "abc", b.key(node("host", "uuid=abc")), "key with uuid")
	// Falls back to instance name when the key func returns "".
	assert.Equal(t, "host", b.key(node("host")), "key without uuid")
}

func TestUUIDFromTXT(t *testing.T) {
	assert.Equal(t, "xyz", UUIDFromTXT([]string{"v=1", "uuid=xyz", "ip=1.2.3.4"}))
	assert.Equal(t, "", UUIDFromTXT([]string{"v=1"}), "UUIDFromTXT with no uuid")
}

func TestSeed(t *testing.T) {
	b := New("_nvpair-test._tcp", "local", WithKeyFunc(func(n Node) string { return UUIDFromTXT(n.TXT) }))
	b.Seed(node("host-a", "uuid=a"), node("host-b", "uuid=b"))
	require.Len(t, b.Nodes(), 2)
	// A subsequent scan that omits a seeded node must not immediately drop it
	// (miss counting starts fresh); and re-seeding replaces in place.
	b.Seed(node("host-a", "uuid=a", "ip=1.2.3.4"))
	require.Len(t, b.Nodes(), 2, "re-seed changed node count")
}

func TestPollReturnsSnapshot(t *testing.T) {
	b := New("_nvpair-test._tcp", "local")
	b.browseFunc = func(context.Context) map[string]Node { return seenSet(node("a"), node("b")) }
	require.Len(t, b.Poll(context.Background()), 2)
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
			require.True(t, ok, "events channel closed before observing discovered+removed")
			got[e.Type]++
			if got[Discovered] >= 1 && got[Removed] >= 1 {
				cancel()
				<-done // Run must close the channel on return
				return
			}
		case <-timeout:
			cancel()
			require.FailNow(t, "timed out", "events seen: %v", got)
		}
	}
}

func TestSendMulticastQueryOnInterfaceUsesFirstIPv4AndMDNSTarget(t *testing.T) {
	ifi := &net.Interface{Index: 7, Name: "eth0"}
	wantSource := net.IPv4(192, 0, 2, 10)
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("2001:db8::10")},
		&net.IPNet{IP: wantSource},
		&net.IPNet{IP: net.IPv4(198, 51, 100, 20)},
	}
	payload := []byte("PTR query")

	var gotPayload []byte
	var gotInterface *net.Interface
	var gotSource net.IP
	var gotTarget *net.UDPAddr
	source, err := sendMulticastQueryOnInterface(
		payload,
		ifi,
		addrs,
		func(buf []byte, sentIfi *net.Interface, src net.IP, target *net.UDPAddr) error {
			gotPayload = append([]byte(nil), buf...)
			gotInterface = sentIfi
			gotSource = append(net.IP(nil), src...)
			gotTarget = target
			return nil
		},
	)
	require.NoError(t, err, "sendMulticastQueryOnInterface")
	assert.Equal(t, wantSource.String(), source.String(), "source")
	assert.Equal(t, wantSource.String(), gotSource.String(), "source")
	assert.Same(t, ifi, gotInterface)
	assert.Equal(t, string(payload), string(gotPayload))
	require.NotNil(t, gotTarget)
	assert.Equal(t, "224.0.0.251", gotTarget.IP.String(), "target")
	assert.Equal(t, 5353, gotTarget.Port)
}

func TestSendMulticastQueryOnInterfaceReturnsSenderFailure(t *testing.T) {
	wantErr := errors.New("send refused")
	wantSource := net.IPv4(192, 0, 2, 10)
	source, err := sendMulticastQueryOnInterface(
		[]byte("PTR query"),
		&net.Interface{Index: 7, Name: "eth0"},
		[]net.Addr{&net.IPNet{IP: wantSource}},
		func([]byte, *net.Interface, net.IP, *net.UDPAddr) error {
			return wantErr
		},
	)
	assert.Equal(t, wantSource.String(), source.String(), "source")
	require.ErrorIs(t, err, wantErr, "error")
}

func TestSendMulticastQueryOnInterfaceSkipsInterfacesWithoutIPv4(t *testing.T) {
	called := false
	source, err := sendMulticastQueryOnInterface(
		[]byte("PTR query"),
		&net.Interface{Index: 7, Name: "eth0"},
		[]net.Addr{&net.IPNet{IP: net.ParseIP("2001:db8::10")}},
		func([]byte, *net.Interface, net.IP, *net.UDPAddr) error {
			called = true
			return nil
		},
	)
	require.NoError(t, err, "sendMulticastQueryOnInterface")
	assert.Empty(t, source)
	require.False(t, called, "sender called without an IPv4 address")
}

// TestSendFailuresNeedARunAndClearOnRecovery: this feeds address selection, so a
// single blip must not move a host's canonical address, and one success must undo
// the suppression immediately.
func TestSendFailuresNeedARunAndClearOnRecovery(t *testing.T) {
	b := New("_x._tcp", "local")
	for range sendFailureThreshold - 1 {
		b.recordSendOutcomes(map[string]bool{"eth0": false})
	}
	assert.False(t, b.SendFailures()["eth0"], "eth0 reported failed before the consecutive-failure threshold")
	b.recordSendOutcomes(map[string]bool{"eth0": false})
	assert.True(t, b.SendFailures()["eth0"], "eth0 not reported failed after the consecutive-failure threshold")
	b.recordSendOutcomes(map[string]bool{"eth0": true})
	assert.False(t, b.SendFailures()["eth0"], "one successful send must clear the suppression outright")
}

// TestSendFailuresForgetAnInterfaceThatIsGone: a VPN adapter that comes and goes
// would otherwise leave a run of failures behind that suppresses an address on an
// interface no longer present, and grow the counter map for the process's life.
func TestSendFailuresForgetAnInterfaceThatIsGone(t *testing.T) {
	b := New("_x._tcp", "local")
	for range sendFailureThreshold {
		b.recordSendOutcomes(map[string]bool{"eth0": false, "tun0": false})
	}
	assert.True(t, b.SendFailures()["tun0"], "tun0 not reported failed after a full run")

	// tun0 is gone: the next scan attempts eth0 only.
	b.recordSendOutcomes(map[string]bool{"eth0": false})
	got := b.SendFailures()
	assert.False(t, got["tun0"], "an interface that is no longer attempted must not stay asserted as failed")
	assert.True(t, got["eth0"], "eth0 is still being attempted and still failing; its run must survive")
	b.mu.RLock()
	assert.NotContains(t, b.sendMisses, "tun0", "the counter for a departed interface was retained")
	b.mu.RUnlock()
}
