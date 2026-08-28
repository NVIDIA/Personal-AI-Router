// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

// The mDNS-independent cluster-membership half of the periodic refresh loop
// (daemon.refreshClusterIdentityOnce). These tests drive it against a stub
// /v1/node-info server, with no mDNS anywhere: the whole point is that a peer's
// membership converges without a browse event ever firing.

// nodeInfoStub is a swappable /v1/node-info handler. `body` is the raw JSON so a
// test can control exactly what the peer reports — including omitting clusterUuid
// entirely, which is how a node too old to know about the field answers.
type nodeInfoStub struct {
	mu     sync.Mutex
	status int
	body   string
}

func (s *nodeInfoStub) set(status int, body string) {
	s.mu.Lock()
	s.status = status
	s.body = body
	s.mu.Unlock()
}

func (s *nodeInfoStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		status, body := s.status, s.body
		s.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func startNodeInfoStub(t *testing.T, body string) (*nodeInfoStub, int) {
	t.Helper()
	stub := &nodeInfoStub{}
	stub.set(http.StatusOK, body)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	return stub, portFromURL(t, srv.URL)
}

// newIdentityRefreshTestDaemon builds a daemon wired for the node-info refresh
// path only: a real directory and plain node-info client, an unclustered mesh (so
// it holds no pins, like a machine that has left), and no codec (emit is
// nil-safe). The registry self uuid is one no peer test seeds, so every seeded
// node is treated as a peer.
func newIdentityRefreshTestDaemon() *daemon {
	return &daemon{
		reg:                newRegistry("self-node-uuid", "", selfAddrs("127.0.0.1")),
		dir:                newDirectory(),
		mesh:               clustertrust.Open(""),
		http:               &http.Client{Timeout: nodeInfoFetchTimeout},
		lastInfo:           make(map[string]NodeInfoResponse),
		lastModels:         make(map[string][]string),
		lastModelsByEngine: make(map[string]map[string][]string),
		lastLoadedByEngine: make(map[string]map[string][]string),
	}
}

// seedPeer puts a peer in the directory advertising node-info at port, annotated
// as belonging to clusterUUID — the state a browse event would have left behind.
func seedPeer(d *daemon, hostUUID, clusterUUID string, niPort int) {
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID:    hostUUID,
		Name:        hostUUID,
		IP:          "127.0.0.1",
		ClusterUUID: clusterUUID,
		Trusted:     clusterUUID != "",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: niPort},
		},
	})
}

// TestDepartedPeerConvergesWithoutMDNS is the regression guard for the reported
// failure: after every peer left the cluster, one machine went on showing them as
// belonging to another cluster with invites disabled, while the rest of the fleet
// showed them as available.
//
// Membership reaches this daemon as the cluster-uuid= key on a peer's mDNS record,
// read only when a browse *change* event fires. The peer left and re-advertised,
// but this host did not receive it — and since the peer was still reachable, the
// liveness probe kept the frozen entry alive rather than letting it be
// rediscovered. Nothing asked again, so the principal outlived the cluster.
//
// No browse event occurs anywhere in this test. The peer's own HTTP report has to
// be enough.
func TestDepartedPeerConvergesWithoutMDNS(t *testing.T) {
	d := newIdentityRefreshTestDaemon()
	const peerUUID = "peer-uuid"

	// The peer has left, so it now reports no cluster principal...
	_, niPort := startNodeInfoStub(t, `{"GPUs":[],"hostUuid":"peer-uuid","clusterUuid":""}`)
	// ...but our entry still carries the one it advertised before it left.
	seedPeer(d, peerUUID, "their-old-principal", niPort)

	d.refreshClusterIdentityOnce()

	n, ok := d.dir.get(peerUUID)
	if !ok {
		t.Fatal("peer dropped from the directory by the refresh")
	}
	if n.Clustered() {
		t.Errorf("peer still annotated as clustered (clusterUuid=%q) after reporting it left", n.ClusterUUID)
	}
	if n.Trusted {
		t.Error("peer still annotated as trusted after leaving the cluster")
	}
	// A second pass agrees with the first and stays silent.
	if d.refreshNodeClusterIdentity(peerUUID, "127.0.0.1", niPort) {
		t.Error("refresh emitted again for an unchanged peer")
	}
}

func TestClusterIdentityRefreshFallsBackToASecondPublishedAddress(t *testing.T) {
	d := newIdentityRefreshTestDaemon()
	const peerUUID = "peer-multihomed"
	_, niPort := startNodeInfoStub(t, `{"GPUs":[],"hostUuid":"peer-multihomed","clusterUuid":""}`)
	seedPeer(d, peerUUID, "stale-principal", niPort)

	if !d.refreshNodeClusterIdentityCandidates(peerUUID, []string{"127.0.0.2", "127.0.0.1"}, niPort) {
		t.Fatal("membership did not refresh through the second published address")
	}
	n, ok := d.dir.get(peerUUID)
	if !ok || n.Clustered() {
		t.Fatalf("peer after refresh = %+v, want unclustered", n)
	}
}

// TestPeerInForeignClusterStaysSuppressed is the other half of the contract, and
// the reason the refresh trusts only an explicit report: a peer that is genuinely
// in a cluster we are not part of must keep its annotation, so the invite stays
// suppressed rather than being offered and rejected.
func TestPeerInForeignClusterStaysSuppressed(t *testing.T) {
	d := newIdentityRefreshTestDaemon()
	const peerUUID = "peer-uuid"

	_, niPort := startNodeInfoStub(t, `{"GPUs":[],"hostUuid":"peer-uuid","clusterUuid":"their-principal"}`)
	seedPeer(d, peerUUID, "", niPort)

	d.refreshClusterIdentityOnce()

	n, _ := d.dir.get(peerUUID)
	if !n.Clustered() {
		t.Fatal("peer reporting a cluster principal was not annotated as clustered")
	}
	if n.ClusterUUID != "their-principal" {
		t.Errorf("clusterUuid = %q, want their-principal", n.ClusterUUID)
	}
	// We hold no pin for it, so it is clustered but not trusted.
	if n.Trusted {
		t.Error("peer in a cluster we hold no pin for was annotated trusted")
	}
}

// TestPeerWithoutClusterFieldIsLeftAlone pins the version-skew rule. A node too
// old to report clusterUuid says nothing about its membership, and silence must
// not be read as "unclustered" — doing so would mark a clustered peer invitable,
// the exact failure the annotation prevents.
func TestPeerWithoutClusterFieldIsLeftAlone(t *testing.T) {
	d := newIdentityRefreshTestDaemon()
	const peerUUID = "peer-uuid"

	_, niPort := startNodeInfoStub(t, `{"GPUs":[],"hostUuid":"peer-uuid"}`)
	seedPeer(d, peerUUID, "their-principal", niPort)

	if d.refreshNodeClusterIdentity(peerUUID, "127.0.0.1", niPort) {
		t.Error("refresh acted on a peer that reported no clusterUuid field")
	}
	n, _ := d.dir.get(peerUUID)
	if n.ClusterUUID != "their-principal" {
		t.Errorf("clusterUuid = %q, want the prior value kept for a peer that reported nothing", n.ClusterUUID)
	}
}

// TestUnreachablePeerKeepsItsAnnotation covers the same rule for a failed fetch:
// no answer is not an answer.
func TestUnreachablePeerKeepsItsAnnotation(t *testing.T) {
	d := newIdentityRefreshTestDaemon()
	const peerUUID = "peer-uuid"

	stub, niPort := startNodeInfoStub(t, `{"GPUs":[],"clusterUuid":""}`)
	stub.set(http.StatusInternalServerError, "")
	seedPeer(d, peerUUID, "their-principal", niPort)

	if d.refreshNodeClusterIdentity(peerUUID, "127.0.0.1", niPort) {
		t.Error("refresh acted on a failed node-info fetch")
	}
	n, _ := d.dir.get(peerUUID)
	if n.ClusterUUID != "their-principal" {
		t.Errorf("clusterUuid = %q, want the prior value kept across a fetch failure", n.ClusterUUID)
	}
}

// TestSelfExcludedFromIdentityRefresh keeps the sweep off this node's own entry:
// publishSelf owns the local principal from the registry, and a loopback HTTP read
// must not become a second source for it.
func TestSelfExcludedFromIdentityRefresh(t *testing.T) {
	d := newIdentityRefreshTestDaemon()
	self := d.reg.record().HostUUID

	// Self's node-info would report unclustered, but the registry says otherwise.
	_, niPort := startNodeInfoStub(t, `{"GPUs":[],"clusterUuid":""}`)
	seedPeer(d, self, "our-principal", niPort)

	d.refreshClusterIdentityOnce()

	n, _ := d.dir.get(self)
	if n.ClusterUUID != "our-principal" {
		t.Errorf("self clusterUuid = %q, want the registry-owned value untouched", n.ClusterUUID)
	}
}

// TestStrangerAtTheAddressIsNotApplied covers the case where the machine
// answering is not the one the entry describes — a wiped and reinstalled node, or
// a reused DHCP lease. node-info names who it really is, so its membership must
// not be written onto the previous occupant's entry. That entry ages out through
// the liveness probe's identity check instead, which owns the decision.
func TestStrangerAtTheAddressIsNotApplied(t *testing.T) {
	d := newIdentityRefreshTestDaemon()
	const peerUUID = "peer-uuid"

	// A different machine now answers here, and it belongs to no cluster.
	_, niPort := startNodeInfoStub(t, `{"GPUs":[],"hostUuid":"someone-else","clusterUuid":""}`)
	seedPeer(d, peerUUID, "their-principal", niPort)

	if d.refreshNodeClusterIdentity(peerUUID, "127.0.0.1", niPort) {
		t.Error("applied membership reported by a different host")
	}
	n, _ := d.dir.get(peerUUID)
	if n.ClusterUUID != "their-principal" {
		t.Errorf("clusterUuid = %q, want the entry left for the liveness probe to age out", n.ClusterUUID)
	}

	// A peer that reports no hostUuid predates the field and is taken at its word.
	_, oldPort := startNodeInfoStub(t, `{"GPUs":[],"clusterUuid":""}`)
	seedPeer(d, "old-peer-uuid", "their-principal", oldPort)
	if !d.refreshNodeClusterIdentity("old-peer-uuid", "127.0.0.1", oldPort) {
		t.Error("skipped a peer that reported no hostUuid; it cannot be identity-checked either way")
	}
}

// TestSweepAnnotatesEveryPeerAgainstThePinSet exercises the sweep the way it
// actually runs: several peers at once, against a mesh holding a real pin. It
// covers the three answers a peer can give in one pass, and puts concurrent load
// on the fan-out so -race has something to observe.
func TestSweepAnnotatesEveryPeerAgainstThePinSet(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	// This node is in a cluster and holds a pin for one of the peers below.
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", "pinned-principal")

	d := newIdentityRefreshTestDaemon()
	d.mesh = clustertrust.Open(clusterDir)

	// A fellow member, a machine in someone else's cluster, and one that has left.
	for _, peer := range []struct {
		uuid     string
		reported string
	}{
		{"member-uuid", "pinned-principal"},
		{"stranger-uuid", "foreign-principal"},
		{"departed-uuid", ""},
	} {
		_, niPort := startNodeInfoStub(t, `{"GPUs":[],"clusterUuid":"`+peer.reported+`"}`)
		// Seed each one annotated wrongly, so every case has to be corrected.
		seedPeer(d, peer.uuid, "stale-principal", niPort)
	}

	d.refreshClusterIdentityOnce()

	member, _ := d.dir.get("member-uuid")
	if member.ClusterUUID != "pinned-principal" || !member.Trusted {
		t.Errorf("member = (clusterUuid=%q, trusted=%v), want the pinned principal and trusted",
			member.ClusterUUID, member.Trusted)
	}
	stranger, _ := d.dir.get("stranger-uuid")
	if stranger.ClusterUUID != "foreign-principal" || stranger.Trusted {
		t.Errorf("stranger = (clusterUuid=%q, trusted=%v), want its own principal and untrusted",
			stranger.ClusterUUID, stranger.Trusted)
	}
	departed, _ := d.dir.get("departed-uuid")
	if departed.Clustered() || departed.Trusted {
		t.Errorf("departed = (clusterUuid=%q, trusted=%v), want cleared",
			departed.ClusterUUID, departed.Trusted)
	}
}

// TestPinSetPassCannotWriteBackAPrincipal guards the interaction between the two
// membership passes. Both write the same two fields and they run concurrently: the
// pin-set pass on the broker's reload-trust announcement, the sweep on its own
// tick. The pin-set pass knows the pin set but not what any peer advertises, so it
// supplies no principal and trust is derived from whatever is stored when its write
// lands. Were it to pass a principal read earlier — from the snapshot it iterates —
// a sweep landing in between would be silently reverted and the invite gate would
// flip in the UI.
//
// Asserted at the write path rather than by racing the two passes, so it fails
// deterministically: a nil principal must leave the stored one alone, and the trust
// it derives must come from that stored value.
func TestPinSetPassCannotWriteBackAPrincipal(t *testing.T) {
	dir := newDirectory()
	dir.upsert(noderec.DirectoryNode{HostUUID: "peer", ClusterUUID: "current-principal"})

	// Stands in for the sweep having just moved the principal: the pin-set pass
	// holds a pin for the current value and none for what it used to be.
	trustFor := func(clusterUUID string) bool { return clusterUUID == "current-principal" }

	n, changed := dir.applyClusterIdentity("peer", nil, trustFor)
	if !changed {
		t.Fatal("pin-set pass reported no change, want the trust annotation derived")
	}
	if n.ClusterUUID != "current-principal" {
		t.Errorf("clusterUuid = %q, want the stored value untouched by a pass that supplied none", n.ClusterUUID)
	}
	if !n.Trusted {
		t.Error("trust was not derived from the stored principal")
	}

	// And a pass that does know the principal still governs it.
	moved := "moved-principal"
	n, _ = dir.applyClusterIdentity("peer", &moved, trustFor)
	if n.ClusterUUID != "moved-principal" || n.Trusted {
		t.Errorf("after an explicit move = (clusterUuid=%q, trusted=%v), want the new principal and untrusted",
			n.ClusterUUID, n.Trusted)
	}
}

// TestNodeInfoResponseDistinguishesAbsentFromEmpty pins the wire contract the
// version-skew rule depends on: the pointer has to survive decoding so absent and
// present-but-empty stay distinguishable.
func TestNodeInfoResponseDistinguishesAbsentFromEmpty(t *testing.T) {
	var absent NodeInfoResponse
	if err := json.Unmarshal([]byte(`{"GPUs":[]}`), &absent); err != nil {
		t.Fatalf("decode absent: %v", err)
	}
	if absent.ClusterUUID != nil {
		t.Errorf("absent clusterUuid decoded to %q, want nil", *absent.ClusterUUID)
	}
	if absent.TelemetryValid || absent.MSSince != 0 {
		t.Errorf("absent telemetry fields decoded as valid: %+v", absent)
	}

	var empty NodeInfoResponse
	if err := json.Unmarshal([]byte(`{"GPUs":[],"clusterUuid":"","telemetryValid":true,"msSince":137}`), &empty); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if empty.ClusterUUID == nil {
		t.Fatal("present-but-empty clusterUuid decoded to nil, want a non-nil empty string")
	}
	if *empty.ClusterUUID != "" {
		t.Errorf("empty clusterUuid = %q", *empty.ClusterUUID)
	}
	if !empty.TelemetryValid || empty.MSSince != 137 {
		t.Errorf("telemetry fields = valid:%v age:%d, want true/137", empty.TelemetryValid, empty.MSSince)
	}
}
