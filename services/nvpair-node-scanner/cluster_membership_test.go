// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func newMembershipTestDaemon(clusterDir string) *daemon {
	mesh := clustertrust.Open(clusterDir)
	return &daemon{
		mesh:               mesh,
		modelsClients:      clustertrust.NewPeerClientPool(mesh, modelsFetchTimeout),
		clusterDir:         clusterDir,
		baseDir:            filepath.Dir(clusterDir),
		reg:                newRegistry("host-uuid", "", selfAddrs("127.0.0.1")),
		dir:                newDirectory(),
		lastInfo:           make(map[string]NodeInfoResponse),
		lastInfoAt:         make(map[string]time.Time),
		lastModels:         make(map[string][]string),
		lastModelsByEngine: make(map[string]map[string][]string),
		lastLoadedByEngine: make(map[string]map[string][]string),
		// codec/responder nil: emit is nil-safe; reloadIdentity skips re-advertise.
	}
}

// TestModelFetchTransportFollowsMembership pins the transport the daemon uses to
// read a model inventory. A peer's inventory is cluster data, so it is fetched
// only over mTLS pinned to that peer's principal; THIS node's own engine-manager
// is read over loopback in plaintext, which is what keeps a standalone machine's
// own model list working while it belongs to no cluster.
func TestModelFetchTransportFollowsMembership(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	d := newMembershipTestDaemon(clusterDir)
	d.modelsHTTP = &http.Client{}

	// Unclustered: loopback (self) still resolves a plain client...
	if _, scheme, ok := d.modelsClient("127.0.0.1", ""); !ok || scheme != "http" {
		t.Fatalf("unclustered loopback: scheme=%q ok=%v, want http/true", scheme, ok)
	}
	// ...but no LAN peer is reachable at all, in either transport.
	if _, _, ok := d.modelsClient("192.168.1.42", "principal-peer"); ok {
		t.Fatal("an unclustered node must not fetch a peer's model inventory")
	}
	if _, _, ok := d.modelsClient("192.168.1.42", ""); ok {
		t.Fatal("a peer advertising no cluster principal must not be fetched")
	}

	// Join a cluster that pins one peer.
	clustertrusttest.Join(t, clusterDir, "cluster-models", "principal-self", "principal-peer")
	d.mesh.Refresh()

	if _, scheme, ok := d.modelsClient("192.168.1.42", "principal-peer"); !ok || scheme != "https" {
		t.Fatalf("pinned peer: scheme=%q ok=%v, want https/true", scheme, ok)
	}
	if _, _, ok := d.modelsClient("192.168.1.99", "principal-stranger"); ok {
		t.Fatal("an unpinned peer must not be fetched")
	}
	// Self keeps its loopback plaintext path once clustered too.
	if _, scheme, ok := d.modelsClient("127.0.0.1", "principal-self"); !ok || scheme != "http" {
		t.Fatalf("clustered loopback: scheme=%q ok=%v, want http/true", scheme, ok)
	}
}

// TestClusterUUIDGatedOnLiveMembership pins the invite-deadlock fix at the
// scanner: cluster-uuid= is advertised only while actually a member, not because
// a keypair from a past membership is still on disk.
func TestClusterUUIDGatedOnLiveMembership(t *testing.T) {
	// A left/removed node: keypair present, but no active admission and no pins.
	// It must NOT advertise a cluster-uuid= (so peers see it as invitable).
	leftover := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.WriteKeypair(t, leftover, "principal-A")
	d := newMembershipTestDaemon(leftover)
	d.reloadIdentity()
	if got := d.reg.record().ClusterUUID; got != "" {
		t.Fatalf("leftover-keypair node advertised cluster-uuid=%q, want empty", got)
	}

	// An already-clustered node on restart: keypair + live admission on disk. It
	// must advertise its cluster principal so peers gate correctly.
	clustered := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.WriteKeypair(t, clustered, "principal-B")
	clustertrusttest.WriteAdmission(t, clustered, "cluster-xyz", 1)
	d2 := newMembershipTestDaemon(clustered)
	d2.reloadIdentity()
	if got := d2.reg.record().ClusterUUID; got != "principal-B" {
		t.Fatalf("clustered node advertised cluster-uuid=%q, want principal-B", got)
	}
}

// TestClusterUUIDAppearsOnJoinWithoutRestart is the advertisement half of the
// half-clustered regression guard. A scanner that started before this node had
// any cluster identity must begin advertising cluster-uuid= once the join lands
// on disk. Peers key their pins on that TXT value, so a scanner that could only
// learn it at startup left this node looking unclustered to the whole fleet:
// skipped by every peer's workload broadcast and dropped as a routing target,
// while still sitting in the roster.
func TestClusterUUIDAppearsOnJoinWithoutRestart(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	d := newMembershipTestDaemon(clusterDir)
	d.reloadIdentity()
	if got := d.reg.record().ClusterUUID; got != "" {
		t.Fatalf("pre-join cluster-uuid=%q, want empty", got)
	}

	// The cluster-manager mints the identity and activates the admission while
	// the scanner is already running.
	clustertrusttest.WriteKeypair(t, clusterDir, "principal-C")
	clustertrusttest.WriteAdmission(t, clusterDir, "cluster-xyz", 1)

	d.reloadIdentity()
	if got := d.reg.record().ClusterUUID; got != "principal-C" {
		t.Fatalf("post-join cluster-uuid=%q, want principal-C", got)
	}
}

// peerEntry is a directory entry for a peer already advertising a cluster
// principal — the shape a browse event produces for a node that is a member of a
// cluster we may or may not have joined yet.
func peerEntry(hostUUID, clusterUUID string) noderec.DirectoryNode {
	return noderec.DirectoryNode{
		HostUUID:    hostUUID,
		Name:        hostUUID,
		IP:          "192.0.2.10",
		ClusterUUID: clusterUUID,
		Services:    map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
	}
}

// TestReloadTrustAdoptsPeerPinnedAfterDiscovery is the regression guard for the
// reported failure, driven through the RPC entry point the broker calls.
//
// A peer is discovered while this node is still unclustered, so it is annotated
// untrusted — correctly, at that instant. The join then lands, but the peer's
// mDNS record never changes again, so no further browse event is produced and
// nothing re-asks the question. That entry stayed untrusted for the life of the
// process: both proxies dropped the peer as an unpinned relay peer, so the node
// listed only its own models and never routed inference anywhere, while its
// roster and pins on disk said it was a healthy member of a three-node cluster.
func TestReloadTrustAdoptsPeerPinnedAfterDiscovery(t *testing.T) {
	const peerUUID = "principal-peer"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	d := newMembershipTestDaemon(clusterDir)

	// Discovered before we hold any cluster identity.
	d.dir.upsert(toDirectoryNodeTrusted(peerEntry("peer-host", peerUUID), d))
	if n, _ := d.dir.get("peer-host"); n.Trusted {
		t.Fatal("peer annotated trusted while this node held no pins")
	}

	// The join lands on disk and the cluster-manager announces it. No browse
	// event accompanies it, and none ever will.
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)
	d.reloadTrust()

	if n, _ := d.dir.get("peer-host"); !n.Trusted {
		t.Fatal("peer still untrusted after its pin landed; routing would drop it")
	}
	// The same pass converges what we advertise, so peers can pin us back.
	if got := d.reg.record().ClusterUUID; got != "principal-self" {
		t.Fatalf("advertised cluster-uuid=%q, want principal-self", got)
	}

	// A removal is picked up just as promptly, and for the same reason.
	clustertrusttest.RemovePeerPin(t, clusterDir, peerUUID)
	d.reloadTrust()

	if n, _ := d.dir.get("peer-host"); n.Trusted {
		t.Fatal("peer still trusted after its pin was removed")
	}
}

// TestTrustReconcileIsSilentWhenNothingChanged keeps the reconcile from becoming
// an event source of its own: it runs every couple of seconds against every
// known node, so an unconditional write would push a directory-sized
// discovery:nodes snapshot through the broker to every subscriber on each pass.
func TestTrustReconcileIsSilentWhenNothingChanged(t *testing.T) {
	const peerUUID = "principal-peer"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self", peerUUID)
	d := newMembershipTestDaemon(clusterDir)

	d.dir.upsert(toDirectoryNodeTrusted(peerEntry("peer-host", peerUUID), d))
	d.dir.upsert(toDirectoryNodeTrusted(peerEntry("stranger", "principal-stranger"), d))

	for _, hostUUID := range []string{"peer-host", "stranger"} {
		if _, changed := d.dir.applyClusterIdentity(hostUUID, nil, d.mesh.HasPin); changed {
			t.Fatalf("%s reported a trust change on a steady-state pass", hostUUID)
		}
	}
}

// TestReconcileIdentityFollowsMembership covers the identity half of the same
// pass: it re-advertises only when live membership disagrees with what the
// registry is publishing, so the common case costs a string compare.
func TestReconcileIdentityFollowsMembership(t *testing.T) {
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	d := newMembershipTestDaemon(clusterDir)

	d.reconcileIdentity()
	if got := d.reg.record().ClusterUUID; got != "" {
		t.Fatalf("pre-join cluster-uuid=%q, want empty", got)
	}

	clustertrusttest.Join(t, clusterDir, "cluster-xyz", "principal-self")
	d.mesh.Refresh()
	d.reconcileIdentity()
	if got := d.reg.record().ClusterUUID; got != "principal-self" {
		t.Fatalf("post-join cluster-uuid=%q, want principal-self", got)
	}
}

// TestPeerLeavingItsClusterClearsInviteGate pins the directory half of the
// reported failure: a machine kept showing peers as belonging to another
// cluster, with invites disabled, after every one of them had left.
//
// A client suppresses an invite a clustered peer would reject, and it decides
// that from this annotation, which is only ever derived from the peer's own
// record. So an entry that keeps a cluster-uuid= the peer no longer advertises
// blocks a legitimate invite for as long as the entry survives — the annotation
// has to follow the record down as readily as it followed it up.
func TestPeerLeavingItsClusterClearsInviteGate(t *testing.T) {
	d := newMembershipTestDaemon(filepath.Join(t.TempDir(), "cluster"))
	const peerUUID = "peer-uuid"

	// The peer belongs to a cluster this node is not part of: not invitable.
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{
		ID:  "peer",
		TXT: []string{"v=1", "uuid=" + peerUUID, "cluster-uuid=their-cluster", "ip=192.168.1.9"},
	}})
	n, ok := d.dir.get(peerUUID)
	if !ok {
		t.Fatal("peer advertising a cluster principal was not recorded")
	}
	if !n.Clustered() {
		t.Fatalf("peer not annotated as clustered: %+v", n)
	}

	// It leaves, so it re-advertises without a cluster principal: invitable again.
	d.onBrowse(DiscoveryEvent{Type: "updated", Node: RawNode{
		ID:  "peer",
		TXT: []string{"v=1", "uuid=" + peerUUID, "ip=192.168.1.9"},
	}})
	n, ok = d.dir.get(peerUUID)
	if !ok {
		t.Fatal("peer dropped from the directory when it left its cluster")
	}
	if n.Clustered() {
		t.Errorf("peer still annotated as clustered (clusterUuid=%q) after it stopped advertising one", n.ClusterUUID)
	}
}

// toDirectoryNodeTrusted annotates an entry the way onBrowse does at the moment
// it is folded in, so a test can reproduce "discovered at time T" faithfully.
func toDirectoryNodeTrusted(n noderec.DirectoryNode, d *daemon) noderec.DirectoryNode {
	n.Trusted = d.mesh.HasPin(n.ClusterUUID)
	return n
}

func mustClusterUUID(t *testing.T, d *daemon, hostUUID string) string {
	t.Helper()
	n, ok := d.dir.get(hostUUID)
	if !ok {
		t.Fatalf("node %q not in directory", hostUUID)
	}
	return n.ClusterUUID
}
