// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
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

func TestDirectory_IdenticalPeerRefreshPreservesModelsAndTimestamp(t *testing.T) {
	dir := newDirectory()
	old := noderec.DirectoryNode{
		HostUUID: "peer-uuid", DiscoveryRecordID: "peer:peer-uuid",
		IdentityVerification: noderec.IdentityPinned, ClusterUUID: "peer-uuid", Trusted: true,
		Name: "peer", IP: "192.0.2.20", IPs: []string{"192.0.2.20"},
		Services:       map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14318}},
		GPUs:           []GPUInfo{{Name: "GPU", UtilizationPercent: 61}},
		CPU:            &CPUInfo{Name: "CPU"},
		Memory:         &MemoryInfo{TotalBytes: 1024},
		Models:         []string{"current-model"},
		ModelsByEngine: map[string][]string{"ollama": {"current-model"}},
		LoadedByEngine: map[string][]string{"ollama": {"current-model"}},
		LastSeen:       1,
	}
	dir.upsert(old)
	// A new response has equal values in separately allocated slices, maps,
	// and hardware pointers. It supplies neither models nor a change timestamp.
	body, err := json.Marshal(old)
	require.NoError(t, err)
	var refreshed noderec.DirectoryNode
	require.NoError(t, json.Unmarshal(body, &refreshed))
	refreshed.Models, refreshed.ModelsByEngine, refreshed.LoadedByEngine = nil, nil, nil
	refreshed.LastSeen = 0
	_, discovered, changed := dir.promote("", refreshed)
	assert.False(t, discovered)
	assert.False(t, changed)
	got, present := dir.get(old.HostUUID)
	require.True(t, present)
	assert.Equal(t, old, got)
}

func TestToDirectoryNode_ProjectsClaimsWithoutGrantingTrust(t *testing.T) {
	raw := RawNode{
		ID:        "hostA",
		Host:      "hostA.local.",
		Addresses: []string{"10.221.0.9", "192.168.1.10"},
		TXT:       []string{"v=1", "uuid=host-uuid", "cluster-uuid=clu", "ip=192.168.1.10", "ni=14318", "ol=11434"},
	}
	n, ok := toDirectoryNode(raw)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, "host-uuid", n.HostUUID)
	assert.Equal(t, "hostA", n.Name)
	assert.Equal(t, "192.168.1.10", n.IP)
	assert.False(t, n.Trusted)
	assert.True(t, n.HasService(noderec.ServiceNodeInfo))
	assert.True(t, n.HasService(noderec.ServiceOllama))
	assert.Equal(t, 11434, n.Services[noderec.ServiceOllama].Port)
}

// Candidate service ports come from TXT claims, independent of the SRV port.
func TestToDirectoryNode_IgnoresSRVPort(t *testing.T) {
	raw := RawNode{
		ID:        "hostC",
		Host:      "hostC.local.",
		Port:      65000, // bogus SRV port — must be ignored
		Addresses: []string{"192.168.1.30"},
		TXT:       []string{"v=1", "uuid=host-c", "ip=192.168.1.30", "ni=14318", "ol=11434"},
	}
	n, ok := toDirectoryNode(raw)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, 14318, n.Services[noderec.ServiceNodeInfo].Port)
	assert.Equal(t, 11434, n.Services[noderec.ServiceOllama].Port)
	for svc, st := range n.Services {
		assert.NotEqual(t, 65000, st.Port, "the non-authoritative SRV port leaked into service (%v)", svc)
	}
}

// Candidate authentication needs a host UUID claim, even though the claim
// does not determine the independent discovery record ID or grant trust.
func TestToDirectoryNode_SkipsWithoutUUID(t *testing.T) {
	raw := RawNode{
		ID:        "hostB",
		Addresses: []string{"10.221.0.9", "192.168.1.20"},
		TXT:       []string{"v=1", "er=14319"},
	}
	_, ok := toDirectoryNode(raw)
	assert.False(t, ok)
}

// Without TXT addresses, retain every resolved candidate in its published order.
func TestToDirectoryNode_UsesResolvedAddressesWithoutIPClaims(t *testing.T) {
	raw := RawNode{
		ID:        "hostB",
		Addresses: []string{"10.221.0.9", "192.168.1.20"},
		TXT:       []string{"v=1", "uuid=host-b", "er=14319"},
	}
	n, ok := toDirectoryNode(raw)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, "10.221.0.9", n.IP)
	assert.Equal(t, []string{"10.221.0.9", "192.168.1.20"}, n.IPs)
	assert.False(t, n.Clustered())
}

// The TXT address order supplies candidate preference over resolved address order.
func TestToDirectoryNode_PreservesPublishedOrder(t *testing.T) {
	raw := RawNode{
		ID:        "spark",
		Addresses: []string{"192.168.240.2", "10.172.54.70"},
		TXT: []string{
			"v=1", "uuid=spark-1", "ip=10.172.54.70",
			"ips=10.172.54.70,192.168.240.2", "ni=14318",
		},
	}
	n, ok := toDirectoryNode(raw)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, "10.172.54.70", n.IP)
	assert.Equal(t, []string{"10.172.54.70", "192.168.240.2"}, n.IPs)
}

// Resolved addresses absent from TXT remain available as fallback candidates.
func TestToDirectoryNode_UnionsUnpublishedAddresses(t *testing.T) {
	raw := RawNode{
		ID:        "hostC",
		Addresses: []string{"10.0.9.9"},
		TXT:       []string{"v=1", "uuid=host-c", "ip=10.0.0.5", "ips=10.0.0.5"},
	}
	n, ok := toDirectoryNode(raw)
	require.True(t, ok, "a record with uuid= should project")
	assert.Equal(t, []string{"10.0.0.5", "10.0.9.9"}, n.IPs)
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
			original, ok := toDirectoryNode(observationFixture())
			require.True(t, ok, "original observation was not projected")
			next, ok := toDirectoryNode(candidate)
			require.True(t, ok, "candidate observation was not projected")
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
			original, ok := toDirectoryNode(observationFixture())
			require.True(t, ok, "original observation was not projected")
			next, ok := toDirectoryNode(candidate)
			require.True(t, ok, "candidate observation was not projected")
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

func TestToDirectoryNode_ClaimsNeverVerifyIdentity(t *testing.T) {
	node, ok := toDirectoryNode(observationFixture())
	require.True(t, ok, "observation was not projected")
	assert.Equal(t, noderec.IdentityUnverified, node.IdentityVerification)
}
