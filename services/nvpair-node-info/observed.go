// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// observed.go records which of this host's addresses remote peers actually reach
// it on, and reports the set upward.
//
// A multi-homed host cannot tell from its own vantage point which of its
// addresses a given peer can use: a direct-connect link between two machines is
// unreachable from every other machine, and nothing local says so. A peer
// completing a request is the one fact that settles it, and this process already
// serves those requests — every peer's inventory poll arrives here — so the proof
// is free and continuous. Everything else address selection can consult (routes,
// interface flags, multicast success) describes the link from this side only.

import (
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// observationTTL is how long a peer connection keeps vouching for a local
// address. Long enough that a quiet peer doesn't retract its proof, short enough
// that an address the host stops answering on stops being advertised as
// peer-proven within a couple of minutes of the link going away.
const observationTTL = 5 * time.Minute

// observedReportEvery re-sends the current set even when nothing changed, so a
// restarted parent relearns it without waiting for the set to move.
const observedReportEvery = 30 * time.Second

// addressObserver holds the local addresses seen serving non-loopback peers,
// each with the time it was last seen.
type addressObserver struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func newAddressObserver() *addressObserver {
	return &addressObserver{seen: make(map[string]time.Time), now: time.Now}
}

// connState is the http.Server.ConnState hook. It records the local address of
// every connection from a non-loopback peer.
//
// StateActive is the point a request has actually been read, which is what makes
// this evidence rather than a guess: a bare TCP connection can come from a port
// scanner, but a request being served means the peer reached this host and this
// host answered. A loopback peer is this machine talking to itself and says
// nothing about what any other machine can reach.
func (o *addressObserver) connState(c net.Conn, state http.ConnState) {
	if state != http.StateActive {
		return
	}
	remote, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok || remote.IP.IsLoopback() {
		return
	}
	local, ok := c.LocalAddr().(*net.TCPAddr)
	if !ok || local.IP.IsLoopback() || local.IP.IsUnspecified() {
		return
	}
	o.record(local.IP.String())
}

func (o *addressObserver) record(addr string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen[addr] = o.now()
}

// addresses returns the still-current observations, sorted so an unchanged set
// reports identically every time.
func (o *addressObserver) addresses() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	cutoff := o.now().Add(-observationTTL)
	addrs := make([]string, 0, len(o.seen))
	for addr, at := range o.seen {
		if at.Before(cutoff) {
			delete(o.seen, addr)
			continue
		}
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	return addrs
}

// reportLoop emits the observed set on a timer for as long as ctx lives. It
// reports unconditionally rather than only on change so a parent that restarted
// mid-session relearns the set on the next tick, and so an expired observation
// propagates.
func (o *addressObserver) reportLoop(done <-chan struct{}, notifier *applog.Notifier) {
	if notifier == nil {
		return
	}
	ticker := time.NewTicker(observedReportEvery)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			o.report(notifier)
		}
	}
}

// report emits the complete current set, including an empty one. Empty is a
// meaningful replacement: it withdraws the last peer-observed evidence after
// its TTL expires. Suppressing it would leave the scanner ranking stale evidence
// forever.
func (o *addressObserver) report(notifier *applog.Notifier) {
	if notifier == nil {
		return
	}
	if err := notifier.Notify(noderec.NotifyObservedAddresses,
		noderec.ObservedAddressesParams{Addresses: o.addresses()}); err != nil {
		slog.Debug("reporting peer-observed addresses failed", "err", err)
	}
}
