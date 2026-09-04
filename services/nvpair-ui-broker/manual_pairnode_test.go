// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Folding a manually added PAIR node into the discovery relay.
//
// A peer reachable only across an overlay network such as a Tailscale tailnet is
// never discovered — no multicast crosses it — but it is not a different kind of
// node. Once its node-info reports a service map, the broker synthesizes the
// directory record the scanner would have produced, so both inference proxies,
// the scheduler, engine-manager's remote operations, the workload relay and the
// errors peer sync all see a pinned peer rather than a special case.

package main

import (
	"testing"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

func pairStatus() manualNodeStatus {
	principal := "peer-cluster-uuid"
	return manualNodeStatus{
		ID:           "gpu-box.tail1234.ts.net",
		Address:      "gpu-box.tail1234.ts.net",
		NodeInfoPort: 14318,
		HostUUID:     "peer-host-uuid",
		PairNode:     true,
		ClusterUUID:  &principal,
		Services: map[noderec.ServiceKey]int{
			noderec.ServiceNodeInfo:      14318,
			noderec.ServiceOllama:        11434,
			noderec.ServiceLMStudio:      1234,
			noderec.ServiceEngineManager: 14322,
			noderec.ServiceEngineControl: 14323,
			noderec.ServiceCluster:       14321,
		},
		Models:         []string{"llama3.2:latest"},
		ModelsByEngine: map[string][]string{"ollama": {"llama3.2:latest"}},
		TelemetryValid: true,
	}
}

func newPairPeerTestBroker(nodeID string) *Broker {
	return &Broker{
		nodeID:          nodeID,
		store:           newDiscoveryStore(),
		relayDir:        relay.NewDirectory(),
		manualRelayKeys: make(map[string]bool),
	}
}

func TestManualDirectoryNode_CarriesEverythingAPeerNeedsToBeRouted(t *testing.T) {
	b := newPairPeerTestBroker("this-host")
	s := pairStatus()

	node, ok := b.manualDirectoryNode(s, s.HostUUID)
	if !ok {
		t.Fatal("a PAIR node with a service map must enter the directory")
	}
	if node.HostUUID != "peer-host-uuid" {
		t.Errorf("hostUuid = %q, want the peer's identity", node.HostUUID)
	}
	// The typed address is this node's canonical one. It is a MagicDNS name here,
	// which is the case that used to be dropped: a name is dialable, and it
	// re-resolves, so it outlives every literal the peer holds.
	if node.IP != "gpu-box.tail1234.ts.net" {
		t.Errorf("ip = %q, want the address the operator typed", node.IP)
	}
	if got := node.CandidateIPs(); len(got) != 1 || got[0] != "gpu-box.tail1234.ts.net" {
		t.Errorf("candidate addresses = %v, want the typed address", got)
	}
	if got := node.AddressTXT(); len(got) != 1 || got[0] != "ip=gpu-box.tail1234.ts.net" {
		t.Errorf("address TXT = %v, want ip=<address>", got)
	}
	// The cluster principal is what a consumer pins the peer's certificate on.
	// Without it every mTLS surface is dialed in plaintext and answers 403.
	if node.ClusterUUID != "peer-cluster-uuid" || !node.Clustered() {
		t.Errorf("clusterUuid = %q, want the peer's principal", node.ClusterUUID)
	}
	for svc, want := range map[noderec.ServiceKey]int{
		noderec.ServiceOllama:        11434,
		noderec.ServiceLMStudio:      1234,
		noderec.ServiceEngineManager: 14322,
		noderec.ServiceEngineControl: 14323,
	} {
		got, ok := node.Services[svc]
		if !ok || got.Port != want {
			t.Errorf("service %s = %d (present:%v), want %d", svc, got.Port, ok, want)
		}
	}
	if len(node.Models) != 1 || node.Models[0] != "llama3.2:latest" {
		t.Errorf("models = %v, want the peer's inventory", node.Models)
	}
}

// A bare Ollama / LM Studio box has no proxy and no engine manager, so it is not
// a peer. It stays on the raw-engine bridge, which is what manual nodes were for.
func TestManualDirectoryNode_BareHostStaysOutOfTheDirectory(t *testing.T) {
	b := newPairPeerTestBroker("this-host")
	s := manualNodeStatus{
		ID: "10.0.0.9", Address: "10.0.0.9", HostUUID: "bare-host",
		OllamaUp: true, OllamaPort: 11434, OllamaModels: []string{"llama3.2:latest"},
	}
	if _, ok := b.manualDirectoryNode(s, s.HostUUID); ok {
		t.Fatal("a bare inference host must not be synthesized as a directory peer")
	}
}

// A manual entry naming this machine must never become a routing target. The
// test is identity, not address: the same host reached by its overlay name is
// still us, and an address comparison would miss it.
func TestManualDirectoryNode_RefusesThisHost(t *testing.T) {
	b := newPairPeerTestBroker("peer-host-uuid")
	if _, ok := b.manualDirectoryNode(pairStatus(), "peer-host-uuid"); ok {
		t.Fatal("a manual entry naming this host must not enter the directory")
	}
}

// When the daemon already holds the node, its record wins: it carries the peer's
// full ranked address list and its liveness probes, which this synthesis cannot.
func TestManualDirectoryNode_YieldsToTheScanner(t *testing.T) {
	b := newPairPeerTestBroker("this-host")
	b.store.Upsert(EnrichedNode{ID: "gpu-box", HostUUID: "peer-host-uuid"}, sourceScanner)
	if _, ok := b.manualDirectoryNode(pairStatus(), "peer-host-uuid"); ok {
		t.Fatal("the scanner's record is authoritative; the manual synthesis must stand down")
	}
}

func TestApplyManualDirectory_AddsAndWithdraws(t *testing.T) {
	b := newPairPeerTestBroker("this-host")
	s := pairStatus()

	b.applyManualDirectory(s, s.HostUUID)
	if got := b.relayDir.Snapshot(noderec.ServiceOllama); len(got) != 1 || got[0].HostUUID != "peer-host-uuid" {
		t.Fatalf("relay directory = %v, want the synthesized peer", got)
	}

	// It went down, or turned out not to be a PAIR node: withdraw it, or the
	// proxies keep a routing target nothing vouches for.
	down := s
	down.PairNode = false
	b.applyManualDirectory(down, s.HostUUID)
	if got := b.relayDir.Snapshot(""); len(got) != 0 {
		t.Fatalf("relay directory = %v, want the withdrawn peer gone", got)
	}
}

// The relay directory is keyed by hostUuid and the scanner writes the same keys.
// A withdrawal must remove only what this broker synthesized, or a live
// discovered peer disappears from every consumer until its next browse event.
func TestReleaseManualDirectory_NeverWithdrawsADiscoveredPeer(t *testing.T) {
	b := newPairPeerTestBroker("this-host")
	b.relayDir.Apply(noderec.NotifyNodeDiscovered, noderec.DirectoryNode{
		HostUUID: "discovered-peer",
		Name:     "gpu-box",
		IP:       "192.168.1.10",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
	})

	b.releaseManualDirectory("discovered-peer")
	if got := b.relayDir.Snapshot(""); len(got) != 1 {
		t.Fatalf("relay directory = %v, want the discovered peer untouched", got)
	}
}

// A machine the scanner took over mid-life must not be withdrawn either: the
// manual claim goes away, the daemon's record stays.
func TestReleaseManualDirectory_YieldsWhenTheScannerTookOver(t *testing.T) {
	b := newPairPeerTestBroker("this-host")
	s := pairStatus()
	b.applyManualDirectory(s, s.HostUUID)
	b.store.Upsert(EnrichedNode{ID: "gpu-box", HostUUID: s.HostUUID}, sourceScanner)

	b.releaseManualDirectory(s.HostUUID)
	if got := b.relayDir.Snapshot(""); len(got) != 1 {
		t.Fatalf("relay directory = %v, want the record kept for the scanner", got)
	}
}

func TestManualToEnriched_PairNodeCarriesItsInventoryAndMembership(t *testing.T) {
	en := manualToEnriched(pairStatus())
	if !en.Clustered {
		t.Error("a peer reporting a cluster principal must project as clustered")
	}
	if len(en.Models) != 1 || en.Models[0] != "llama3.2:latest" {
		t.Errorf("models = %v, want the peer's inventory", en.Models)
	}
	if len(en.Addresses) != 1 || en.Addresses[0] != "gpu-box.tail1234.ts.net" {
		t.Errorf("addresses = %v, want the typed address", en.Addresses)
	}
	if len(en.TXT) != 1 || en.TXT[0] != "ip=gpu-box.tail1234.ts.net" {
		t.Errorf("txt = %v, want ip=<address> so netpick ranks it", en.TXT)
	}
}
