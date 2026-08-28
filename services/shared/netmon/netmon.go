// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package netmon watches the host's network interfaces for address changes and
// notifies subscribers when the set of local IPs (or per-interface IPv4
// addresses) changes.
//
// It exists because several nvpair subprocesses snapshot the interface list once
// at startup and never refresh it: ollama-proxy's "is this address local?"
// table and every mDNS responder's per-interface address map. On a multi-homed
// host, or after a sleep/wake that reassigns an IP, those snapshots go stale
// and the process keeps using (or advertising) an address that no longer
// exists. A Monitor lets each process re-read the live address set and react.
//
// Backends are platform-specific (see netmon_windows.go / netmon_linux.go /
// netmon_darwin.go) and fall back to periodic polling elsewhere
// (netmon_poll.go). All backends funnel through the same Monitor, which
// debounces bursts of OS events and only notifies subscribers when an actual
// change is observed, so a spurious or duplicated OS event never produces a
// spurious notification.
package netmon

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// defaultDebounce coalesces a burst of OS change events (an interface
	// coming up often fires several in quick succession) into a single
	// re-enumeration.
	defaultDebounce = 1500 * time.Millisecond
	// pollInterval is how often the portable fallback backend re-checks for
	// changes, and how often a native backend re-checks if it could not
	// register for OS notifications.
	pollInterval = 10 * time.Second
)

// poll calls onEvent on a fixed interval until ctx is cancelled. It is the
// portable fallback used both by the poll-only backend and by a native
// backend whose OS registration failed.
func poll(ctx context.Context, onEvent func()) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			onEvent()
		}
	}
}

// Snapshot is an immutable view of the host's addressing at a point in time.
type Snapshot struct {
	// LocalIPs is the set of every unicast IP (v4 and v6) bound to any
	// interface, keyed by its String() form. Used to answer "is this
	// address one of mine?".
	LocalIPs map[string]bool
	// IfaceV4 maps interface index to its non-loopback IPv4 addresses on
	// multicast-capable, up interfaces. Mirrors the filter every mDNS
	// responder applies, so responders can adopt it directly.
	IfaceV4 map[int][]net.IP
}

func (s Snapshot) clone() Snapshot {
	out := Snapshot{
		LocalIPs: make(map[string]bool, len(s.LocalIPs)),
		IfaceV4:  make(map[int][]net.IP, len(s.IfaceV4)),
	}
	for ip := range s.LocalIPs {
		out.LocalIPs[ip] = true
	}
	for idx, ips := range s.IfaceV4 {
		cp := make([]net.IP, len(ips))
		copy(cp, ips)
		out.IfaceV4[idx] = cp
	}
	return out
}

// Enumerate reads the current interface/address state. It never errors: on a
// failure to list interfaces it returns empty maps, which callers treat as
// "nothing local right now".
func Enumerate() Snapshot {
	s := Snapshot{LocalIPs: map[string]bool{}, IfaceV4: map[int][]net.IP{}}
	ifaces, err := net.Interfaces()
	if err != nil {
		return s
	}
	for _, ifi := range ifaces {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		mcastV4 := ifi.Flags&net.FlagUp != 0 &&
			ifi.Flags&net.FlagMulticast != 0 &&
			ifi.Flags&net.FlagLoopback == 0
		var v4 []net.IP
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			s.LocalIPs[ip.String()] = true
			if mcastV4 {
				if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
					v4 = append(v4, ip4)
				}
			}
		}
		if len(v4) > 0 {
			s.IfaceV4[ifi.Index] = v4
		}
	}
	return s
}

// fingerprint reduces a Snapshot to a stable string so two snapshots can be
// compared without caring about map iteration order.
func fingerprint(s Snapshot) string {
	parts := make([]string, 0, len(s.LocalIPs)+len(s.IfaceV4))
	for ip := range s.LocalIPs {
		parts = append(parts, "ip:"+ip)
	}
	idxs := make([]int, 0, len(s.IfaceV4))
	for idx := range s.IfaceV4 {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		ips := s.IfaceV4[idx]
		strs := make([]string, len(ips))
		for j, ip := range ips {
			strs[j] = ip.String()
		}
		sort.Strings(strs)
		parts = append(parts, fmt.Sprintf("if%d:%s", idx, strings.Join(strs, ",")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// Monitor tracks the live addressing state and fans out change notifications.
type Monitor struct {
	debounce time.Duration

	mu   sync.RWMutex
	snap Snapshot
	fp   string
	subs []chan struct{}
}

// Watch starts a Monitor that runs until ctx is cancelled. The platform
// backend (watchChanges) drives re-enumeration; the returned Monitor is usable
// immediately and already carries an initial Snapshot.
func Watch(ctx context.Context) (*Monitor, error) {
	initial := Enumerate()
	m := &Monitor{
		debounce: defaultDebounce,
		snap:     initial,
		fp:       fingerprint(initial),
	}

	events := make(chan struct{}, 1)
	go watchChanges(ctx, func() {
		select {
		case events <- struct{}{}:
		default:
		}
	})
	go m.loop(ctx, events)
	return m, nil
}

func (m *Monitor) loop(ctx context.Context, events <-chan struct{}) {
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			m.closeSubs()
			return
		case <-events:
			// (Re)arm the debounce window. Stop-and-drain before Reset is
			// the safe pattern for a timer that may have already fired.
			if timer == nil {
				timer = time.NewTimer(m.debounce)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(m.debounce)
			}
			timerC = timer.C
		case <-timerC:
			timer = nil
			timerC = nil
			m.refresh()
		}
	}
}

func (m *Monitor) refresh() {
	next := Enumerate()
	nextFP := fingerprint(next)

	m.mu.Lock()
	if nextFP == m.fp {
		m.mu.Unlock()
		return
	}
	m.snap = next
	m.fp = nextFP
	subs := make([]chan struct{}, len(m.subs))
	copy(subs, m.subs)
	m.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (m *Monitor) closeSubs() {
	m.mu.Lock()
	for _, ch := range m.subs {
		close(ch)
	}
	m.subs = nil
	m.mu.Unlock()
}

// Subscribe returns a channel that receives a value whenever the addressing
// changes. The channel is buffered (depth 1) and notifications coalesce, so a
// slow consumer never blocks the Monitor and only learns "something changed,
// re-read Snapshot". The channel is closed when the Monitor's context ends.
func (m *Monitor) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	m.subs = append(m.subs, ch)
	m.mu.Unlock()
	return ch
}

// Snapshot returns a defensive copy of the current addressing state.
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap.clone()
}

// LocalIPs returns a copy of the current local-IP set, a convenience for the
// common "is this address mine?" caller.
func (m *Monitor) LocalIPs() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.snap.LocalIPs))
	for ip := range m.snap.LocalIPs {
		out[ip] = true
	}
	return out
}
