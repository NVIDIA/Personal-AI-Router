// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reach

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNet describes which addresses accept, and records every attempt, so tests
// can assert on the connections a chooser did and did not make.
type fakeNet struct {
	mu       sync.Mutex
	accept   map[string]bool
	attempts []string
	dials    atomic.Int64
}

func newFakeNet(accepting ...string) *fakeNet {
	f := &fakeNet{accept: make(map[string]bool)}
	for _, a := range accepting {
		f.accept[a] = true
	}
	return f
}

func (f *fakeNet) dial(_, address string, _ time.Duration) (net.Conn, error) {
	f.dials.Add(1)
	f.mu.Lock()
	f.attempts = append(f.attempts, address)
	ok := f.accept[address]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	// A closed pipe end is a net.Conn the caller can Close, with no socket.
	local, remote := net.Pipe()
	_ = remote.Close()
	return local, nil
}

func (f *fakeNet) attemptedAddresses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attempts...)
}

func newTestChooser(f *fakeNet) *Chooser {
	return &Chooser{
		timeout:    10 * time.Millisecond,
		dial:       f.dial,
		cache:      make(map[string]choice),
		probing:    make(map[string]bool),
		generation: make(map[string]uint64),
	}
}

// choose is ChooseWithin with no deadline: the blocking form, as a periodic or
// user-initiated caller uses it.
func choose(c *Chooser, key string, candidates []string) string {
	return c.ChooseWithin(context.Background(), key, candidates)
}

// preferred is Prefer plus a wait for the confirmation it starts, so a test can
// assert on where later requests go without racing the background probe.
func preferred(t *testing.T, c *Chooser, key string, candidates []string) string {
	t.Helper()
	address := c.Prefer(key, candidates)
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		running := c.probing[key]
		c.mu.Unlock()
		if !running {
			return address
		}
		if time.Now().After(deadline) {
			t.Fatal("the background confirmation never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

// expireCooldown ages key's entry past UnconfirmedCooldown, so a test can observe
// what happens when it lapses without waiting out the real duration.
func (c *Chooser) expireCooldown(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cache[key]; ok {
		e.at = e.at.Add(-UnconfirmedCooldown - time.Second)
		c.cache[key] = e
	}
}

// TestChooseFailsOverToASecondAddress is the reported defect: a peer publishing an
// address this host cannot reach, alongside one it can, must be dialed at the one
// it can. Before this, the unreachable address was the only one tried.
func TestChooseFailsOverToASecondAddress(t *testing.T) {
	f := newFakeNet("10.172.55.129:14321")
	c := newTestChooser(f)
	got := choose(c, "peer", []string{"192.168.240.1:14321", "10.172.55.129:14321"})
	if got != "10.172.55.129:14321" {
		t.Fatalf("Choose = %q, want the address that accepted", got)
	}
	attempts := f.attemptedAddresses()
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v, want both published addresses probed", attempts)
	}
}

// TestChoosePrefersTheBestRankedAcceptingAddress: with the candidates probed
// together, the answer still has to be the node's own ranking rather than whichever
// handshake happened to land first. Otherwise two equally reachable addresses would
// trade places on timing noise, and every swap costs a reconnect for nothing.
func TestChoosePrefersTheBestRankedAcceptingAddress(t *testing.T) {
	f := newFakeNet("10.0.0.1:14321", "10.0.0.2:14321")
	c := newTestChooser(f)
	if got := choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"}); got != "10.0.0.1:14321" {
		t.Fatalf("Choose = %q, want the best-ranked accepting address", got)
	}
}

// TestFirstPrefersRankOverArrivalOrder states that directly: the better-ranked
// address wins even when a lower-ranked one answers first.
func TestFirstPrefersRankOverArrivalOrder(t *testing.T) {
	dial := func(_, address string, _ time.Duration) (net.Conn, error) {
		if address == "10.0.0.1:1" {
			time.Sleep(40 * time.Millisecond)
		}
		local, remote := net.Pipe()
		_ = remote.Close()
		return local, nil
	}
	got, ok := first(dial, []string{"10.0.0.1:1", "10.0.0.2:1"}, time.Second, time.Time{})
	if !ok || got != "10.0.0.1:1" {
		t.Fatalf("first = %q,%v, want the top-ranked address despite answering later", got, ok)
	}
}

// TestFirstPaysOneTimeoutForTheWholeList: the failure that costs real time is an
// address that neither answers nor refuses, because a dropped SYN costs the full
// timeout. Probing in sequence paid that per address, so a node whose leading
// addresses blackhole could burn a caller's whole budget before reaching one that
// works.
func TestFirstPaysOneTimeoutForTheWholeList(t *testing.T) {
	const timeout = 150 * time.Millisecond
	dial := func(_, address string, timeout time.Duration) (net.Conn, error) {
		if address != "10.0.0.4:1" {
			time.Sleep(timeout)
			return nil, errors.New("timed out")
		}
		local, remote := net.Pipe()
		_ = remote.Close()
		return local, nil
	}
	candidates := []string{"10.0.0.1:1", "10.0.0.2:1", "10.0.0.3:1", "10.0.0.4:1"}

	start := time.Now()
	got, ok := first(dial, candidates, timeout, time.Time{})
	elapsed := time.Since(start)

	if !ok || got != "10.0.0.4:1" {
		t.Fatalf("first = %q,%v, want the address that accepted", got, ok)
	}
	// Sequentially this would be three timeouts before the fourth address was even
	// attempted.
	if elapsed > 2*timeout {
		t.Errorf("probing four addresses took %v with a %v timeout; they are not being probed together",
			elapsed, timeout)
	}
}

// TestChooseCachesTheConfirmedAddress: a repeated dial must cost nothing. Without
// this, every request would re-walk the candidate list.
func TestChooseCachesTheConfirmedAddress(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321")
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321"}
	first := choose(c, "peer", candidates)
	before := f.dials.Load()
	for range 5 {
		if got := choose(c, "peer", candidates); got != first {
			t.Fatalf("Choose = %q, want the cached %q", got, first)
		}
	}
	if got := f.dials.Load(); got != before {
		t.Fatalf("cached lookups made %d extra attempts, want 0", got-before)
	}
}

// TestForgetReprobes: a caller reporting a failure is what retires an answer.
// Without it, a node whose network moved would be dialed at a dead address forever.
func TestForgetReprobes(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321")
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321"}
	choose(c, "peer", candidates)
	before := f.dials.Load()
	c.Forget("peer")
	choose(c, "peer", candidates)
	if f.dials.Load() <= before {
		t.Fatal("Choose after Forget reused the cache, want a fresh confirmation")
	}
}

// TestForgetDiscardsAnInFlightConfirmation is the same rule on the request path,
// where the probe runs behind the caller rather than in front of it. Prefer hands
// out an unconfirmed address and confirms it in the background, so the failure a
// caller reports is routinely a failure against the address that probe is still
// busy confirming. That probe's handshake succeeds — something is listening,
// which is precisely the wrong-machine case being reported — so recording its
// verdict afterwards would settle the node on the address just retired, and a
// settled entry has nothing left to retire it.
func TestForgetDiscardsAnInFlightConfirmation(t *testing.T) {
	const wrong, working = "10.0.0.1:14321", "10.0.0.2:14321"
	candidates := []string{wrong, working}

	var answering atomic.Value
	answering.Store(wrong)
	var issued atomic.Int64
	dialing := make(chan struct{}, len(candidates))
	release := make(chan struct{})
	returned := make(chan struct{}, len(candidates))
	// Each connect decides its verdict when it is issued and only the first
	// probe's connects are held, which is what lets the test place a Forget inside
	// a probe already under way and still have the replacement probe see a network
	// where the other address is the one that answers.
	dial := func(_, address string, _ time.Duration) (net.Conn, error) {
		accepts := address == answering.Load()
		if issued.Add(1) <= int64(len(candidates)) {
			dialing <- struct{}{}
			<-release
			defer func() { returned <- struct{}{} }()
		}
		if !accepts {
			return nil, errors.New("connection refused")
		}
		local, remote := net.Pipe()
		_ = remote.Close()
		return local, nil
	}
	c := &Chooser{
		timeout:    time.Minute,
		dial:       dial,
		cache:      make(map[string]choice),
		probing:    make(map[string]bool),
		generation: make(map[string]uint64),
	}

	if got := c.Prefer("peer", candidates); got != wrong {
		t.Fatalf("Prefer = %q, want the top-ranked address on first sight", got)
	}
	for range candidates {
		<-dialing
	}

	// The forward to that address failed at the application layer, so the caller
	// retires it while its confirmation is still connecting.
	c.Forget("peer")

	// The node is in fact reachable at its other address, and the replacement
	// probe — which only starts because Forget cleared the registration too — is
	// what gets to say so.
	answering.Store(working)
	if got := c.Prefer("peer", candidates); got != wrong {
		t.Fatalf("Prefer = %q, want the top-ranked address while nothing is confirmed", got)
	}
	waitFor(t, func() bool { return c.Prefer("peer", candidates) == working },
		"a replacement confirmation to settle on the address that answers")

	// Only now is the retired probe let go, so its verdict is offered last and
	// nothing but the generation check can keep it out.
	close(release)
	for range candidates {
		<-returned
	}
	time.Sleep(20 * time.Millisecond)
	if got := c.Prefer("peer", candidates); got != working {
		t.Fatalf("Prefer = %q, want %q — a retired probe's verdict was recorded after the Forget",
			got, working)
	}
}

// TestChooseReprobesWhenTheNodeStopsPublishingTheAddress: an address the node no
// longer claims cannot be kept, so the new list is confirmed from scratch.
func TestChooseReprobesWhenTheNodeStopsPublishingTheAddress(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321", "10.0.9.9:14321")
	c := newTestChooser(f)
	choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"})
	got := choose(c, "peer", []string{"10.0.9.9:14321", "10.0.0.1:14321"})
	if got != "10.0.9.9:14321" {
		t.Fatalf("Choose = %q, want a fresh answer from the new list", got)
	}
}

// TestChooseKeepsAWorkingAddressAcrossARerank is the other half: a node re-ranks
// from what it can see from where it sits, while a connection this host has already
// made is direct evidence of how this host reaches it. As long as the node still
// claims that address, keep it — re-probing to switch off an address with nothing
// wrong with it costs a reconnect and can land somewhere worse.
func TestChooseKeepsAWorkingAddressAcrossARerank(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321", "10.0.9.9:14321")
	c := newTestChooser(f)
	if got := choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"}); got != "10.0.0.2:14321" {
		t.Fatalf("Choose = %q, want the address that accepted", got)
	}
	before := f.dials.Load()

	// The node republishes, promoting an address that also works.
	candidates := []string{"10.0.9.9:14321", "10.0.0.2:14321"}
	if got := choose(c, "peer", candidates); got != "10.0.0.2:14321" {
		t.Fatalf("Choose = %q after a re-rank, want the address already known to work", got)
	}
	if got := f.dials.Load(); got != before {
		t.Errorf("a re-rank re-probed %d times, want the confirmed address kept as-is", got-before)
	}
	// And the new list is what is remembered, so this does not re-scan every call.
	if got := choose(c, "peer", candidates); got != "10.0.0.2:14321" {
		t.Fatalf("Choose = %q, want the kept address", got)
	}
	if got := f.dials.Load(); got != before {
		t.Errorf("made %d further attempts, want none", got-before)
	}
}

// TestPreferNeverBlocksARequest is the request-path guarantee: a proxy resolves
// every discovered node on every inference request, including nodes the request
// will not be routed to, so nothing here may wait on a connect. The answer is the
// node's own ranking, immediately, and the confirmation happens behind it.
func TestPreferNeverBlocksARequest(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	var dials atomic.Int64
	stalled := func(_, _ string, timeout time.Duration) (net.Conn, error) {
		dials.Add(1)
		select {
		case <-blocked:
		case <-time.After(timeout):
		}
		return nil, errors.New("timed out")
	}
	c := &Chooser{
		timeout:    time.Minute,
		dial:       stalled,
		cache:      make(map[string]choice),
		probing:    make(map[string]bool),
		generation: make(map[string]uint64),
	}
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321"}

	start := time.Now()
	for range 50 {
		if got := c.Prefer("peer", candidates); got != "10.0.0.1:14321" {
			t.Fatalf("Prefer = %q, want the top-ranked address without waiting", got)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("50 Prefer calls took %v against a network that never answers", elapsed)
	}

	// One confirmation for the burst, not one per call. The probe is still in
	// flight — nothing answers — so its connects are all there will be.
	waitFor(t, func() bool { return dials.Load() == int64(len(candidates)) },
		"the background confirmation to start")
	time.Sleep(20 * time.Millisecond)
	if n := dials.Load(); n != int64(len(candidates)) {
		t.Errorf("started %d connects, want %d — the burst is probing per call", n, len(candidates))
	}
}

// waitFor polls until done reports true, so a test can observe a background probe
// without a fixed sleep long enough to be slow and short enough to be flaky.
func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPreferUsesTheConfirmedAddressOnceItIsKnown: the point of confirming at all.
// The request that discovers a new node uses its published ranking; the ones behind
// it use the address that answered.
func TestPreferUsesTheConfirmedAddressOnceItIsKnown(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321")
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321"}

	if got := preferred(t, c, "peer", candidates); got != "10.0.0.1:14321" {
		t.Fatalf("Prefer = %q, want the top-ranked address on first sight", got)
	}
	before := f.dials.Load()
	for range 5 {
		if got := c.Prefer("peer", candidates); got != "10.0.0.2:14321" {
			t.Fatalf("Prefer = %q, want the confirmed address", got)
		}
	}
	if got := f.dials.Load(); got != before {
		t.Errorf("a confirmed address cost %d further connects, want 0", got-before)
	}
}

// TestPreferStopsProbingAnUnreachableNode: with nothing answering, requests must
// keep being answered instantly and the confirmation must not restart on every one.
func TestPreferStopsProbingAnUnreachableNode(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321", "10.0.0.3:14321"}

	preferred(t, c, "peer", candidates)
	for range 20 {
		if got := c.Prefer("peer", candidates); got != "10.0.0.1:14321" {
			t.Fatalf("Prefer = %q, want the top-ranked address", got)
		}
	}
	if got := f.dials.Load(); got != int64(len(candidates)) {
		t.Errorf("made %d connects across 21 calls, want %d — the cooldown is not holding",
			got, len(candidates))
	}
}

// TestChooseFallsBackWhenNothingAccepts: a transient blip must not become a hard
// failure. The caller's own error path is better placed to report what happened.
func TestChooseFallsBackWhenNothingAccepts(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	got := choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"})
	if got != "10.0.0.1:14321" {
		t.Fatalf("Choose = %q, want the top-ranked candidate as a fallback", got)
	}
}

// TestChooseSingleCandidateSkipsTheHandshake: with one address there is nothing to
// choose between, and confirming it would only duplicate the connection the caller
// is about to make anyway.
func TestChooseSingleCandidateSkipsTheHandshake(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	if got := choose(c, "peer", []string{"10.0.0.1:14321"}); got != "10.0.0.1:14321" {
		t.Fatalf("Choose = %q, want the only candidate", got)
	}
	if n := f.dials.Load(); n != 0 {
		t.Fatalf("made %d attempts for a single candidate, want 0", n)
	}
}

func TestChooseNoCandidates(t *testing.T) {
	c := newTestChooser(newFakeNet())
	if got := choose(c, "peer", nil); got != "" {
		t.Fatalf("Choose(nil) = %q, want empty", got)
	}
}

// TestChooseRecoversFromAnUnconfirmedFallbackWithoutForget: with nothing accepting
// there is no confirmed answer, so the top candidate is returned but not kept.
// Keeping it would pin a briefly-unreachable node to the wrong address until some
// caller happened to report a failure against it.
func TestChooseRecoversFromAnUnconfirmedFallbackWithoutForget(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	candidates := []string{"192.168.240.1:14321", "10.0.0.9:14321"}
	if got := choose(c, "peer", candidates); got != "192.168.240.1:14321" {
		t.Fatalf("Choose = %q, want the top-ranked address as a last resort", got)
	}

	// The second address comes back. Without a Forget, and without any change to
	// the published list, the next Choose past the cooldown must find it.
	f.mu.Lock()
	f.accept["10.0.0.9:14321"] = true
	f.mu.Unlock()
	c.expireCooldown("peer")
	if got := choose(c, "peer", candidates); got != "10.0.0.9:14321" {
		t.Fatalf("Choose = %q after recovery, want the address that now accepts", got)
	}
}

// TestChooseDoesNotRewalkADeadListEveryCall is the efficiency half of the same
// entry. Callers choose per request — a proxy resolves every discovered node on
// every inference request — so an offline multi-homed node must not make each one
// pay the whole candidate list.
func TestChooseDoesNotRewalkADeadListEveryCall(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321", "10.0.0.3:14321"}

	for range 20 {
		if got := choose(c, "peer", candidates); got != "10.0.0.1:14321" {
			t.Fatalf("Choose = %q, want the top-ranked address", got)
		}
	}
	if got := f.dials.Load(); got != int64(len(candidates)) {
		t.Fatalf("made %d connect attempts across 20 calls, want %d — the walk is repeating",
			got, len(candidates))
	}

	// Once the cooldown lapses the list is walked again, so recovery is noticed
	// without anything having to report a failure.
	c.expireCooldown("peer")
	if got := choose(c, "peer", candidates); got != "10.0.0.1:14321" {
		t.Fatalf("Choose = %q after the cooldown, want the top-ranked address", got)
	}
	if got := f.dials.Load(); got != int64(2*len(candidates)) {
		t.Fatalf("made %d connect attempts, want %d — the cooldown never expires", got, 2*len(candidates))
	}
}

// TestChooseKeepsAConfirmedAddressIndefinitely: the cooldown is only for a walk
// that found nothing. A confirmed address is retired by evidence — a caller
// reporting a failure, or the node dropping it from its list — never by time, which
// would spend connections to learn nothing on a stable network.
func TestChooseKeepsAConfirmedAddressIndefinitely(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321")
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321"}
	if got := choose(c, "peer", candidates); got != "10.0.0.2:14321" {
		t.Fatalf("Choose = %q, want the address that accepted", got)
	}
	before := f.dials.Load()
	c.expireCooldown("peer")
	if got := choose(c, "peer", candidates); got != "10.0.0.2:14321" {
		t.Fatalf("Choose = %q, want the remembered address", got)
	}
	if got := f.dials.Load(); got != before {
		t.Errorf("a confirmed address was re-probed after %v", UnconfirmedCooldown)
	}
}

// TestFirstReportsWhenNothingIsReachable: the distinction matters for one-shot,
// user-initiated work, where "no address answered" is a clear, immediate result
// and a caller's expiring timeout is not.
func TestFirstReportsWhenNothingIsReachable(t *testing.T) {
	f := newFakeNet()
	if _, ok := first(f.dial, []string{"10.0.0.1:1", "10.0.0.2:1"}, time.Millisecond, time.Time{}); ok {
		t.Fatal("first reported success with nothing accepting")
	}
	if n := f.dials.Load(); n != 2 {
		t.Errorf("attempted %d addresses, want both tried before giving up", n)
	}

	f = newFakeNet("10.0.0.2:1")
	got, ok := first(f.dial, []string{"", "10.0.0.1:1", "10.0.0.2:1"}, time.Millisecond, time.Time{})
	if !ok || got != "10.0.0.2:1" {
		t.Fatalf("first = %q,%v, want 10.0.0.2:1,true (blank entries skipped)", got, ok)
	}
}

// TestChooseWithinStopsWhenTheCallersBudgetIsSpent: a caller that caps a whole
// operation must not have an uncapped walk over the same list run first. The
// candidates are probed together and every probe is clamped to what is left of the
// budget, so the confirmation cannot outlive the operation it belongs to.
func TestChooseWithinStopsWhenTheCallersBudgetIsSpent(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)
	var dials atomic.Int64
	slow := func(_, _ string, timeout time.Duration) (net.Conn, error) {
		dials.Add(1)
		select {
		case <-blocked:
		case <-time.After(timeout):
		}
		return nil, errors.New("timed out")
	}
	c := &Chooser{
		timeout:    time.Minute,
		dial:       slow,
		cache:      make(map[string]choice),
		probing:    make(map[string]bool),
		generation: make(map[string]uint64),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := c.ChooseWithin(ctx, "peer", []string{"10.0.0.1:1", "10.0.0.2:1", "10.0.0.3:1"})

	if got != "10.0.0.1:1" {
		t.Fatalf("ChooseWithin = %q, want the top candidate once the budget is spent", got)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("the walk took %v, want it capped near the 80ms budget", elapsed)
	}
	// All three were probed — within one budget, which is the point — rather than
	// the budget being spent on the first address alone.
	if n := dials.Load(); n != 3 {
		t.Errorf("attempted %d addresses, want all three inside the one budget", n)
	}
}

func TestChooseIsConcurrencySafe(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321")
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321"}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			choose(c, "peer", candidates)
			if i%4 == 0 {
				c.Forget("peer")
			}
		}(i)
	}
	wg.Wait()
	if got := choose(c, "peer", candidates); got != "10.0.0.2:14321" {
		t.Fatalf("Choose = %q, want the accepting address", got)
	}
}
