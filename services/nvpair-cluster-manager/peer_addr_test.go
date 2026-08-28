// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// unreachableAddr is in TEST-NET-1 (RFC 5737), which is reserved for
// documentation and routed nowhere, so a connection to it never completes.
const unreachableAddr = "192.0.2.1:14321"

// TestReconcileWithFailsOverToAnAnsweringAddress is the reported defect at the
// cluster layer: a peer's leading address was a direct-connect link this host
// could not reach, so every reconcile burned the full request timeout against it
// while the address that worked sat unused in the same candidate list.
func TestReconcileWithFailsOverToAnAnsweringAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mA := newTestManagerPort(t, 15031)
	mB := newTestManagerPort(t, 15032)
	go func() { _ = mA.runHTTP(ctx) }()
	go func() { _ = mB.runHTTP(ctx) }()
	time.Sleep(400 * time.Millisecond)

	pinTrusted(t, mA, mB.identity.NodeUUID, string(mB.identity.CertPEM), mB.identity.CertFingerprint)
	pinTrusted(t, mB, mA.identity.NodeUUID, string(mA.identity.CertPEM), mA.identity.CertFingerprint)
	mA.upsertMember(&ClusterNode{NodeUUID: mB.identity.NodeUUID, ID: "node-b", IPAddress: "127.0.0.1", Port: 15032, AdmissionEpoch: 1, State: stateMember})
	mB.upsertMember(&ClusterNode{NodeUUID: mA.identity.NodeUUID, ID: "node-a", IPAddress: "192.0.2.1", Port: 15031, AdmissionEpoch: 1, State: stateMember})

	working := net.JoinHostPort("127.0.0.1", strconv.Itoa(15031))
	outcome, _ := mB.reconcileWith([]string{unreachableAddr, working}, mA.identity.NodeUUID)
	if outcome != reconcileAccepted {
		t.Fatalf("reconcile outcome = %v, want accepted via the second candidate", outcome)
	}
}

// TestReconcileBudgetCoversTheWholeCandidateWalk: failover is worth one round
// trip, not one per address. Reconcile runs on a heartbeat and the self-removal
// verdict waits on every peer, so a peer whose four published addresses all
// blackhole must cost one budget rather than four.
func TestReconcileBudgetCoversTheWholeCandidateWalk(t *testing.T) {
	m := newTestManagerPort(t, 15041)
	peerUUID, certPEM, fingerprint, _ := makeNode(t, "192.0.2.9")
	pinTrusted(t, m, peerUUID, certPEM, fingerprint)

	blackholed := []string{"192.0.2.1:14321", "192.0.2.2:14321", "192.0.2.3:14321", "192.0.2.4:14321"}
	budget := 600 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	outcome, _ := m.reconcileWithin(ctx, blackholed, peerUUID)
	elapsed := time.Since(start)

	if outcome != reconcileUnreachable {
		t.Fatalf("outcome = %v, want unreachable", outcome)
	}
	// Generous headroom over the budget, but far below the four-times-the-budget
	// cost of a per-address deadline.
	if limit := 2 * budget; elapsed > limit {
		t.Fatalf("the walk took %v with a %v budget (limit %v); the budget is being spent per address", elapsed, budget, limit)
	}
}

// TestResolvePeerAddrsPutsTheRecordedAddressFirst: the pairing-time address was
// observed working from here, so it leads — but the peer's other advertised
// addresses follow it rather than being discarded.
func TestResolvePeerAddrsPutsTheRecordedAddressFirst(t *testing.T) {
	m := newTestManagerPort(t, 14321)
	peer := "33333333-3333-3333-3333-333333333333"
	m.browser = newBrowser()
	m.browser.setRelay([]noderec.DirectoryNode{{
		HostUUID: peer,
		Name:     "peer",
		IP:       "192.168.240.1",
		IPs:      []string{"192.168.240.1", "10.172.55.129"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCluster: {Port: 14321}},
	}})

	got := m.resolvePeerAddrs(ClusterNode{NodeUUID: peer, ID: "peer", IPAddress: "10.172.55.129", Port: 14321})
	want := []string{"10.172.55.129:14321", "192.168.240.1:14321"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("resolvePeerAddrs = %v, want %v", got, want)
	}
}

// TestRefreshMemberAddrsFromMDNSKeepsAStillAdvertisedAddress: the stored address
// was proven from this host, while the peer's own first choice is ranked from its
// vantage point, so a peer that still advertises what we hold must not be
// redirected onto a link only its neighbour can use.
func TestRefreshMemberAddrsFromMDNSKeepsAStillAdvertisedAddress(t *testing.T) {
	m := newTestManagerPort(t, 14321)
	peer := "44444444-4444-4444-4444-444444444444"
	m.upsertMember(&ClusterNode{NodeUUID: peer, ID: "peer", IPAddress: "10.172.55.129", Port: 14321, State: stateMember})
	m.browser = newBrowser()
	m.browser.setRelay([]noderec.DirectoryNode{{
		HostUUID: peer,
		Name:     "peer",
		IP:       "192.168.240.1",
		IPs:      []string{"192.168.240.1", "10.172.55.129"},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCluster: {Port: 14321}},
	}})

	m.refreshMemberAddrsFromMDNS()
	if n, _ := m.memberByNodeID(peer); n.IPAddress != "10.172.55.129" {
		t.Fatalf("stored address = %q, want it kept at 10.172.55.129", n.IPAddress)
	}

	// A peer that has stopped advertising the stored address really moved.
	m.browser.setRelay([]noderec.DirectoryNode{{
		HostUUID: peer,
		Name:     "peer",
		IP:       "10.172.55.200",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCluster: {Port: 14321}},
	}})
	m.refreshMemberAddrsFromMDNS()
	if n, _ := m.memberByNodeID(peer); n.IPAddress != "10.172.55.200" {
		t.Fatalf("stored address = %q, want the moved peer's 10.172.55.200", n.IPAddress)
	}
}

func TestConfirmedFirst(t *testing.T) {
	addrs := []string{"a:1", "b:1", "c:1"}
	if got := confirmedFirst(addrs, ""); got[0] != "a:1" {
		t.Fatalf("no confirmation should keep the ranking, got %v", got)
	}
	got := confirmedFirst(addrs, "c:1")
	want := []string{"c:1", "a:1", "b:1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("confirmedFirst = %v, want %v", got, want)
	}
}

// TestReachableEndpointFirstLeadsWithTheAddressThatAnswers: pairing is
// user-initiated and happens once, so it confirms the target rather than failing
// in front of the person who asked for it. It reorders only — an open port is not
// proof of which machine is behind it — so every address stays available.
func TestReachableEndpointFirstLeadsWithTheAddressThatAnswers(t *testing.T) {
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
			_ = c.Close()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	answering := net.JoinHostPort("127.0.0.1", portStr)

	got := reachableEndpointFirst([]string{unreachableAddr, answering})
	if len(got) != 2 || got[0] != answering || got[1] != unreachableAddr {
		t.Fatalf("reachableEndpointFirst = %v, want the answering address first and the other retained", got)
	}
}

// TestReachableEndpointFirstSingleCandidateSkipsConfirmation: with one address
// there is nothing to choose between, so pairing must not spend a handshake before
// its own request.
func TestReachableEndpointFirstSingleCandidateSkipsConfirmation(t *testing.T) {
	start := time.Now()
	got := reachableEndpointFirst([]string{unreachableAddr})
	if len(got) != 1 || got[0] != unreachableAddr {
		t.Fatalf("reachableEndpointFirst = %v, want [%s]", got, unreachableAddr)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("single candidate took %v, want no connect attempt", elapsed)
	}
}

// pairingProbeServer serves the pairing path with a fixed status and body, and
// counts the requests it received. It stands in for a machine that accepts a
// connection at a published address without being able to pair.
func pairingProbeServer(t *testing.T, status int, body string) (addr string, requests *atomic.Int64) {
	t.Helper()
	requests = &atomic.Int64{}
	mux := http.NewServeMux()
	mux.HandleFunc(pairingPath, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), requests
}

// publishPendingInvite records the pending outbound invite that handleInviteNode
// publishes before it starts walking addresses. The walk re-reads it between
// attempts, so a test that drives the walk directly has to supply it.
func publishPendingInvite(t *testing.T, m *Manager, inviteID string) {
	t.Helper()
	cid, friendly := m.clusterIdentity()
	m.putInvite(&Invite{
		InviteID:            inviteID,
		FromNodeID:          m.identity.NodeID,
		FromNodeUUID:        m.identity.NodeUUID,
		FromNodeName:        m.identity.Name,
		ClusterID:           cid,
		ClusterFriendlyName: friendly,
		State:               inviteStatePending,
		CreatedAt:           time.Now().UnixMilli(),
	})
}

// TestPairingWalksPastAnEndpointThatAnswersButCannotPair is the reported gap. A
// TCP handshake only proves a port is open: identically-wired machines genuinely
// share a direct-connect address, so the endpoint that answers can be the wrong
// machine, and the remaining published addresses must still be tried.
func TestPairingWalksPastAnEndpointThatAnswersButCannotPair(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	cid, _ := m.clusterIdentity()
	publishPendingInvite(t, m, "inv-walk")

	wrongMachine, wrongHits := pairingProbeServer(t, http.StatusInternalServerError, `{}`)
	alsoBroken, alsoHits := pairingProbeServer(t, http.StatusInternalServerError, `{}`)

	_, pin, err := m.pairAtFirstWorkingEndpoint("inv-walk", []string{wrongMachine, alsoBroken}, cid, m.sessGen.Load())
	if err == nil {
		t.Fatal("pairing reported success with every endpoint failing")
	}
	if pin != "" {
		t.Fatalf("pin = %q, want empty on failure", pin)
	}
	if got := wrongHits.Load(); got == 0 {
		t.Error("the first published address was never tried")
	}
	if got := alsoHits.Load(); got == 0 {
		t.Error("pairing stopped at the first address that answered; the second was never tried")
	}
}

// TestPairingStopsAtAnExplicitRefusal: a peer that answered and refused refuses at
// every address it has. Walking on would re-ask the same machine and report a
// transport failure over its actual answer.
func TestPairingStopsAtAnExplicitRefusal(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	cid, _ := m.clusterIdentity()
	publishPendingInvite(t, m, "inv-refused")

	refusing, refusedHits := pairingProbeServer(t, http.StatusConflict,
		`{"rejected":true,"reason":"already-clustered"}`)
	second, secondHits := pairingProbeServer(t, http.StatusInternalServerError, `{}`)

	_, _, err := m.pairAtFirstWorkingEndpoint("inv-refused", []string{refusing, second}, cid, m.sessGen.Load())
	var rejected *pairingRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v, want a pairing refusal", err)
	}
	if rejected.reason != "already-clustered" {
		t.Errorf("reason = %q, want already-clustered", rejected.reason)
	}
	if got := refusedHits.Load(); got == 0 {
		t.Error("the refusing address was never asked")
	}
	if got := secondHits.Load(); got != 0 {
		t.Errorf("a refusal must end the pairing, but %d further request(s) were made", got)
	}
}

// TestPairingDoesNotWalkAfterOurOwnClusterIsGone: no address of the peer's can fix
// a teardown on this side, and the abandoned pairing must stay abandoned.
func TestPairingDoesNotWalkAfterOurOwnClusterIsGone(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	cid, _ := m.clusterIdentity()
	publishPendingInvite(t, m, "inv-gone")

	first, firstHits := pairingProbeServer(t, http.StatusInternalServerError, `{}`)
	second, secondHits := pairingProbeServer(t, http.StatusInternalServerError, `{}`)

	// A stale session generation is what a teardown leaves behind.
	_, _, err := m.pairAtFirstWorkingEndpoint("inv-gone", []string{first, second}, cid, m.sessGen.Load()+1)
	if !errors.Is(err, errPairingClusterGone) {
		t.Fatalf("err = %v, want %v", err, errPairingClusterGone)
	}
	if got := firstHits.Load() + secondHits.Load(); got != 0 {
		t.Errorf("an abandoned pairing made %d request(s); want none", got)
	}
}

// TestPairingStopsWhenTheInviteIsCanceledMidWalk: a cancel that lands while one
// address is being tried must end the pairing, not merely that attempt. The walk
// previously read only the error, which does not describe a cancel, so it advanced
// to the next address and paired the peer the user had just stopped inviting.
func TestPairingStopsWhenTheInviteIsCanceledMidWalk(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	cid, _ := m.clusterIdentity()
	publishPendingInvite(t, m, "inv-cancel")

	// handleCancelInvite's authoritative section, run while the first address is
	// mid-exchange: the invite goes terminal and the running attempt's session
	// goes with it.
	canceling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		m.inviteMu.Lock()
		m.finishInvite("inv-cancel", inviteStateCanceled)
		m.deleteSession("inv-cancel")
		m.inviteMu.Unlock()
		http.Error(w, "{}", http.StatusInternalServerError)
	}))
	t.Cleanup(canceling.Close)
	next, nextHits := pairingProbeServer(t, http.StatusInternalServerError, `{}`)

	first := strings.TrimPrefix(canceling.URL, "http://")
	_, pin, err := m.pairAtFirstWorkingEndpoint("inv-cancel", []string{first, next}, cid, m.sessGen.Load())
	if !errors.Is(err, errPairingAbandoned) {
		t.Fatalf("err = %v, want %v", err, errPairingAbandoned)
	}
	if pin != "" {
		t.Fatalf("pin = %q, want none for a canceled invite", pin)
	}
	if got := nextHits.Load(); got != 0 {
		t.Errorf("the walk made %d request(s) to another address after the invite was canceled; want none", got)
	}
	if inv, ok := m.getInvite("inv-cancel"); !ok || inv.State != inviteStateCanceled {
		t.Fatalf("invite = %+v, want it left canceled", inv)
	}
}

// TestCanceledInviteRefusesLaterSessionRegistration covers the window after the
// walk's pending-state check but before the next address registers its session.
func TestCanceledInviteRefusesLaterSessionRegistration(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	cid, _ := m.clusterIdentity()
	publishPendingInvite(t, m, "inv-register")
	old := putInviterSession(t, m, "inv-register", cid)
	next := inviterSession("inv-register", cid)
	sessGen := m.sessGen.Load()

	m.inviteMu.Lock()
	started := make(chan struct{})
	registered := make(chan bool, 1)
	go func() {
		close(started)
		registered <- m.registerInviterSession(next, cid, sessGen)
	}()
	<-started
	m.finishInvite("inv-register", inviteStateCanceled)
	m.deleteSession(old.inviteID)
	m.inviteMu.Unlock()

	if <-registered {
		t.Fatal("a later address registered a session after the invite was canceled")
	}
	if _, live := m.getSession("inv-register"); live {
		t.Fatal("a canceled invite retained a pairing session")
	}
}

// TestCanceledInviteIsNotRepublishedByALaterAddress closes the same race one step
// later, where the cancel lands inside an attempt that then succeeds. The walk's
// next attempt registers a session of its own, so a liveness check alone passes;
// writing this attempt's PIN onto the invite captured before the exchange put the
// record back to pending with a fresh PIN, and the joiner could still complete.
func TestCanceledInviteIsNotRepublishedByALaterAddress(t *testing.T) {
	m := newTestManager(t)
	m.addSelfMember()
	cid, _ := m.clusterIdentity()
	publishPendingInvite(t, m, "inv-republish")

	m.inviteMu.Lock()
	m.finishInvite("inv-republish", inviteStateCanceled)
	m.deleteSession("inv-republish")
	m.inviteMu.Unlock()

	// The next address pairs, and registers cleanly: nothing about the session map
	// records that this invite was canceled.
	sess := putInviterSession(t, m, "inv-republish", cid)

	pin := "123456"
	m.inviteMu.Lock()
	recorded, applied := m.withPendingPairing("inv-republish", sess, func(cur *Invite) {
		cur.Pin = &pin
		m.putInvite(cur)
	})
	m.inviteMu.Unlock()

	if applied {
		t.Fatal("a canceled invite was republished by a later address's pairing")
	}
	if recorded != inviteStateCanceled {
		t.Fatalf("recorded state = %q, want %q reported back to the invite request", recorded, inviteStateCanceled)
	}
	inv, ok := m.getInvite("inv-republish")
	if !ok || inv.State != inviteStateCanceled {
		t.Fatalf("invite = %+v, want it left canceled", inv)
	}
	if inv.Pin != nil {
		t.Error("a PIN was published for a canceled invite")
	}
	if _, live := m.getSession("inv-republish"); live {
		t.Error("the pairing session outlived the canceled invite; a Completion could still be served")
	}
}
