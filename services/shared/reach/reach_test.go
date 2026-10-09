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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		require.LessOrEqual(t, time.Now(), deadline, "the background confirmation never finished")
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
	assert.Equal(t, "10.172.55.129:14321", choose(c, "peer", []string{"192.168.240.1:14321", "10.172.55.129:14321"}))
	require.Len(t, f.attemptedAddresses(), 2)
}

// TestChoosePrefersTheBestRankedAcceptingAddress: with the candidates probed
// together, the answer still has to be the node's own ranking rather than whichever
// handshake happened to land first. Otherwise two equally reachable addresses would
// trade places on timing noise, and every swap costs a reconnect for nothing.
func TestChoosePrefersTheBestRankedAcceptingAddress(t *testing.T) {
	f := newFakeNet("10.0.0.1:14321", "10.0.0.2:14321")
	c := newTestChooser(f)
	assert.Equal(t, "10.0.0.1:14321", choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"}))
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
	require.True(t, ok, "first")
	assert.Equal(t, "10.0.0.1:1", got, "first")
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

	require.True(t, ok, "first")
	assert.Equal(t, "10.0.0.4:1", got, "first")
	// Sequentially this would be three timeouts before the fourth address was even
	// attempted.
	assert.LessOrEqual(t, elapsed, 2*timeout, "probing four addresses took")
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
		assert.Equal(t, first, choose(c, "peer", candidates))
	}
	assert.Equal(t, before, f.dials.Load(), "cached lookups made new dials")
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
	assert.Greater(t, f.dials.Load(), before, "Choose after Forget reused the cache, want a fresh confirmation")
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

	assert.Equal(t, wrong, c.Prefer("peer", candidates))
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
	assert.Equal(t, wrong, c.Prefer("peer", candidates))
	waitFor(t, func() bool { return c.Prefer("peer", candidates) == working },
		"a replacement confirmation to settle on the address that answers")

	// Only now is the retired probe let go, so its verdict is offered last and
	// nothing but the generation check can keep it out.
	close(release)
	for range candidates {
		<-returned
	}
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, working, c.Prefer("peer", candidates))
}

// TestChooseReprobesWhenTheNodeStopsPublishingTheAddress: an address the node no
// longer claims cannot be kept, so the new list is confirmed from scratch.
func TestChooseReprobesWhenTheNodeStopsPublishingTheAddress(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321", "10.0.9.9:14321")
	c := newTestChooser(f)
	choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"})
	assert.Equal(t, "10.0.9.9:14321", choose(c, "peer", []string{"10.0.9.9:14321", "10.0.0.1:14321"}))
}

// TestChooseKeepsAWorkingAddressAcrossARerank is the other half: a node re-ranks
// from what it can see from where it sits, while a connection this host has already
// made is direct evidence of how this host reaches it. As long as the node still
// claims that address, keep it — re-probing to switch off an address with nothing
// wrong with it costs a reconnect and can land somewhere worse.
func TestChooseKeepsAWorkingAddressAcrossARerank(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321", "10.0.9.9:14321")
	c := newTestChooser(f)
	assert.Equal(t, "10.0.0.2:14321", choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"}))
	before := f.dials.Load()

	// The node republishes, promoting an address that also works.
	candidates := []string{"10.0.9.9:14321", "10.0.0.2:14321"}
	assert.Equal(t, "10.0.0.2:14321", choose(c, "peer", candidates))
	assert.Equal(t, before, f.dials.Load(), "a re-rank re-probed")
	// And the new list is what is remembered, so this does not re-scan every call.
	assert.Equal(t, "10.0.0.2:14321", choose(c, "peer", candidates))
	assert.Equal(t, before, f.dials.Load())
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
		assert.Equal(t, "10.0.0.1:14321", c.Prefer("peer", candidates))
	}
	assert.LessOrEqual(t, time.Since(start), time.Second, "50 Prefer calls took")

	// One confirmation for the burst, not one per call. The probe is still in
	// flight — nothing answers — so its connects are all there will be.
	waitFor(t, func() bool { return dials.Load() == int64(len(candidates)) },
		"the background confirmation to start")
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(len(candidates)), dials.Load(), "background confirmation dials")
}

// waitFor polls until done reports true, so a test can observe a background probe
// without a fixed sleep long enough to be slow and short enough to be flaky.
func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() {
		require.LessOrEqual(t, time.Now(), deadline, "timed out waiting for %s", what)
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

	assert.Equal(t, "10.0.0.1:14321", preferred(t, c, "peer", candidates))
	before := f.dials.Load()
	for range 5 {
		assert.Equal(t, "10.0.0.2:14321", c.Prefer("peer", candidates))
	}
	assert.Equal(t, before, f.dials.Load(), "a confirmed address cost new dials")
}

// TestPreferStopsProbingAnUnreachableNode: with nothing answering, requests must
// keep being answered instantly and the confirmation must not restart on every one.
func TestPreferStopsProbingAnUnreachableNode(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321", "10.0.0.3:14321"}

	preferred(t, c, "peer", candidates)
	for range 20 {
		assert.Equal(t, "10.0.0.1:14321", c.Prefer("peer", candidates))
	}
	assert.Equal(t, int64(len(candidates)), f.dials.Load())
}

// TestChooseFallsBackWhenNothingAccepts: a transient blip must not become a hard
// failure. The caller's own error path is better placed to report what happened.
func TestChooseFallsBackWhenNothingAccepts(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	assert.Equal(t, "10.0.0.1:14321", choose(c, "peer", []string{"10.0.0.1:14321", "10.0.0.2:14321"}))
}

// TestChooseSingleCandidateSkipsTheHandshake: with one address there is nothing to
// choose between, and confirming it would only duplicate the connection the caller
// is about to make anyway.
func TestChooseSingleCandidateSkipsTheHandshake(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	assert.Equal(t, "10.0.0.1:14321", choose(c, "peer", []string{"10.0.0.1:14321"}))
	assert.Equal(t, int64(0), f.dials.Load())
}

func TestChooseNoCandidates(t *testing.T) {
	c := newTestChooser(newFakeNet())
	assert.Equal(t, "", choose(c, "peer", nil))
}

// TestChooseRecoversFromAnUnconfirmedFallbackWithoutForget: with nothing accepting
// there is no confirmed answer, so the top candidate is returned but not kept.
// Keeping it would pin a briefly-unreachable node to the wrong address until some
// caller happened to report a failure against it.
func TestChooseRecoversFromAnUnconfirmedFallbackWithoutForget(t *testing.T) {
	f := newFakeNet()
	c := newTestChooser(f)
	candidates := []string{"192.168.240.1:14321", "10.0.0.9:14321"}
	assert.Equal(t, "192.168.240.1:14321", choose(c, "peer", candidates))

	// The second address comes back. Without a Forget, and without any change to
	// the published list, the next Choose past the cooldown must find it.
	f.mu.Lock()
	f.accept["10.0.0.9:14321"] = true
	f.mu.Unlock()
	c.expireCooldown("peer")
	assert.Equal(t, "10.0.0.9:14321", choose(c, "peer", candidates))
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
		assert.Equal(t, "10.0.0.1:14321", choose(c, "peer", candidates))
	}
	assert.Equal(t, int64(len(candidates)), f.dials.Load())

	// Once the cooldown lapses the list is walked again, so recovery is noticed
	// without anything having to report a failure.
	c.expireCooldown("peer")
	assert.Equal(t, "10.0.0.1:14321", choose(c, "peer", candidates))
	assert.Equal(t, int64(2*len(candidates)), f.dials.Load())
}

// TestChooseKeepsAConfirmedAddressIndefinitely: the cooldown is only for a walk
// that found nothing. A confirmed address is retired by evidence — a caller
// reporting a failure, or the node dropping it from its list — never by time, which
// would spend connections to learn nothing on a stable network.
func TestChooseKeepsAConfirmedAddressIndefinitely(t *testing.T) {
	f := newFakeNet("10.0.0.2:14321")
	c := newTestChooser(f)
	candidates := []string{"10.0.0.1:14321", "10.0.0.2:14321"}
	assert.Equal(t, "10.0.0.2:14321", choose(c, "peer", candidates))
	before := f.dials.Load()
	c.expireCooldown("peer")
	assert.Equal(t, "10.0.0.2:14321", choose(c, "peer", candidates))
	assert.Equal(t, before, f.dials.Load(), "a confirmed address was re-probed after cooldown")
}

// TestFirstReportsWhenNothingIsReachable: the distinction matters for one-shot,
// user-initiated work, where "no address answered" is a clear, immediate result
// and a caller's expiring timeout is not.
func TestFirstReportsWhenNothingIsReachable(t *testing.T) {
	f := newFakeNet()
	_, ok := first(f.dial, []string{"10.0.0.1:1", "10.0.0.2:1"}, time.Millisecond, time.Time{})
	require.False(t, ok, "first reported success with nothing accepting")
	assert.Equal(t, int64(2), f.dials.Load())

	f = newFakeNet("10.0.0.2:1")
	got, ok := first(f.dial, []string{"", "10.0.0.1:1", "10.0.0.2:1"}, time.Millisecond, time.Time{})
	require.True(t, ok, "first")
	assert.Equal(t, "10.0.0.2:1", got, "first")
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
	assert.Equal(t, "10.0.0.1:1", c.ChooseWithin(ctx, "peer", []string{"10.0.0.1:1", "10.0.0.2:1", "10.0.0.3:1"}))
	assert.LessOrEqual(t, time.Since(start), 500*time.Millisecond, "the walk took")
	// All three were probed — within one budget, which is the point — rather than
	// the budget being spent on the first address alone.
	assert.Equal(t, int64(3), dials.Load())
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
	assert.Equal(t, "10.0.0.2:14321", choose(c, "peer", candidates))
}
