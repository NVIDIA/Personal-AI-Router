// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// trustedReaders is the set of peer addresses the broker currently sees a PAIR
// node on. It gates the PLAINTEXT inventory only; the cluster-gated mTLS path is
// unchanged and stricter.
//
// Fail-open until told. `told` separates "the broker says there are no peers"
// from "no broker has ever pushed", which are the same empty set and must not be
// the same answer: gating on the second would break every deployment whose
// supervisor does not push, including node-info run standalone.
type trustedReaders struct {
	mu    sync.RWMutex
	addrs map[string]struct{}
	told  bool
}

func newTrustedReaders() *trustedReaders {
	return &trustedReaders{addrs: map[string]struct{}{}}
}

// normalizeAddr parses an address into the one form both sides of the
// comparison must agree on. netip rather than net.ParseIP because ParseIP
// returns nil for a zoned IPv6 literal ("fe80::1%en0"), which is exactly the
// form a link-local LAN caller arrives as -- and an address that fails to parse
// used to fall through to "allow", so link-local traffic walked past this gate
// entirely. Unmap folds ::ffff:1.2.3.4 onto 1.2.3.4; WithZone("") drops the
// interface scope, which names the receiver's own NIC and cannot identify a peer.
func normalizeAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

func (t *trustedReaders) set(addresses []string) {
	next := make(map[string]struct{}, len(addresses))
	dropped := 0
	for _, a := range addresses {
		if addr, ok := normalizeAddr(a); ok {
			next[addr.String()] = struct{}{}
			continue
		}
		// Silently dropping is the dangerous case: the broker believes it
		// installed policy while a real peer is now refused.
		dropped++
		slog.Warn("trusted-readers push contained an address that is not an IP; that peer will be refused", "address", a)
	}
	if dropped > 0 {
		slog.Warn("some trusted reader addresses were unusable", "dropped", dropped, "kept", len(next))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.addrs = next
	t.told = true
}

// allows reports whether a remote address may read the plaintext inventory.
// Loopback is always allowed: the local broker, scanner and desktop app all read
// over it, and a caller already on this host has no need of the endpoint to
// learn the host's own hardware.
// allows is nil-safe on purpose: a nil set is "no policy has been installed",
// which is the same documented fail-open as `!told`. It is NOT a way to skip the
// check -- the handler used to test `readers != nil`, which meant any path that
// forgot to construct one served the inventory to the whole LAN.
func (t *trustedReaders) allows(remoteAddr string) bool {
	if t == nil {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, ok := normalizeAddr(host)
	if !ok {
		// A TCP RemoteAddr always carries a parseable IP, so reaching here means
		// something we cannot classify. Deny: the previous fail-open here was
		// the link-local bypass described on normalizeAddr.
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.told {
		return true
	}
	_, known := t.addrs[addr.String()]
	return known
}

// handleTrustedReaders applies a MethodSetTrustedReaders notification. A
// malformed payload is dropped rather than latching a wrong set: the broker
// re-pushes on every discovery change, so the next one corrects us.
func handleTrustedReaders(msg applog.StdinMessage, readers *trustedReaders) {
	if msg.Method != noderec.MethodSetTrustedReaders {
		return
	}
	var params noderec.TrustedReadersParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		slog.Warn("ignoring malformed trusted-readers push", "err", err)
		return
	}
	readers.set(params.Addresses)
	slog.Debug("trusted readers updated", "count", len(params.Addresses))
}

// denyUntrustedReader writes the 403 for a caller outside the set. Split out so
// the handler reads as one decision.
func denyUntrustedReader(w http.ResponseWriter, remoteAddr string) {
	slog.Debug("refused node-info read from a non-peer address", "remote", remoteAddr)
	http.Error(w, "forbidden: not a known PAIR node on this network", http.StatusForbidden)
}
