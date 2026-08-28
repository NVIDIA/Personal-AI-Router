// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package reach confirms which of a node's published addresses actually accepts a
// connection, and remembers the answer.
//
// A node publishes a ranked list of addresses rather than one, because no single
// address is reachable from everywhere: a direct-connect link between two
// machines works only from the machine on its far end, and a host cannot tell
// from its own vantage point which of its addresses a given peer will be able to
// use. The publisher's ranking is its best evidence, not a guarantee, so anything
// that must connect confirms it by connecting.
//
// The confirmation is a TCP handshake to a port that is already listening. The
// SYN / SYN-ACK / ACK exchange is itself the hi, hello and accept, so this needs
// no new protocol, no new port, and no new firewall rule, and it costs one round
// trip on a reachable link. An unreachable address either refuses immediately or
// hits the per-candidate timeout, which is why that timeout is short: the point is
// to stop paying a full request timeout, per attempt, for an address that was
// never going to work.
//
// This exists as one package because every dialer needs exactly the same thing.
// The alternative — each service walking the candidate list its own way — is how
// the two proxies came to have separate copies of it while the cluster and roster
// paths had none, and would sit on a known-good address without trying it.
package reach

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds one candidate's connect attempt. It is generous for a LAN
// round trip and short enough that walking a few candidates stays well inside a
// caller's own budget: the whole point is to fail over faster than a single
// request timeout would elapse.
//
// It is not tuned down: the cost of being too short is skipping an address that
// does work, on exactly the congested VPN or WAN link where a handshake is slowest
// and failover matters most.
const DefaultTimeout = time.Second

// DialFunc connects to address, giving up after timeout. Injectable so tests can
// describe a network instead of opening real sockets.
type DialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

func netDial(network, address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, address, timeout)
}

// accepts reports whether address completes a TCP handshake within timeout. The
// connection is closed immediately: the handshake is the entire question, and
// anything sent afterwards would be a protocol this package has no business
// knowing.
func accepts(dial DialFunc, address string, timeout time.Duration) bool {
	conn, err := dial("tcp", address, timeout)
	if err != nil {
		slog.Debug("reach: candidate did not accept, trying next", "address", address, "err", err)
		return false
	}
	_ = conn.Close()
	return true
}

// First returns the first candidate that accepts a connection, in the order given.
// ok is false when none does, which is a real answer worth surfacing: it says the
// node published nothing this host can reach, and reporting that beats letting a
// caller's own timeout expire against an address that will never answer.
//
// For one-shot, user-initiated work. A caller that dials the same node repeatedly
// should use a Chooser so a confirmed address is not re-probed every time.
func First(candidates []string, timeout time.Duration) (address string, ok bool) {
	return first(netDial, candidates, timeout, time.Time{})
}

// first probes the candidates together and returns the best-ranked one that
// accepts. A non-zero deadline caps the whole probe.
//
// Together rather than one after another because the expensive failure is an
// address that neither answers nor refuses: a dropped SYN costs the full timeout,
// and paying that per address means a node whose leading address blackholes can
// exhaust a caller's entire budget before reaching an address that works. Probing
// concurrently bounds the walk at one timeout however many addresses the node
// published. The cost is one connect per candidate instead of stopping at the
// first success, which is affordable precisely because a walk is now rare — a
// settled address is reused without probing at all.
//
// The verdicts are read in the node's published order, not in the order they
// arrive. That is what makes the answer identical to a sequential walk's:
// whichever address happens to finish its handshake first, the one returned is the
// best-ranked address that accepted. Taking the fastest responder instead would
// let two equally reachable addresses trade places on timing noise, and every
// swap costs a reconnect to learn nothing.
func first(dial DialFunc, candidates []string, timeout time.Duration, deadline time.Time) (string, bool) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			slog.Debug("reach: confirmation budget spent before it began; nothing was probed",
				"candidates", candidates)
			return "", false
		}
		timeout = min(timeout, remaining)
	}

	// Buffered so a probe whose verdict is no longer wanted — every candidate
	// ranked below the one that won — still finishes and exits rather than
	// blocking on a send nobody will receive.
	verdicts := make([]chan bool, len(candidates))
	for i, address := range candidates {
		if address == "" {
			continue
		}
		verdicts[i] = make(chan bool, 1)
		go func(address string, verdict chan<- bool) {
			verdict <- accepts(dial, address, timeout)
		}(address, verdicts[i])
	}

	for i, verdict := range verdicts {
		if verdict == nil {
			continue
		}
		if <-verdict {
			return candidates[i], true
		}
	}
	return "", false
}

// UnconfirmedCooldown is how long a walk that confirmed nothing suppresses the
// next one for the same node.
//
// It exists because callers choose per request, not per node: a proxy resolves
// every discovered node on every inference request. Without it, one offline
// multi-homed node makes every request pay the whole candidate list at
// DefaultTimeout each, which is a far worse outcome than the stale address the
// cooldown risks. Short enough that a node coming back is picked up within a
// discovery cycle, so recovery still needs no explicit Forget.
const UnconfirmedCooldown = 5 * time.Second

// choice is the address chosen for a node plus the candidate list it was chosen
// from. The fingerprint is how a republished list is noticed at all; what happens
// then depends on whether the address survived it (see reuse).
//
// settled marks an entry that needs no further probing: a handshake proved the
// address, or the caller asked for none and the top candidate is by definition
// its answer. It is kept until a caller retires it with Forget. An unsettled
// entry is the consolation prize from a walk that confirmed nothing, and exists
// only to stop the next call repeating that walk, so it expires on its own.
type choice struct {
	fingerprint string
	address     string
	settled     bool
	at          time.Time
}

// reuse reports the address to return without probing, and whether it can be.
func (c choice) reuse(candidates []string, fingerprint string, now time.Time) (string, bool) {
	if c.fingerprint == fingerprint {
		return c.address, c.settled || now.Sub(c.at) < UnconfirmedCooldown
	}
	// The node republished its list, so it has re-ranked — possibly onto an address
	// it now has better evidence for. A settled address it still claims is kept
	// anyway: a connection this host has actually made is better evidence for how
	// this host reaches that node than the node's own ranking, which it derives
	// from what it can see from where it sits. Dropping an address with nothing
	// wrong with it costs a reconnect and can land somewhere worse. An address the
	// node stopped claiming is a different matter, and is walked away from.
	return c.address, c.settled && slices.Contains(candidates, c.address)
}

// Chooser remembers which address worked for each node so a repeated dial costs
// nothing. It is safe for concurrent use.
//
// The cache is deliberately not time-expiring. A confirmed address stops being
// used for one of two concrete reasons — the caller reports a failure against it
// (Forget), or the node stops publishing it — and both are observed events.
// Re-probing on a timer would spend connections to learn nothing on the
// overwhelmingly common stable-network path, and each swap it provoked would cost
// a reconnect.
type Chooser struct {
	timeout time.Duration
	dial    DialFunc

	mu    sync.Mutex
	cache map[string]choice
	// probing holds the keys with a background confirmation already running, so a
	// burst of requests for the same unconfirmed node starts one probe rather than
	// one per request.
	probing map[string]bool
	// generation counts how many times each key's answer has been retired. A probe
	// captures it before it starts and its verdict is dropped if it has moved
	// since, because a Forget that lands mid-probe is a caller reporting that the
	// very address being probed did not work at the application layer — and a
	// handshake succeeds against anything listening on that port, including the
	// wrong machine. Installing such a verdict afterwards would settle the node on
	// the address just retired, where it would stay: a settled entry never
	// expires, and the failure that would have retired it has already been
	// reported.
	generation map[string]uint64
}

// Option adjusts a Chooser at construction.
type Option func(*Chooser)

// WithDial replaces the connect function, so a caller can exercise confirmation
// and failover against a described network rather than real sockets.
func WithDial(fn DialFunc) Option {
	return func(c *Chooser) {
		if fn != nil {
			c.dial = fn
		}
	}
}

// NewChooser builds a Chooser with DefaultTimeout and real TCP dialing.
func NewChooser(opts ...Option) *Chooser {
	c := &Chooser{
		timeout:    DefaultTimeout,
		dial:       netDial,
		cache:      make(map[string]choice),
		probing:    make(map[string]bool),
		generation: make(map[string]uint64),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Prefer returns the address to use for key without ever dialing.
//
// This is the form for a request path. A proxy resolves every discovered node on
// every inference request, so a handshake here would be paid by the user's request
// — and paid for nodes the request is not even going to be routed to. So: answer
// at once with the confirmed address if there is one and the node's own top-ranked
// address otherwise, and start one background confirmation so the requests behind
// this one have a better answer.
//
// Not waiting costs nothing that was not already handled. An address that turns out
// to be wrong surfaces as a transport error, which the caller already reacts to by
// calling Forget and failing over to the next node; the confirmation started here
// has normally landed by the request after that. Waiting, by contrast, would put a
// connect timeout in front of requests that had no need of one.
func (c *Chooser) Prefer(key string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	fingerprint := strings.Join(candidates, "|")

	c.mu.Lock()
	if address, ok := c.reuse(key, candidates, fingerprint, time.Now()); ok {
		c.mu.Unlock()
		return address
	}
	confirm := !c.probing[key]
	if confirm {
		c.probing[key] = true
	}
	generation := c.generation[key]
	c.mu.Unlock()

	if confirm {
		go c.confirm(key, candidates, fingerprint, generation)
	}
	return candidates[0]
}

// ChooseWithin returns the address to use for key, confirming it by connecting when
// there is nothing remembered, bounded by ctx's deadline.
//
// For background and one-shot work, where being right on the first attempt is worth
// more than the latency — a periodic reconcile, or an operation a person just asked
// for. A request path should use Prefer instead.
//
// When no candidate accepts, the top candidate is returned anyway. Refusing to
// return an address would turn a transient network blip into a hard failure,
// whereas attempting the best-ranked one lets the caller's own error path report
// what really happened.
func (c *Chooser) ChooseWithin(ctx context.Context, key string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	fingerprint := strings.Join(candidates, "|")
	deadline, _ := ctx.Deadline()

	c.mu.Lock()
	if address, ok := c.reuse(key, candidates, fingerprint, time.Now()); ok {
		c.mu.Unlock()
		return address
	}
	generation := c.generation[key]
	c.mu.Unlock()

	address, ok := first(c.dial, candidates, c.timeout, deadline)
	return c.remember(key, candidates, fingerprint, address, ok, generation)
}

// confirm probes in the background for Prefer and records the result for the calls
// that follow.
//
// The registration is released only if this probe is still the current one. A
// Forget clears it as part of retiring the answer, and the next Prefer starts a
// replacement; releasing it here regardless would clear that replacement's
// registration and let a third probe start alongside it.
func (c *Chooser) confirm(key string, candidates []string, fingerprint string, generation uint64) {
	address, ok := first(c.dial, candidates, c.timeout, time.Time{})
	c.remember(key, candidates, fingerprint, address, ok, generation)
	c.mu.Lock()
	if c.generation[key] == generation {
		delete(c.probing, key)
	}
	c.mu.Unlock()
}

// reuse returns the remembered address for key when it can be used as-is. Callers
// hold c.mu.
func (c *Chooser) reuse(key string, candidates []string, fingerprint string, now time.Time) (string, bool) {
	e, ok := c.cache[key]
	if !ok {
		return "", false
	}
	address, keep := e.reuse(candidates, fingerprint, now)
	if !keep {
		return "", false
	}
	// Re-stamp the list this answer now belongs to, so an address kept across a
	// re-rank does not re-scan the new list on every later call.
	e.fingerprint = fingerprint
	c.cache[key] = e
	return address, true
}

// remember records a probe's outcome and returns the address to use. The outcome
// is still returned when it is not recorded: it is what this probe actually
// found, and the caller that started it has a connection to make either way.
func (c *Chooser) remember(key string, candidates []string, fingerprint, address string, ok bool, generation uint64) string {
	if !ok {
		// The top candidate beats refusing: a blip must not become a hard failure,
		// and the caller's own error path reports what really happened. It is
		// recorded as unconfirmed so it expires on its own after
		// UnconfirmedCooldown — long enough that a caller asking per request does
		// not re-walk a dead list every time, short enough that a node coming back
		// needs no explicit Forget.
		slog.Debug("reach: no candidate accepted; using the top-ranked address until the cooldown expires",
			"key", key, "candidates", candidates, "cooldown", UnconfirmedCooldown)
		address = candidates[0]
	}

	c.mu.Lock()
	if c.generation[key] == generation {
		c.cache[key] = choice{fingerprint: fingerprint, address: address, settled: ok, at: time.Now()}
	} else {
		slog.Debug("reach: discarding a confirmation that was retired while it was in flight",
			"key", key, "address", address)
	}
	c.mu.Unlock()
	return address
}

// Forget drops the remembered address for key so the next call confirms again.
// Callers invoke it when a connection to the chosen address fails: that failure is
// the evidence which retires the answer, and without it a node whose network moved
// would keep being dialed at an address that no longer works.
//
// It also retires a confirmation that is still in flight, and clears its
// registration so the next Prefer starts a replacement. Both matter for the case
// this is called from: Prefer hands out an unconfirmed address and starts a probe
// behind it, so the failure being reported here is very often a failure against
// the address that probe is busy confirming.
func (c *Chooser) Forget(key string) {
	c.mu.Lock()
	delete(c.cache, key)
	delete(c.probing, key)
	c.generation[key]++
	c.mu.Unlock()
}
