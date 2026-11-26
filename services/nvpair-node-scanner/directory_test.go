// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
)

// Hardware snapshots carry no models. Applying one must preserve every model
// inventory already supplied by an independent model refresh.
func TestApplyLocalInfo_UpdatesOnlyHardware(t *testing.T) {
	dir := newDirectory()
	node := noderec.DirectoryNode{
		HostUUID: "owned-host", DiscoveryRecordID: "local:owned-host",
		IdentityVerification: noderec.IdentityLocal, Trusted: true,
		Name: "workstation", IP: "192.0.2.20", ClusterUUID: "principal",
		Services:       map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineManager: {Port: 14322}},
		Models:         []string{"current-model"},
		ModelsByEngine: map[string][]string{"ollama": {"current-model"}},
		LoadedByEngine: map[string][]string{"ollama": {"current-model"}},
		LastSeen:       1,
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
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo:      {Port: 14318},
			noderec.ServiceEngineManager: {Port: 14322},
		},
		GPUs:               []GPUInfo{{Name: "GPU", UtilizationPercent: 61}},
		CPU:                &CPUInfo{Name: "CPU"},
		Memory:             &MemoryInfo{TotalBytes: 1024},
		Models:             []string{"current-model"},
		ModelsByEngine:     map[string][]string{"ollama": {"current-model"}},
		LoadedByEngine:     map[string][]string{"ollama": {"current-model"}},
		DownloadedByEngine: map[string][]string{"vllm": {"downloaded-model"}},
		LastSeen:           1,
	}
	dir.upsert(old)
	// A new response has equal values in separately allocated slices, maps,
	// and hardware pointers. It supplies neither models nor a change timestamp.
	body, err := json.Marshal(old)
	require.NoError(t, err)
	var refreshed noderec.DirectoryNode
	require.NoError(t, json.Unmarshal(body, &refreshed))
	refreshed.Models, refreshed.ModelsByEngine, refreshed.LoadedByEngine = nil, nil, nil
	refreshed.DownloadedByEngine = nil
	refreshed.LastSeen = 0
	_, discovered, changed := dir.promote("", refreshed)
	assert.False(t, discovered)
	assert.False(t, changed)
	got, present := dir.get(old.HostUUID)
	require.True(t, present)
	assert.Equal(t, old, got)
}

func TestDirectory_PeerRefreshClearsModelsWhenSourceChanges(t *testing.T) {
	test := func(name string, changeSource func(*noderec.DirectoryNode)) {
		t.Run(name, func(t *testing.T) {
			dir := newDirectory()
			old := noderec.DirectoryNode{
				HostUUID: "peer-uuid", DiscoveryRecordID: "peer:original-proof",
				IdentityVerification: noderec.IdentityPinned, ClusterUUID: "peer-uuid", Trusted: true,
				IP: "192.0.2.20", IPs: []string{"192.0.2.20", "192.0.2.21"},
				Services: map[noderec.ServiceKey]noderec.ServiceStatus{
					noderec.ServiceNodeInfo:      {Port: 14318},
					noderec.ServiceEngineManager: {Port: 14322},
				},
				Models:             []string{"old-model"},
				ModelsByEngine:     map[string][]string{"ollama": {"old-model"}},
				LoadedByEngine:     map[string][]string{"ollama": {"old-model"}},
				DownloadedByEngine: map[string][]string{"vllm": {"old-download"}},
			}
			dir.upsert(old)
			refreshed := old
			refreshed.Services = maps.Clone(old.Services)
			refreshed.IPs = slices.Clone(old.IPs)
			refreshed.Models, refreshed.ModelsByEngine, refreshed.LoadedByEngine = nil, nil, nil
			refreshed.DownloadedByEngine = nil
			changeSource(&refreshed)

			_, discovered, changed := dir.promote("", refreshed)

			assert.False(t, discovered)
			assert.True(t, changed)
			current, present := dir.get(old.HostUUID)
			require.True(t, present)
			assert.Empty(t, current.Models)
			assert.Empty(t, current.ModelsByEngine)
			assert.Empty(t, current.LoadedByEngine)
			assert.Empty(t, current.DownloadedByEngine)
		})
	}
	test("engine-manager removed", func(node *noderec.DirectoryNode) {
		delete(node.Services, noderec.ServiceEngineManager)
	})
	test("engine-manager port changed", func(node *noderec.DirectoryNode) {
		node.Services[noderec.ServiceEngineManager] = noderec.ServiceStatus{Port: 14323}
	})
	test("primary address changed", func(node *noderec.DirectoryNode) {
		node.IP, node.IPs[0] = "192.0.2.30", "192.0.2.30"
	})
	test("secondary address changed", func(node *noderec.DirectoryNode) {
		node.IPs[1] = "192.0.2.31"
	})
	test("authenticated proof changed", func(node *noderec.DirectoryNode) {
		node.DiscoveryRecordID = "peer:replacement-proof"
	})
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

// netpick.Candidates deliberately preserves the publisher's order, so a node
// re-publishing its own instance can hand the hash the same address set in a
// different order: one fixture publishes "192.0.2.30,192.0.2.20", which is not
// sort.Strings order, and the other the reverse. Only the hash's own
// normalization keeps them at one record ID. The "resolved address order
// changes" case above cannot stand in for this one: its TXT publishes no
// ip=/ips=, so netpick.RankRemote's ascending tie-break sorts the addresses
// before the hash sees a difference.
func TestToDirectoryNode_PublishedAddressOrderChangesPreserveDiscoveryRecordID(t *testing.T) {
	published := RawNode{
		ID:        "spark",
		Host:      "nvpair-0123456789ab.local.",
		Addresses: []string{"192.0.2.20", "192.0.2.30"},
		TXT: []string{
			"v=1", "uuid=spark-1", "ni=14318",
			"ip=192.0.2.30", "ips=192.0.2.30,192.0.2.20",
		},
	}
	republished := published
	republished.TXT = []string{
		"v=1", "uuid=spark-1", "ni=14318",
		"ip=192.0.2.20", "ips=192.0.2.20,192.0.2.30",
	}

	first, ok := toDirectoryNode(published)
	require.True(t, ok, "published observation was not projected")
	second, ok := toDirectoryNode(republished)
	require.True(t, ok, "re-published observation was not projected")
	require.NotEmpty(t, first.DiscoveryRecordID)
	assert.Equal(t, first.DiscoveryRecordID, second.DiscoveryRecordID,
		"swapping the published ip=/ips= order changed the discovery record ID")
}

func TestToDirectoryNode_DifferencesInDiscoveryFieldsYieldDifferentDiscoveryRecordIDs(t *testing.T) {
	// Every field listed here is a hash input by rule. The SRV target used to be
	// one of these cases; it is no longer a discovery field, and
	// TestToDirectoryNode_DoesNotHashTheSRVTarget and
	// TestNodeDiscoveryRecordID_IgnoresTheSRVTarget now pin that invariant.
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

// The SRV target is a PAIR-claimed host label (noderec.HostLabel), so it is not
// an input to an unverified record ID: a record whose service instance, advertised
// node-info port and address set are unchanged keeps its ID as the label the peer
// advertises moves — including the move from a hostname-derived label to a
// synthetic one when a node upgrades.
func TestToDirectoryNode_DoesNotHashTheSRVTarget(t *testing.T) {
	original, ok := toDirectoryNode(observationFixture())
	require.True(t, ok, "original observation was not projected")
	require.NotEmpty(t, original.DiscoveryRecordID)

	test := func(name, advertisedHost string) {
		t.Run(name, func(t *testing.T) {
			candidate := observationFixture()
			candidate.Host = advertisedHost
			next, ok := toDirectoryNode(candidate)
			require.True(t, ok, "candidate observation was not projected")
			require.NotEmpty(t, next.DiscoveryRecordID)
			assert.Equal(t, original.DiscoveryRecordID, next.DiscoveryRecordID,
				"host label %q changed the unverified record ID", advertisedHost)
		})
	}

	test("hostname-derived label replaced by another hostname", "another-host.local.")
	test("hostname-derived label replaced by the synthetic PAIR label", "nvpair-0123456789ab.local.")
	test("SRV target missing or unresolvable in the browse result", "")
}

// nodeDiscoveryRecordID is the key daemon.go hands the browser (WithKeyFunc) and
// the key the directory stores unverified records under, so the SRV-target rule
// has to hold there too and not only on the projected node.
//
// The fixture publishes an ip= the browse did not resolve, so the two address
// sources the call sites could use differ: the projection hashes
// netpick.Candidates(...) while the browser key hashes whatever
// nodeDiscoveryRecordID feeds it, and an agreement assertion between them is
// only worth having while those inputs are distinguishable.
func TestNodeDiscoveryRecordID_IgnoresTheSRVTarget(t *testing.T) {
	base := observationFixture()
	base.TXT = append(base.TXT, "ip=192.0.2.30")
	require.NotEqual(t, netpick.Candidates(base.TXT, base.Addresses), base.Addresses,
		"the published ip= must stay outside the resolved set, or the call-site agreement assertion below is toothless")
	baseID := nodeDiscoveryRecordID(base)
	require.NotEmpty(t, baseID)

	for _, advertisedHost := range []string{"another-host.local.", "nvpair-0123456789ab.local.", ""} {
		// RawNode has no nested state this test mutates: only the scalar Host
		// field differs, so the copy needs no deep copy of the slices.
		candidate := base
		candidate.Host = advertisedHost
		assert.Equal(t, baseID, nodeDiscoveryRecordID(candidate),
			"host label %q reached the record key the browser and directory use", advertisedHost)
		// Both call sites must still agree: a change to one of them would re-key
		// the directory under the key the browser no longer uses.
		node, ok := toDirectoryNode(candidate)
		require.True(t, ok, "observation with host %q was not projected", advertisedHost)
		assert.Equal(t, nodeDiscoveryRecordID(candidate), node.DiscoveryRecordID,
			"the projected record and the browser key diverged for host %q", advertisedHost)
	}
}

// The byte-identity proof for this record ID: one exact digest for one fixed
// input, pinned in the tree rather than in a scratch program. The two cases are
// one address set published in two orders, so removing the hash's
// order-insensitivity breaks the first; letting any field that is not a
// connection input into the hash breaks both — the SRV target and port are
// populated right here precisely to make that mistake tempting to commit.
func TestDiscoveryRecordID_MatchesThePinnedGoldenDigest(t *testing.T) {
	test := func(name string, addresses []string) {
		t.Run(name, func(t *testing.T) {
			raw := RawNode{
				ID:   "workstation",
				Host: "nvpair-a1b2c3d4e5f6.local.",
				Port: noderec.SRVPort,
			}
			// sha256 over fmt.Sprintf("%q", fields) for fields =
			// ["workstation" "14318" "192.0.2.10" "192.0.2.20" "192.0.2.30"],
			// the addresses in sort.Strings order. Updating this digest is a
			// deliberate, rollout-impacting act: it re-keys every unverified
			// discovery in the field at once.
			assert.Equal(t, "observation:a719ee0865d3fe75d10ba7711aa94290d5d1a191ff898a8a0f9577d1cf562678",
				discoveryRecordID(raw, 14318, addresses))
		})
	}
	test("published order", []string{"192.0.2.30", "192.0.2.10", "192.0.2.20"})
	test("sorted order", []string{"192.0.2.10", "192.0.2.20", "192.0.2.30"})
}

// The caller's ranked address order is a candidate preference the hash must read
// without rewriting: it sorts a clone, so a slice handed to it stays in the
// publication order its owner is still using.
func TestDiscoveryRecordID_LeavesTheCallersAddressOrderAlone(t *testing.T) {
	raw := RawNode{ID: "workstation", Host: "workstation.local."}
	addresses := []string{"192.0.2.30", "192.0.2.10", "192.0.2.20"}
	want := []string{"192.0.2.30", "192.0.2.10", "192.0.2.20"}
	recordID := discoveryRecordID(raw, 14318, addresses)
	require.NotEmpty(t, recordID)
	assert.Equal(t, want, addresses, "discoveryRecordID sorted the caller's slice in place")
}

func TestToDirectoryNode_ClaimsNeverVerifyIdentity(t *testing.T) {
	node, ok := toDirectoryNode(observationFixture())
	require.True(t, ok, "observation was not projected")
	assert.Equal(t, noderec.IdentityUnverified, node.IdentityVerification)
}
