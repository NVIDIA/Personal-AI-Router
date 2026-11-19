// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/url"
	"testing"
)

// The joiner's Completion Exchange POST targets the inviter address carried in
// PairingInfo.Addr. When the inviter was reached over IPv6 link-local, that
// address is only dialable with its scope zone (#69).
func TestScopedHostPreservesZone(t *testing.T) {
	ua := &net.UDPAddr{IP: net.ParseIP("fe80::c16:bee4:c6a1:3fd"), Zone: "en0"}
	if got := scopedHost(ua); got != "fe80::c16:bee4:c6a1:3fd%en0" {
		t.Fatalf("scopedHost = %q, want the scope zone preserved", got)
	}
}

func TestScopedHostWithoutZoneUnchanged(t *testing.T) {
	for _, ip := range []string{"192.168.86.10", "fe80::1"} {
		ua := &net.UDPAddr{IP: net.ParseIP(ip)}
		if got := scopedHost(ua); got != ip {
			t.Errorf("scopedHost(%q) = %q, want unchanged", ip, got)
		}
	}
}

// outboundIP against loopback must keep returning the bare IP (no zone).
func TestOutboundIPLoopbackHasNoZone(t *testing.T) {
	if got := outboundIP("127.0.0.1:9"); got != "127.0.0.1" {
		t.Fatalf("outboundIP = %q, want 127.0.0.1", got)
	}
}

// A raw % in the URL host is rejected as an invalid URL escape, so the zone
// must be percent-encoded (RFC 6874) for the joiner's return POST to parse.
func TestPeerURLZoneEncoding(t *testing.T) {
	got := peerURL("http", "[fe80::c16:bee4:c6a1:3fd%en0]:14321", "/v1/cluster/pairing")
	want := "http://[fe80::c16:bee4:c6a1:3fd%25en0]:14321/v1/cluster/pairing"
	if got != want {
		t.Fatalf("peerURL = %q, want %q", got, want)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("peerURL output does not parse: %v", err)
	}
	if u.Hostname() != "fe80::c16:bee4:c6a1:3fd%en0" || u.Port() != "14321" {
		t.Fatalf("reparsed host = %q port = %q, zone lost", u.Hostname(), u.Port())
	}
}

func TestPeerURLPlainHostportUnchanged(t *testing.T) {
	got := peerURL("https", "192.168.86.10:14321", "/v1/cluster/members/remove")
	want := "https://192.168.86.10:14321/v1/cluster/members/remove"
	if got != want {
		t.Fatalf("peerURL = %q, want %q", got, want)
	}
}
