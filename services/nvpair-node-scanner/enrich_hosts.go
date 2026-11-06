// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"log/slog"
	"slices"
	"sync"

	"nvpair-shared/noderec"
)

// modelInventory carries engine-manager's model lists and, for remote responses,
// the certificate used to authenticate the server.
type modelInventory struct {
	models         []string
	byEngine       map[string][]string
	loadedByEngine map[string][]string
	certificate    *x509.Certificate
}

// hostKey identifies address memory for one record and service. Node-info uses
// discovery record IDs to keep candidates separate from verified peers; model
// reads use the canonical host UUID. Each service remembers its own working IP.
type hostKey struct {
	hostUUID string
	service  noderec.ServiceKey
}

// hostMemory remembers which published address answered, per node and service.
//
// It exists because the enrichment sweep repeats every peerRefreshInterval for as
// long as a node is advertised. Without it, every sweep started at the top of the
// node's list, so a multi-homed peer whose leading address this host cannot reach
// paid that address's full fetch timeout — up to modelsFetchTimeout — again and
// again, forever, to reach the address that had already worked the last time.
type hostMemory struct {
	mu    sync.Mutex
	hosts map[hostKey]string
}

func (m *hostMemory) get(key hostKey) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hosts[key]
}

func (m *hostMemory) set(key hostKey, host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hosts == nil {
		m.hosts = make(map[hostKey]string)
	}
	m.hosts[key] = host
}

func (m *hostMemory) forget(key hostKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.hosts, key)
}

// askRemembered asks the address that answered last sweep, by itself, and falls
// back to the rest of the list only when it has stopped answering.
//
// By itself, and first, because nothing is wrong with an address that is still
// answering: racing it against its siblings would let one of them win on timing
// alone, and each switch changes the address this node is enriched and reported at
// to learn nothing. Only its failure is a reason to look elsewhere.
//
// ask must be safe to call concurrently and reports whether the host answered for
// the node in question — a reused direct-connect address can answer for a
// different machine, which is not an answer for this one.
func askRemembered[T any](mem *hostMemory, key hostKey, hosts []string, ask func(host string) (T, bool)) (string, T, bool) {
	if remembered := mem.get(key); remembered != "" {
		if slices.Contains(hosts, remembered) {
			if value, ok := ask(remembered); ok {
				return remembered, value, true
			}
		}
		mem.forget(key)
		slog.Debug("enrichment: the remembered address stopped answering; trying the rest of the list",
			"host_uuid", key.hostUUID, "service", key.service, "address", remembered)
		hosts = withoutHost(hosts, remembered)
	}

	host, value, ok := askTogether(hosts, ask)
	if ok {
		mem.set(key, host)
	}
	return host, value, ok
}

// askTogether asks every host at once and returns the answer from the best-ranked
// host that gave one.
//
// At once because the failure that costs real time is an address that neither
// answers nor refuses: a dropped SYN costs the whole fetch timeout, so asking in
// sequence meant one such address delayed every address behind it. Asking together
// bounds a sweep at one timeout however many addresses a node published.
//
// The answers are then read in the node's published order rather than the order
// they arrive, so the host chosen is the one the node ranks highest among those
// that answered. Taking the fastest instead would let two working addresses swap
// places between sweeps on nothing but timing noise.
func askTogether[T any](hosts []string, ask func(host string) (T, bool)) (string, T, bool) {
	type answer struct {
		value T
		ok    bool
	}
	// Buffered so a probe whose answer is no longer wanted still finishes and exits
	// rather than blocking on a send nobody will receive.
	answers := make([]chan answer, len(hosts))
	for i, host := range hosts {
		if host == "" {
			continue
		}
		answers[i] = make(chan answer, 1)
		go func(host string, out chan<- answer) {
			value, ok := ask(host)
			out <- answer{value: value, ok: ok}
		}(host, answers[i])
	}

	for i, ch := range answers {
		if ch == nil {
			continue
		}
		if got := <-ch; got.ok {
			return hosts[i], got.value, true
		}
	}
	var zero T
	return "", zero, false
}

func withoutHost(hosts []string, drop string) []string {
	out := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if host != drop {
			out = append(out, host)
		}
	}
	return out
}
