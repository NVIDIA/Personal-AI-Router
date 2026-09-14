// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"nvpair-shared/clustertrust"
)

func TestTrustedReadersAllows(t *testing.T) {
	r := newTrustedReaders()

	// Fail-open until the broker has ever pushed: node-info run standalone, or
	// under a supervisor that does not push, must not lock everyone out.
	if !r.allows("192.168.1.99:5000") {
		t.Error("before any push, a LAN caller must still be answered")
	}

	r.set([]string{"192.168.1.21", "10.0.0.7"})

	for _, tc := range []struct {
		remote string
		want   bool
		why    string
	}{
		{"192.168.1.21:51000", true, "a known peer"},
		{"10.0.0.7:9", true, "another known peer"},
		{"192.168.1.99:51000", false, "a LAN device that is not a PAIR node"},
		{"127.0.0.1:51000", true, "loopback is always allowed"},
		{"[::1]:51000", true, "loopback over v6"},
		// A v4-mapped v6 remote must match the v4 address it was pushed as, or
		// a dual-stack peer is refused for a formatting reason.
		{"[::ffff:192.168.1.21]:51000", true, "v4-mapped form of a known peer"},
		// Regression: net.ParseIP returns nil for a zoned IPv6 literal, and the
		// first version of this gate fell through to "allow" on a parse
		// failure -- so any link-local LAN caller walked straight past it.
		{"[fe80::1cd4:beef:1:2%en0]:51000", false, "zoned link-local from a non-peer must be refused"},
		{"garbage", false, "an address we cannot classify is refused, not allowed"},
	} {
		if got := r.allows(tc.remote); got != tc.want {
			t.Errorf("allows(%q) = %v, want %v (%s)", tc.remote, got, tc.want, tc.why)
		}
	}

	// ...and the zoned form of an address that IS a peer must still match, or
	// dropping the zone would have traded one bug for another.
	r.set([]string{"fe80::1cd4:beef:1:2", "192.168.1.21"})
	if !r.allows("[fe80::1cd4:beef:1:2%en0]:51000") {
		t.Error("a known peer reached over zoned link-local must be allowed")
	}
	r.set([]string{"192.168.1.21", "10.0.0.7"})

	// An empty push is meaningful: this node knows of no peers, so only
	// loopback should get through.
	r.set(nil)
	if r.allows("192.168.1.21:51000") {
		t.Error("after an empty push, a former peer must lose access")
	}
	if !r.allows("127.0.0.1:51000") {
		t.Error("loopback must survive an empty push")
	}
}

// The gate must sit only on the plaintext path and must not weaken or bypass
// the cluster-gated one.
func TestHandlerRefusesUntrustedPlaintextReader(t *testing.T) {
	mesh := clustertrust.Open("") // not clustered: the mTLS gate is inert
	readers := newTrustedReaders()
	readers.set([]string{"192.168.1.21"})

	h := nodeInfoHandler(mesh, readers, func() []byte { return []byte(`{"ok":true}`) })

	for _, tc := range []struct {
		remote string
		want   int
	}{
		{"192.168.1.21:1234", http.StatusOK},
		{"127.0.0.1:1234", http.StatusOK},
		{"192.168.1.99:1234", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/node-info", nil)
		req.RemoteAddr = tc.remote
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != tc.want {
			t.Errorf("remote %s: status %d, want %d", tc.remote, rec.Code, tc.want)
		}
	}

	// A nil set means "no policy installed yet" -- the same fail-open as before
	// the first push -- and must NOT read as "skip the check entirely".
	open := nodeInfoHandler(mesh, nil, func() []byte { return []byte(`{"ok":true}`) })
	req := httptest.NewRequest(http.MethodGet, "/v1/node-info", nil)
	req.RemoteAddr = "192.168.1.99:1234"
	rec := httptest.NewRecorder()
	open(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with no reader set wired, status %d, want 200", rec.Code)
	}
}
