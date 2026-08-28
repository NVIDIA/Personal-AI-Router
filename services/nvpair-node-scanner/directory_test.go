// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

func TestToDirectoryNode(t *testing.T) {
	raw := RawNode{
		ID:        "hostA",
		Host:      "hostA.local.",
		Addresses: []string{"10.221.0.9", "192.168.1.10"},
		TXT:       []string{"v=1", "uuid=host-uuid", "cluster-uuid=clu", "ip=192.168.1.10", "ni=14318", "ol=11434"},
	}
	n, ok := toDirectoryNode(raw, true)
	if !ok {
		t.Fatal("a record with uuid= should project")
	}
	if n.HostUUID != "host-uuid" {
		t.Errorf("HostUUID = %q, want host-uuid", n.HostUUID)
	}
	if n.Name != "hostA" {
		t.Errorf("Name = %q, want hostA (instance)", n.Name)
	}
	if n.IP != "192.168.1.10" {
		t.Errorf("IP = %q, want the ip= TXT value", n.IP)
	}
	if !n.Trusted {
		t.Error("Trusted should reflect the caller-supplied flag")
	}
	if !n.HasService(noderec.ServiceNodeInfo) || !n.HasService(noderec.ServiceOllama) {
		t.Errorf("services wrong: %+v", n.Services)
	}
	if n.Services[noderec.ServiceOllama].Port != 11434 {
		t.Errorf("ol port = %d", n.Services[noderec.ServiceOllama].Port)
	}
}

// TestToDirectoryNodeIgnoresSRVPort asserts the locked-decision contract: the
// SRV port on a browsed _nvpair-node record is a fixed, NON-authoritative
// constant that consumers MUST ignore — every service port comes only from the
// TXT map. Here the record's SRV port is a bogus value while TXT says ni=14318,
// and the directory entry must report the TXT port, never the SRV one.
func TestToDirectoryNodeIgnoresSRVPort(t *testing.T) {
	raw := RawNode{
		ID:        "hostC",
		Host:      "hostC.local.",
		Port:      65000, // bogus SRV port — must be ignored
		Addresses: []string{"192.168.1.30"},
		TXT:       []string{"v=1", "uuid=host-c", "ip=192.168.1.30", "ni=14318", "ol=11434"},
	}
	n, ok := toDirectoryNode(raw, false)
	if !ok {
		t.Fatal("a record with uuid= should project")
	}
	if got := n.Services[noderec.ServiceNodeInfo].Port; got != 14318 {
		t.Errorf("ni port = %d, want 14318 from TXT (SRV port 65000 must be ignored)", got)
	}
	if got := n.Services[noderec.ServiceOllama].Port; got != 11434 {
		t.Errorf("ol port = %d, want 11434 from TXT", got)
	}
	for svc, st := range n.Services {
		if st.Port == 65000 {
			t.Errorf("the non-authoritative SRV port leaked into service %q", svc)
		}
	}
}

// TestToDirectoryNodeSkipsWithoutUUID: a record with no uuid= can't be keyed by
// a stable identity, so it's skipped (ok=false) rather than keyed by the
// hostname — node identity is the UUID everywhere, no name fallback.
func TestToDirectoryNodeSkipsWithoutUUID(t *testing.T) {
	raw := RawNode{
		ID:        "hostB",
		Addresses: []string{"10.221.0.9", "192.168.1.20"},
		TXT:       []string{"v=1", "er=14319"},
	}
	if _, ok := toDirectoryNode(raw, false); ok {
		t.Error("a record without uuid= must be skipped, not projected")
	}
}

// TestToDirectoryNodeIPFallback: with a uuid= but no ip=, every advertised address
// becomes a candidate and the canonical one is the ranker's head. The private
// blocks tie, so this is an arbitrary but stable choice — deliberately so. Which
// private block a node's real network uses is not knowable from the address, and
// the previous preference for 192.168 over 10.x is what made a multi-homed host
// publish a two-host direct-connect link instead of its LAN. Both addresses are
// kept, and a consumer that must connect resolves it by connecting.
func TestToDirectoryNodeIPFallback(t *testing.T) {
	raw := RawNode{
		ID:        "hostB",
		Addresses: []string{"10.221.0.9", "192.168.1.20"},
		TXT:       []string{"v=1", "uuid=host-b", "er=14319"},
	}
	n, ok := toDirectoryNode(raw, false)
	if !ok {
		t.Fatal("a record with uuid= should project")
	}
	if n.IP != "10.221.0.9" {
		t.Errorf("IP fallback = %q, want 10.221.0.9 (top-ranked)", n.IP)
	}
	if len(n.IPs) != 2 || n.IPs[0] != "10.221.0.9" || n.IPs[1] != "192.168.1.20" {
		t.Errorf("candidates = %v, want both advertised addresses ranked", n.IPs)
	}
	if n.Clustered() {
		t.Error("no cluster-uuid should mean not clustered")
	}
}

// TestToDirectoryNodePreservesPublishedOrder: a node's own ips= order survives
// projection, and its ip= stays canonical even though the advertised address list
// leads with something else. This is the multi-homed fix at the collapse point —
// the node ranked these with evidence no observer has.
func TestToDirectoryNodePreservesPublishedOrder(t *testing.T) {
	raw := RawNode{
		ID:        "spark",
		Addresses: []string{"192.168.240.2", "10.172.54.70"},
		TXT: []string{
			"v=1", "uuid=spark-1", "ip=10.172.54.70",
			"ips=10.172.54.70,192.168.240.2", "ni=14318",
		},
	}
	n, ok := toDirectoryNode(raw, false)
	if !ok {
		t.Fatal("a record with uuid= should project")
	}
	if n.IP != "10.172.54.70" {
		t.Errorf("canonical = %q, want the node's own ip= 10.172.54.70", n.IP)
	}
	if len(n.IPs) != 2 || n.IPs[0] != "10.172.54.70" || n.IPs[1] != "192.168.240.2" {
		t.Errorf("candidates = %v, want the node's published order", n.IPs)
	}
}

// TestToDirectoryNodeUnionsUnpublishedAddresses: an address the browse resolved
// but the node did not rank is kept as a fallback, appended rather than promoted.
func TestToDirectoryNodeUnionsUnpublishedAddresses(t *testing.T) {
	raw := RawNode{
		ID:        "hostC",
		Addresses: []string{"10.0.9.9"},
		TXT:       []string{"v=1", "uuid=host-c", "ip=10.0.0.5", "ips=10.0.0.5"},
	}
	n, ok := toDirectoryNode(raw, false)
	if !ok {
		t.Fatal("a record with uuid= should project")
	}
	want := []string{"10.0.0.5", "10.0.9.9"}
	if len(n.IPs) != len(want) || n.IPs[0] != want[0] || n.IPs[1] != want[1] {
		t.Errorf("candidates = %v, want %v", n.IPs, want)
	}
}

func TestDirectoryUpsertRemoveSnapshot(t *testing.T) {
	d := newDirectory()
	a := noderec.DirectoryNode{HostUUID: "a", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}}}
	b := noderec.DirectoryNode{HostUUID: "b", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceErrors: {Port: 14319}}}

	if !d.upsert(a) {
		t.Error("first upsert should be new")
	}
	if d.upsert(a) {
		t.Error("second upsert of same hostUuid should not be new")
	}
	d.upsert(b)

	if all := d.snapshot(""); len(all) != 2 || all[0].HostUUID != "a" || all[1].HostUUID != "b" {
		t.Fatalf("snapshot(all) = %+v, want [a b] sorted", all)
	}
	// Service filter.
	if ol := d.snapshot(noderec.ServiceOllama); len(ol) != 1 || ol[0].HostUUID != "a" {
		t.Fatalf("snapshot(ol) = %+v, want [a]", ol)
	}
	if !d.remove("a") {
		t.Error("remove(a) should report existed")
	}
	if d.remove("a") {
		t.Error("remove(a) again should report not existed")
	}
	if all := d.snapshot(""); len(all) != 1 || all[0].HostUUID != "b" {
		t.Fatalf("after remove, snapshot = %+v, want [b]", all)
	}
}
