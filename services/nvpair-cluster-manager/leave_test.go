// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestHandleLeave_NotBlockedByUnreachableMember verifies that a member reachable
// at the TCP layer but never completing a request must
// not stall the whole leave for the full pairingHTTPTimeout (10s). The
// departure-tombstone pushes run concurrently under leaveNotifyTimeout, then
// teardown proceeds; the stuck peer converges later from the gossiped tombstone.
func TestHandleLeave_NotBlockedByUnreachableMember(t *testing.T) {
	m := newTestManager(t) // clustered as "cluster-1"

	// A listener that accepts but never responds, so the mTLS reconcile POST
	// hangs in the TLS handshake until the client's 10s timeout — exactly the
	// offline/wedged-peer case that used to hang the leave.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _, _ = io.Copy(io.Discard, c) }(c) // read and drop; never reply
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)

	uuid, cert, fp, _ := makeNode(t, "ghost")
	if err := m.trust.Pin(&TrustedPin{
		NodeUUID: uuid, NodeID: "ghost", Name: "ghost", ClusterID: "cluster-1",
		CertPem: cert, CertFingerprint: fp, PinnedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("pin ghost: %v", err)
	}
	m.upsertMember(&ClusterNode{
		NodeUUID: uuid, ID: "ghost",
		IPAddress: "127.0.0.1", Port: addr.Port, State: stateMember,
	})

	start := time.Now()
	m.handleLeave(&Message{})
	elapsed := time.Since(start)

	if elapsed > leaveNotifyTimeout+3*time.Second {
		t.Fatalf("handleLeave took %v; the bounded notify phase must not wait the full %v for a wedged member", elapsed, pairingHTTPTimeout)
	}
	if id, _ := m.clusterIdentity(); id != "" {
		t.Fatalf("after leave the node must be unclustered, got cluster id %q", id)
	}
}
