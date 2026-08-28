// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// A node whose appdata is wiped mints a fresh hostUuid but keeps its hostname
// and re-binds the same fixed service ports. These tests cover the two defences
// against the duplicate ("ghost") record that leaves behind: the liveness probe
// must notice the machine is no longer the node the record describes, and a
// record arriving under that hostname must collapse the one it replaced without
// waiting for the miss threshold.
//
// Both defences delete only on the same proof — a node-info answer naming a
// different host. The directory's hostname signal comes from an unauthenticated
// mDNS record, so it nominates suspects and nothing more; the address is not a
// signal at all, since machines on a direct-connect link legitimately share one.

// nodeInfoServer starts a /v1/node-info endpoint reporting hostUUID and returns
// its host and port. An empty hostUUID omits the field, mimicking a peer that
// predates node-info reporting identity.
func nodeInfoServer(t *testing.T, hostUUID string) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: hostUUID})
	}))
	t.Cleanup(srv.Close)
	return splitHostPort(t, strings.TrimPrefix(srv.URL, "http://"))
}

// openPort leaves a bare TCP listener open and returns its port. It stands in
// for a service that accepts connections but proves nothing about identity.
func openPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port := splitHostPort(t, ln.Addr().String())
	return port
}

// closedPort returns a port nothing is listening on, by binding and releasing
// it. Using a bound-then-released port rather than a low well-known one keeps
// the dial a prompt refusal on every platform.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port := splitHostPort(t, ln.Addr().String())
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}

// probeDaemon is the minimum daemon the liveness probe needs.
func probeDaemon() *daemon {
	return &daemon{http: &http.Client{Timeout: nodeInfoFetchTimeout}}
}

func rawNode(txt ...string) RawNode {
	return RawNode{ID: "wiped-host", Addresses: []string{"127.0.0.1"}, TXT: txt}
}

// TestReachableRejectsSupersededIdentity is the core of the fix. The ghost's
// cached address and ports are live — they belong to the machine's NEW
// incarnation — so a TCP-only probe reports the stale record alive forever. The
// probe must ask WHO answers, not just WHETHER anything answers.
func TestReachableRejectsSupersededIdentity(t *testing.T) {
	host, port := nodeInfoServer(t, "new-uuid-after-wipe")
	d := probeDaemon()

	n := rawNode("v=1", "uuid=old-uuid-before-wipe", "ip="+host, fmt.Sprintf("ni=%d", port))
	if d.reachable(n) {
		t.Fatal("a record whose address now answers with a DIFFERENT hostUuid must not be kept alive")
	}
}

// TestReachableAcceptsMatchingIdentity guards the other direction: a peer that
// merely dropped off multicast must survive the probe, or a transient mDNS gap
// would evict healthy nodes.
func TestReachableAcceptsMatchingIdentity(t *testing.T) {
	host, port := nodeInfoServer(t, "steady-uuid")
	d := probeDaemon()

	n := rawNode("v=1", "uuid=steady-uuid", "ip="+host, fmt.Sprintf("ni=%d", port))
	if !d.reachable(n) {
		t.Fatal("a record whose address confirms the same hostUuid must be kept alive")
	}
}

// TestReachableFallsBackWithoutIdentity covers every way identity can be
// unavailable. None of them may change today's behaviour: an open ni/em port
// still means alive, so no node that used to survive a probe starts evicting.
// Inference ports (ol/lm) are never used as the TCP fallback.
func TestReachableFallsBackWithoutIdentity(t *testing.T) {
	tcpPort := openPort(t)
	niHost, niPort := nodeInfoServer(t, "") // answers, but reports no identity

	cases := []struct {
		name string
		txt  []string
	}{
		{
			name: "no ni advertised",
			txt:  []string{"v=1", "uuid=some-uuid", "ip=127.0.0.1", fmt.Sprintf("em=%d", tcpPort)},
		},
		{
			name: "node-info not answering",
			txt: []string{"v=1", "uuid=some-uuid", "ip=127.0.0.1",
				fmt.Sprintf("ni=%d", closedPort(t)), fmt.Sprintf("em=%d", tcpPort)},
		},
		{
			name: "node-info reports no hostUuid",
			txt: []string{"v=1", "uuid=some-uuid", "ip=" + niHost,
				fmt.Sprintf("ni=%d", niPort), fmt.Sprintf("em=%d", tcpPort)},
		},
		{
			name: "record carries no uuid to compare",
			txt:  []string{"v=1", "ip=127.0.0.1", fmt.Sprintf("ni=%d", niPort), fmt.Sprintf("em=%d", tcpPort)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !probeDaemon().reachable(rawNode(tc.txt...)) {
				t.Fatal("with identity unavailable the probe must fall back to the TCP sweep")
			}
		})
	}
}

// TestReachableEvictsWhenNothingAnswers keeps the base case honest: a node that
// is genuinely gone still evicts.
func TestReachableEvictsWhenNothingAnswers(t *testing.T) {
	d := probeDaemon()
	dead := rawNode("v=1", "uuid=gone", "ip=127.0.0.1",
		fmt.Sprintf("ni=%d", closedPort(t)), fmt.Sprintf("ol=%d", closedPort(t)))
	if d.reachable(dead) {
		t.Fatal("a node answering on nothing must not be kept alive")
	}
	if d.reachable(rawNode("v=1", "uuid=identity-only", "ip=127.0.0.1")) {
		t.Fatal("a node advertising no services has nothing to probe and must evict")
	}
}

// countingTransport records how many HTTP requests the probe actually issues.
type countingTransport struct {
	n int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n++
	return http.DefaultTransport.RoundTrip(r)
}

// TestReachableSkipsIdentityWhenNothingAnswers pins the probe's ORDER, not just
// its verdicts. The identity read must run only when the TCP sweep already found
// the address answering — i.e. only when the node would otherwise have been
// kept, which is the sole case where the two need telling apart.
//
// Asking the other way round is what this guards against: it would spend a
// node-info timeout on every departed node, serially, inside the browser's scan
// cycle. The verdicts are identical either way, so nothing but a request count
// can catch a regression here.
func TestReachableSkipsIdentityWhenNothingAnswers(t *testing.T) {
	counter := &countingTransport{}
	d := &daemon{http: &http.Client{Transport: counter, Timeout: nodeInfoFetchTimeout}}

	dead := rawNode("v=1", "uuid=gone", "ip=127.0.0.1",
		fmt.Sprintf("ni=%d", closedPort(t)), fmt.Sprintf("ol=%d", closedPort(t)))
	if d.reachable(dead) {
		t.Fatal("a node answering on nothing must not be kept alive")
	}
	if counter.n != 0 {
		t.Fatalf("a departed node cost %d node-info request(s); the TCP sweep must settle it alone", counter.n)
	}

	// When the address DOES answer, the identity read is exactly what decides it.
	host, port := nodeInfoServer(t, "someone-else")
	live := rawNode("v=1", "uuid=old-uuid", "ip="+host, fmt.Sprintf("ni=%d", port))
	if d.reachable(live) {
		t.Fatal("an answering address reporting a different hostUuid must evict")
	}
	if counter.n != 1 {
		t.Fatalf("node-info requests = %d, want exactly 1 once the address answered", counter.n)
	}
}

// testNow is captured once so every ghost() in a run shares one base: LastSeen
// is Unix SECONDS, so computing it per call would let a wall-clock rollover
// between two calls shift the gap by a second and flip a threshold assertion.
var testNow = time.Now().Unix()

// named builds a directory entry with an explicit hostname; ghost is the common
// case where the hostname is the one a wipe would have preserved.
func named(uuid, name, ip string, ageSeconds int64) noderec.DirectoryNode {
	return noderec.DirectoryNode{
		HostUUID: uuid,
		Name:     name,
		IP:       ip,
		LastSeen: testNow - ageSeconds,
	}
}

func ghost(uuid, ip string, ageSeconds int64) noderec.DirectoryNode {
	return named(uuid, "wiped-host", ip, ageSeconds)
}

// withNodeInfo advertises an ni port on a directory entry — the only service the
// supersedence proof reads.
func withNodeInfo(n noderec.DirectoryNode, port int) noderec.DirectoryNode {
	n.Services = map[noderec.ServiceKey]noderec.ServiceStatus{
		noderec.ServiceNodeInfo: {Port: port},
	}
	return n
}

func candidateUUIDs(nodes []noderec.DirectoryNode) string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.HostUUID)
	}
	return strings.Join(ids, ",")
}

// TestSupersedeCandidatesNominatesGhost covers the nomination: when the wiped
// machine reappears under a new uuid at the same address and hostname, the
// record it replaced is put forward for confirmation — and left in place until
// it is confirmed.
func TestSupersedeCandidatesNominatesGhost(t *testing.T) {
	d := newDirectory()
	d.upsert(ghost("old-uuid", "192.168.1.10", 3600))

	candidates := d.supersedeCandidates(ghost("new-uuid", "192.168.1.10", 0), "self-uuid")
	if len(candidates) != 1 || candidates[0].HostUUID != "old-uuid" {
		t.Fatalf("candidates = %+v, want exactly the pre-wipe record", candidates)
	}
	if _, ok := d.get("old-uuid"); !ok {
		t.Fatal("nominating a record must not delete it; only proof may")
	}
}

// TestSupersedeCandidatesKeepFreshNeighbours is the false-positive guard: two
// records observed at about the same time are the shape a misconfiguration
// takes, not a wipe, and are not worth a probe.
func TestSupersedeCandidatesKeepFreshNeighbours(t *testing.T) {
	d := newDirectory()
	d.upsert(ghost("peer-a", "192.168.1.10", supersedeMinAge-1))

	candidates := d.supersedeCandidates(ghost("peer-b", "192.168.1.10", 0), "self-uuid")
	if len(candidates) != 0 {
		t.Fatalf("a gap under supersedeMinAge must not nominate: %+v", candidates)
	}
}

// TestSupersedeCandidatesRequireSameHostname keeps the nomination narrow. Two
// machines colliding on an address almost never also collide on a hostname, so a
// name mismatch means we are not looking at one machine. The wiped-AND-renamed
// case is left to the miss-threshold path, which reaches the same proof.
func TestSupersedeCandidatesRequireSameHostname(t *testing.T) {
	d := newDirectory()
	d.upsert(named("old-uuid", "old-name", "192.168.1.10", 3600))

	candidates := d.supersedeCandidates(named("new-uuid", "new-name", "192.168.1.10", 0), "self-uuid")
	if len(candidates) != 0 {
		t.Fatalf("a different hostname on the address must not be nominated: %+v", candidates)
	}
}

// TestSupersedeCandidatesIgnoreTheAddress covers the signal that was removed. A
// wiped machine keeps its hostname whether or not it comes back on the address
// it had before, so a stale namesake elsewhere is nominated too — and, in the
// other direction, sharing an address is no longer a reason to nominate
// anything, which is what stopped two hosts on a direct-connect link from
// putting each other forward.
func TestSupersedeCandidatesIgnoreTheAddress(t *testing.T) {
	d := newDirectory()
	d.upsert(ghost("peer-a", "192.168.1.10", 3600))

	candidates := d.supersedeCandidates(ghost("peer-b", "192.168.1.11", 0), "self-uuid")
	if len(candidates) != 1 || candidates[0].HostUUID != "peer-a" {
		t.Fatalf("candidates = %+v, want the stale namesake that moved", candidates)
	}

	// The arriving record must still carry an address: that is where the
	// confirming read is made, so without one there is nothing to ask.
	d2 := newDirectory()
	d2.upsert(ghost("peer-c", "192.168.1.10", 3600))
	if candidates := d2.supersedeCandidates(ghost("peer-d", "", 0), "self-uuid"); len(candidates) != 0 {
		t.Fatalf("an unaddressable arrival can prove nothing: %+v", candidates)
	}
}

// TestSupersedeCandidatesNeverIncludeSelf protects the local card. Self is
// registry-driven, so its LastSeen advances only when this node republishes and
// can legitimately look stale — it must not be displaceable by a peer claiming
// its address. Evicting the local node is a failure mode this daemon has hit
// before (see the self-guard in onBrowse).
func TestSupersedeCandidatesNeverIncludeSelf(t *testing.T) {
	d := newDirectory()
	d.upsert(ghost("self-uuid", "192.168.1.10", 3600))

	candidates := d.supersedeCandidates(ghost("peer-uuid", "192.168.1.10", 0), "self-uuid")
	if len(candidates) != 0 {
		t.Fatalf("self must never be nominated by a peer: %+v", candidates)
	}

	// Nor may the local record nominate peers on its way in.
	d2 := newDirectory()
	d2.upsert(ghost("peer-uuid", "192.168.1.10", 3600))
	if candidates := d2.supersedeCandidates(ghost("self-uuid", "192.168.1.10", 0), "self-uuid"); len(candidates) != 0 {
		t.Fatalf("self must not nominate peers: %+v", candidates)
	}
}

// TestSupersedeCandidatesAreDeterministic pins the order, since the caller turns
// the confirmed subset into a stream of node-removed notifications.
func TestSupersedeCandidatesAreDeterministic(t *testing.T) {
	d := newDirectory()
	d.upsert(ghost("uuid-c", "192.168.1.10", 3600))
	d.upsert(ghost("uuid-a", "192.168.1.10", 7200))
	d.upsert(ghost("uuid-b", "192.168.1.10", 5400))

	got := candidateUUIDs(d.supersedeCandidates(ghost("uuid-live", "192.168.1.10", 0), "self-uuid"))
	if want := "uuid-a,uuid-b,uuid-c"; got != want {
		t.Fatalf("candidate order = %v, want %v (sorted by hostUuid)", got, want)
	}
}

// TestUpsertEvictingRejudgesRecordsThatMoved covers the gap the proof opens: it
// is a network read, so the directory is unlocked while it runs and the entry
// under judgement can change underneath it. A verdict formed against a state
// that no longer exists must not be applied to the state that replaced it.
func TestUpsertEvictingRejudgesRecordsThatMoved(t *testing.T) {
	d := newDirectory()
	judged := named("old-uuid", "wiped-host", "192.168.1.10", 3600)
	d.upsert(judged)

	moved := judged
	moved.IP = "192.168.1.11"
	d.upsert(moved)

	arriving := named("new-uuid", "wiped-host", "192.168.1.10", 0)
	if evicted := d.upsertEvicting(arriving, []noderec.DirectoryNode{judged}); len(evicted) != 0 {
		t.Fatalf("a record that changed during the probe must be judged again, not evicted: %+v", evicted)
	}
	if _, ok := d.get("old-uuid"); !ok {
		t.Fatal("the re-addressed record must survive")
	}
	if _, ok := d.get("new-uuid"); !ok {
		t.Fatal("the arriving node must be stored either way")
	}
}

// TestSupersedingUpsertRequiresIdentityProof is the security guard. Address and
// hostname come from an unauthenticated mDNS record and LastSeen freezes for a
// healthy peer exactly as it does for a ghost, so collapsing on those alone
// would let anything on the LAN delete a live node by claiming its address —
// and the browser re-reports a node only when its record CHANGES, so the peer
// might never come back. Nothing is deleted without an answer from the machine.
func TestSupersedingUpsertRequiresIdentityProof(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "192.168.1.1")
	// Nothing is listening on the record's own ni port, so it cannot speak for
	// itself and the claimant's answer is the only evidence in play.
	d.dir.upsert(withNodeInfo(named("live-uuid", "wiped-host", loopbackHost, 3600), closedPort(t)))
	claim := named("claimant-uuid", "wiped-host", loopbackHost, 0)

	// Nothing to ask: the claimant advertises no node-info port.
	d.supersedingUpsert(claim)
	if _, ok := d.dir.get("live-uuid"); !ok {
		t.Fatal("an unprovable claim must never evict a peer: mDNS is not a removal primitive")
	}

	// Asked, but unanswerable — the same verdict.
	d.supersedingUpsert(withNodeInfo(claim, closedPort(t)))
	if _, ok := d.dir.get("live-uuid"); !ok {
		t.Fatal("an address that cannot answer proves nothing; the peer must survive")
	}

	// Answered, and the machine is still the node the record describes.
	_, alivePort := nodeInfoServer(t, "live-uuid")
	d.supersedingUpsert(withNodeInfo(claim, alivePort))
	if _, ok := d.dir.get("live-uuid"); !ok {
		t.Fatal("node-info confirming the record's own identity must keep it")
	}

	// Only a definite mismatch collapses the pair.
	_, wipedPort := nodeInfoServer(t, "claimant-uuid")
	d.supersedingUpsert(withNodeInfo(claim, wipedPort))
	if _, ok := d.dir.get("live-uuid"); ok {
		t.Fatal("node-info naming a different host is proof; the superseded record must go")
	}
	if _, ok := d.dir.get("claimant-uuid"); !ok {
		t.Fatal("the arriving node must be stored")
	}
}

// TestSupersedingUpsertKeepsARecordThatAnswersForItself is the case that made
// the address a liability. Two identically-provisioned machines wired back to
// back both advertise the link's address, so each looked like the other's ghost;
// the pair is now told apart by asking the older record's own endpoint who it
// is. A record that names itself is a live peer, not a leftover, whatever
// address the arriving node claims.
func TestSupersedingUpsertKeepsARecordThatAnswersForItself(t *testing.T) {
	host, livePort := nodeInfoServer(t, "peer-a")
	_, claimPort := nodeInfoServer(t, "peer-b")
	d := newSelfTestDaemon("self-uuid", "192.168.1.1")
	d.dir.upsert(withNodeInfo(named("peer-a", "dgx-station", host, 3600), livePort))

	d.supersedingUpsert(withNodeInfo(named("peer-b", "dgx-station", host, 0), claimPort))

	if _, ok := d.dir.get("peer-a"); !ok {
		t.Fatal("a record still answering with its own hostUuid is a live peer sharing an address, not a ghost")
	}
	if _, ok := d.dir.get("peer-b"); !ok {
		t.Fatal("the arriving node must be stored")
	}
}

// TestSupersedingUpsertKeepsAnUnaskableRecord covers the other half of that
// defence: with no ni port to ask, a live namesake and a leftover look
// identical, so the record stays and the miss-threshold path reaches it when it
// stops advertising.
func TestSupersedingUpsertKeepsAnUnaskableRecord(t *testing.T) {
	host, claimPort := nodeInfoServer(t, "claimant-uuid")
	d := newSelfTestDaemon("self-uuid", "192.168.1.1")
	d.dir.upsert(named("live-uuid", "wiped-host", host, 3600))

	d.supersedingUpsert(withNodeInfo(named("claimant-uuid", "wiped-host", host, 0), claimPort))

	if _, ok := d.dir.get("live-uuid"); !ok {
		t.Fatal("a record that cannot be asked who it is must never be evicted here")
	}
}

// TestOnBrowseEmitsSupersededRemoval covers the wiring: the daemon must tell the
// broker the ghost is gone, and drop its cached enrichment so a later
// rediscovery under that uuid re-probes instead of resurrecting stale metrics.
func TestOnBrowseEmitsSupersededRemoval(t *testing.T) {
	host, port := nodeInfoServer(t, "new-uuid")
	d := newSelfTestDaemon("self-uuid", "192.168.1.1")
	d.dir.upsert(withNodeInfo(ghost("old-uuid", host, 3600), closedPort(t)))
	d.lastInfo["old-uuid"] = NodeInfoResponse{HostUUID: "old-uuid"}

	d.onBrowse(DiscoveryEvent{
		Type: "discovered",
		Node: RawNode{
			ID:        "wiped-host",
			Addresses: []string{host},
			TXT:       []string{"v=1", "uuid=new-uuid", "ip=" + host, fmt.Sprintf("ni=%d", port)},
		},
	})

	if _, ok := d.dir.get("old-uuid"); ok {
		t.Fatal("onBrowse must collapse the record the arriving node is proven to replace")
	}
	if _, ok := d.dir.get("new-uuid"); !ok {
		t.Fatal("the arriving node must be stored")
	}
	d.infoMu.Lock()
	_, cached := d.lastInfo["old-uuid"]
	d.infoMu.Unlock()
	if cached {
		t.Fatal("a superseded node's enrichment cache must be dropped")
	}
}
