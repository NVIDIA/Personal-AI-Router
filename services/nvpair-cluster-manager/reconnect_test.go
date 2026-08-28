// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestSetMemberAddr covers the address-update helper: a host-only update keeps
// the stored listening port, identical updates are no-ops, an explicit port is
// honored, and unknown/empty inputs change nothing.
func TestSetMemberAddr(t *testing.T) {
	m := newTestManager(t)
	uuid := "11111111-1111-1111-1111-111111111111"
	m.upsertMember(&ClusterNode{NodeUUID: uuid, ID: "n", IPAddress: "10.0.0.1", Port: 14321, State: stateMember})

	if !m.setMemberAddr(uuid, "192.168.1.50", 0) {
		t.Fatal("expected a change for a new host")
	}
	if n, _ := m.memberByNodeID(uuid); n.IPAddress != "192.168.1.50" || n.Port != 14321 {
		t.Fatalf("host-only update wrong: got %s:%d want 192.168.1.50:14321", n.IPAddress, n.Port)
	}
	if m.setMemberAddr(uuid, "192.168.1.50", 0) {
		t.Fatal("identical addr should not report a change")
	}
	if !m.setMemberAddr(uuid, "192.168.1.50", 14999) {
		t.Fatal("expected a change for a new port")
	}
	if n, _ := m.memberByNodeID(uuid); n.Port != 14999 {
		t.Fatalf("explicit port not applied: got %d want 14999", n.Port)
	}
	if m.setMemberAddr("does-not-exist", "1.2.3.4", 0) {
		t.Fatal("unknown uuid should not report a change")
	}
	if m.setMemberAddr(uuid, "", 0) {
		t.Fatal("empty host should not report a change")
	}
}

// TestHandleRosterLearnsSourceAddr drives the real mTLS roster endpoint over
// loopback: the server holds a STALE address for the client (as if the client
// moved since pairing), and after one reconcile it must have learned the
// client's real source IP from the authenticated connection while preserving
// the client's known listening port.
func TestHandleRosterLearnsSourceAddr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mA := newTestManagerPort(t, 15021)
	mB := newTestManagerPort(t, 15022)
	go func() { _ = mA.runHTTP(ctx) }()
	go func() { _ = mB.runHTTP(ctx) }()
	time.Sleep(400 * time.Millisecond)

	pinTrusted(t, mA, mB.identity.NodeUUID, string(mB.identity.CertPEM), mB.identity.CertFingerprint)
	pinTrusted(t, mB, mA.identity.NodeUUID, string(mA.identity.CertPEM), mA.identity.CertFingerprint)

	// mA's record for mB points at a stale IP; mB still reaches mA fine.
	mA.upsertMember(&ClusterNode{NodeUUID: mB.identity.NodeUUID, ID: "node-b", IPAddress: "10.9.9.9", Port: 15022, State: stateMember})
	mB.upsertMember(&ClusterNode{NodeUUID: mA.identity.NodeUUID, ID: "node-a", IPAddress: "127.0.0.1", Port: 15021, State: stateMember})

	mB.reconcileWith([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(15021))}, mA.identity.NodeUUID)

	n, ok := mA.memberByNodeID(mB.identity.NodeUUID)
	if !ok {
		t.Fatal("mA lost mB membership")
	}
	if n.IPAddress != "127.0.0.1" {
		t.Fatalf("mA did not learn mB's source IP: got %q want 127.0.0.1", n.IPAddress)
	}
	if n.Port != 15022 {
		t.Fatalf("mA must keep mB's listening port, not the ephemeral source port: got %d want 15022", n.Port)
	}
}

// TestRefreshMemberAddrsFromMDNS verifies the discovery backstop: a member
// whose stored address is stale is refreshed from the mDNS browse map, the
// pass is a safe no-op without a browser, and the self entry is never rewritten.
func TestRefreshMemberAddrsFromMDNS(t *testing.T) {
	m := newTestManager(t)
	peer := "22222222-2222-2222-2222-222222222222"
	m.upsertMember(&ClusterNode{NodeUUID: peer, ID: "peer", IPAddress: "10.9.9.9", Port: 14321, State: stateMember})

	// No browser configured yet: must not panic and must change nothing.
	m.refreshMemberAddrsFromMDNS()
	if n, _ := m.memberByNodeID(peer); n.IPAddress != "10.9.9.9" {
		t.Fatalf("addr changed without a browser: got %q", n.IPAddress)
	}

	m.browser = newBrowser()
	m.browser.seed(peer, "192.168.1.77", 14321)
	m.refreshMemberAddrsFromMDNS()
	if n, _ := m.memberByNodeID(peer); n.IPAddress != "192.168.1.77" {
		t.Fatalf("mDNS refresh did not update stored addr: got %q want 192.168.1.77", n.IPAddress)
	}

	// The self entry must never be redirected by (untrusted) mDNS.
	m.addSelfMember()
	m.browser.seed(m.identity.NodeUUID, "5.5.5.5", 14321)
	m.refreshMemberAddrsFromMDNS()
	if self, _ := m.memberByNodeID(m.identity.NodeUUID); self.IPAddress != "127.0.0.1" {
		t.Fatalf("self address must not be rewritten from mDNS: got %q", self.IPAddress)
	}
}
