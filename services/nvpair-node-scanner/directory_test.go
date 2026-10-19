// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestApplyLocalInfo_UpdatesOnlyHardware(t *testing.T) {
	dir := newDirectory()
	node := noderec.DirectoryNode{
		HostUUID: "owned-host", DiscoveryRecordID: "local:owned-host",
		IdentityVerification: noderec.IdentityLocal, Trusted: true,
		Name: "workstation", IP: "192.0.2.20", ClusterUUID: "principal",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineManager: {Port: 14322}},
		Models:   []string{"current-model"}, LastSeen: 1,
	}
	dir.upsert(node)
	info := noderec.NodeInfo{
		HostUUID: "owned-host", GPUs: []GPUInfo{{Name: "GPU", UtilizationPercent: 61}},
		CPU: &CPUInfo{Name: "CPU"}, Memory: &MemoryInfo{TotalBytes: 1024},
	}
	got, result := dir.applyLocalInfo(info)
	require.Equal(t, localInfoUpdated, result)
	want := node
	want.GPUs, want.CPU, want.Memory = info.GPUs, info.CPU, info.Memory
	// Ignore the refresh timestamp when checking preservation of other fields.
	want.LastSeen = got.LastSeen
	assert.Equal(t, want, got)
}

func TestToDirectoryNode_ProjectsIdentityServicesAndTrust(t *testing.T) {
	raw := RawNode{
		ID:        "hostA",
		Host:      "hostA.local.",
		Addresses: []string{"10.221.0.9", "192.168.1.10"},
		TXT:       []string{"v=1", "uuid=host-uuid", "cluster-uuid=clu", "ip=192.168.1.10", "ni=14318", "ol=11434"},
	}
	n, ok := toDirectoryNode(raw, true)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, "host-uuid", n.HostUUID)
	assert.Equal(t, "hostA", n.Name)
	assert.Equal(t, "192.168.1.10", n.IP)
	assert.True(t, n.Trusted, "Trusted should reflect the caller-supplied flag")
	assert.True(t, n.HasService(noderec.ServiceNodeInfo), "services wrong")
	assert.True(t, n.HasService(noderec.ServiceOllama), "services wrong")
	assert.Equal(t, 11434, n.Services[noderec.ServiceOllama].Port, "ol port")
}

// TestToDirectoryNodeIgnoresSRVPort asserts the locked-decision contract: the
// SRV port on a browsed _nvpair-node record is a fixed, NON-authoritative
// constant that consumers MUST ignore — every service port comes only from the
// TXT map. Here the record's SRV port is a bogus value while TXT says ni=14318,
// and the directory entry must report the TXT port, never the SRV one.
func TestToDirectoryNode_IgnoresSRVPort(t *testing.T) {
	raw := RawNode{
		ID:        "hostC",
		Host:      "hostC.local.",
		Port:      65000, // bogus SRV port — must be ignored
		Addresses: []string{"192.168.1.30"},
		TXT:       []string{"v=1", "uuid=host-c", "ip=192.168.1.30", "ni=14318", "ol=11434"},
	}
	n, ok := toDirectoryNode(raw, false)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, 14318, n.Services[noderec.ServiceNodeInfo].Port, "ni port")
	assert.Equal(t, 11434, n.Services[noderec.ServiceOllama].Port, "ol port")
	for svc, st := range n.Services {
		assert.NotEqual(t, 65000, st.Port, "the non-authoritative SRV port leaked into service (%v)", svc)
	}
}

// TestToDirectoryNodeSkipsWithoutUUID: a record with no uuid= can't be keyed by
// a stable identity, so it's skipped (ok=false) rather than keyed by the
// hostname — node identity is the UUID everywhere, no name fallback.
func TestToDirectoryNode_SkipsWithoutUUID(t *testing.T) {
	raw := RawNode{
		ID:        "hostB",
		Addresses: []string{"10.221.0.9", "192.168.1.20"},
		TXT:       []string{"v=1", "er=14319"},
	}
	_, ok := toDirectoryNode(raw, false)
	assert.False(t, ok, "a record without uuid= must be skipped, not projected")
}

// TestToDirectoryNodeIPFallback: with a uuid= but no ip=, every advertised address
// becomes a candidate and the canonical one is the ranker's head. The private
// blocks tie, so this is an arbitrary but stable choice — deliberately so. Which
// private block a node's real network uses is not knowable from the address, and
// the previous preference for 192.168 over 10.x is what made a multi-homed host
// publish a two-host direct-connect link instead of its LAN. Both addresses are
// kept, and a consumer that must connect resolves it by connecting.
func TestToDirectoryNode_UsesRecordIPWhenAddressesAreAbsent(t *testing.T) {
	raw := RawNode{
		ID:        "hostB",
		Addresses: []string{"10.221.0.9", "192.168.1.20"},
		TXT:       []string{"v=1", "uuid=host-b", "er=14319"},
	}
	n, ok := toDirectoryNode(raw, false)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, "10.221.0.9", n.IP, "IP fallback")
	assert.Equal(t, []string{"10.221.0.9", "192.168.1.20"}, n.IPs)
	assert.False(t, n.Clustered(), "no cluster-uuid should mean not clustered")
}

// TestToDirectoryNodePreservesPublishedOrder: a node's own ips= order survives
// projection, and its ip= stays canonical even though the advertised address list
// leads with something else. This is the multi-homed fix at the collapse point —
// the node ranked these with evidence no observer has.
func TestToDirectoryNode_PreservesPublishedOrder(t *testing.T) {
	raw := RawNode{
		ID:        "spark",
		Addresses: []string{"192.168.240.2", "10.172.54.70"},
		TXT: []string{
			"v=1", "uuid=spark-1", "ip=10.172.54.70",
			"ips=10.172.54.70,192.168.240.2", "ni=14318",
		},
	}
	n, ok := toDirectoryNode(raw, false)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, "10.172.54.70", n.IP, "canonical")
	assert.Equal(t, []string{"10.172.54.70", "192.168.240.2"}, n.IPs)
}

// TestToDirectoryNodeUnionsUnpublishedAddresses: an address the browse resolved
// but the node did not rank is kept as a fallback, appended rather than promoted.
func TestToDirectoryNode_UnionsUnpublishedAddresses(t *testing.T) {
	raw := RawNode{
		ID:        "hostC",
		Addresses: []string{"10.0.9.9"},
		TXT:       []string{"v=1", "uuid=host-c", "ip=10.0.0.5", "ips=10.0.0.5"},
	}
	n, ok := toDirectoryNode(raw, false)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, []string{"10.0.0.5", "10.0.9.9"}, n.IPs, "candidates")
}

func TestDirectory_UpdatesRemovesAndSnapshotsNodes(t *testing.T) {
	d := newDirectory()
	a := noderec.DirectoryNode{HostUUID: "a", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}}}
	b := noderec.DirectoryNode{HostUUID: "b", Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceErrors: {Port: 14319}}}

	assert.True(t, d.upsert(a), "first upsert should be new")
	assert.False(t, d.upsert(a), "second upsert of same hostUuid should not be new")
	d.upsert(b)

	all := d.snapshot("")
	require.Len(t, all, 2, "snapshot(all)")
	require.Equal(t, "a", all[0].HostUUID, "snapshot(all) (%v)", all)
	require.Equal(t, "b", all[1].HostUUID, "snapshot(all) (%v)", all)
	// Service filter.
	ol := d.snapshot(noderec.ServiceOllama)
	require.Len(t, ol, 1, "snapshot(ol)")
	require.Equal(t, "a", ol[0].HostUUID, "snapshot(ol) (%v)", ol)
	assert.True(t, d.remove("a"), "remove(a) should report existed")
	assert.False(t, d.remove("a"), "remove(a) again should report not existed")
	all = d.snapshot("")
	require.Len(t, all, 1, "after remove, snapshot")
	require.Equal(t, "b", all[0].HostUUID, "after remove, snapshot (%v)", all)
}

func observationFixture() RawNode {
	return RawNode{
		ID:        "workstation",
		Host:      "workstation.local.",
		Port:      noderec.SRVPort,
		Addresses: []string{"192.0.2.20", "192.0.2.21"},
		TXT:       []string{"v=1", "uuid=claimed-host", "cluster-uuid=claimed-principal", "ni=14318"},
	}
}

func TestToDirectoryNode_AddressOrderAndClaimChangesPreserveDiscoveryRecordID(t *testing.T) {
	test := func(name string, candidate RawNode) {
		t.Run(name, func(t *testing.T) {
			original, ok := toDirectoryNode(observationFixture(), false)
			require.True(t, ok, "original observation must be projected")
			next, ok := toDirectoryNode(candidate, false)
			require.True(t, ok, "candidate observation must be projected")
			require.NotEmpty(t, original.DiscoveryRecordID)
			assert.Equal(t, original.DiscoveryRecordID, next.DiscoveryRecordID)
		})
	}

	test("repeated observation", observationFixture())
	reordered := observationFixture()
	reordered.Addresses = []string{"192.0.2.21", "192.0.2.20"}
	test("resolved address order changes", reordered)
	claims := observationFixture()
	claims.TXT = []string{"ni=14318", "uuid=another-claim", "v=1", "cluster-uuid=another-principal", "ol=11434"}
	test("identity and other service claims change", claims)
}

func TestToDirectoryNode_DifferencesInDiscoveryFieldsYieldDifferentDiscoveryRecordIDs(t *testing.T) {
	test := func(name string, candidate RawNode) {
		t.Run(name, func(t *testing.T) {
			original, ok := toDirectoryNode(observationFixture(), false)
			require.True(t, ok, "original observation must be projected")
			next, ok := toDirectoryNode(candidate, false)
			require.True(t, ok, "candidate observation must be projected")
			require.NotEmpty(t, original.DiscoveryRecordID)
			require.NotEmpty(t, next.DiscoveryRecordID)
			assert.NotEqual(t, original.DiscoveryRecordID, next.DiscoveryRecordID)
		})
	}

	instance := observationFixture()
	instance.ID = "another-instance"
	test("different service instance", instance)
	host := observationFixture()
	host.Host = "another-host.local."
	test("different resolved host", host)
	address := observationFixture()
	address.Addresses = []string{"192.0.2.30"}
	test("copied UUID at another resolved address", address)
	advertised := observationFixture()
	advertised.TXT = append(advertised.TXT, "ip=192.0.2.30")
	test("different advertised connection address", advertised)
	port := observationFixture()
	port.TXT = []string{"v=1", "uuid=claimed-host", "cluster-uuid=claimed-principal", "ni=14328"}
	test("different node-info port", port)
}

func TestToDirectoryNode_TrustedFlagDoesNotVerifyIdentity(t *testing.T) {
	node, ok := toDirectoryNode(observationFixture(), true)
	require.True(t, ok, "observation must be projected")
	assert.Equal(t, noderec.IdentityUnverified, node.IdentityVerification)
}
