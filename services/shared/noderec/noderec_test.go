// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSampleAging_PreservesValidityAndFreshness(t *testing.T) {
	// Aging must retain sample validity and add elapsed time without making
	// negative ages, backward clock movement, or overflow produce fresh samples.
	test := func(name string, info NodeInfo, elapsed time.Duration, wantAge int64) {
		t.Run(name, func(t *testing.T) {
			got := info.Aged(elapsed)
			assert.Equal(t, info.TelemetryValid, got.TelemetryValid)
			assert.Equal(t, wantAge, got.MSSince)

			telemetry := NodeTelemetry{HostUUID: "host-a", GPUUtilizationPct: 84, TelemetryValid: info.TelemetryValid, MSSince: info.MSSince}
			want := telemetry
			want.MSSince = wantAge
			assert.Equal(t, want, telemetry.Aged(elapsed), "compact scheduling samples share the hardware age rules")
		})
	}
	test("queue time", NodeInfo{TelemetryValid: true, MSSince: 137}, 250*time.Millisecond, 387)
	test("unavailable sample", NodeInfo{MSSince: 137}, time.Second, 0)
	test("negative source age", NodeInfo{TelemetryValid: true, MSSince: -1}, time.Second, 1000)
	test("clock moves backwards", NodeInfo{TelemetryValid: true, MSSince: 137}, -time.Second, 137)
	test("age saturates", NodeInfo{TelemetryValid: true, MSSince: 1<<63 - 2}, time.Second, 1<<63-1)
}

func TestNodeInfo_PeerMetadataIsOptionalOnTheWire(t *testing.T) {
	test := func(name string, peer *PeerMetadata) {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(NodeInfo{HostUUID: "host-a", Peer: peer})
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(body, &fields))
			_, present := fields["peer"]
			assert.Equal(t, peer != nil, present)
			var decoded NodeInfo
			require.NoError(t, json.Unmarshal(body, &decoded))
			assert.Equal(t, peer, decoded.Peer)
		})
	}
	test("public hardware", nil)
	test("peer node inventory", &PeerMetadata{Name: "workstation", Addresses: []string{"192.0.2.20"}, Services: map[ServiceKey]int{ServiceNodeInfo: 14318}})
}

func TestEngineModels_PrefersPerEngineInventoryAndFallsBackToLegacyModels(t *testing.T) {
	// Attribution present: each engine gets exactly its own list, never the union.
	attributed := DirectoryNode{
		Models: []string{"a", "b", "c"},
		ModelsByEngine: map[string][]string{
			"ollama":   {"a", "b"},
			"lmstudio": {"c"},
		},
	}
	assert.Equal(t, []string{"a", "b"}, attributed.EngineModels("ollama"))
	assert.Equal(t, []string{"c"}, attributed.EngineModels("lmstudio"))

	// Attribution present but this engine has no entry: authoritatively empty —
	// NOT the cross-engine union (the whole point of per-engine attribution).
	assert.Empty(t, attributed.EngineModels("llamacpp"), "EngineModels(missing engine, attribution present)")

	// No attribution at all (pre-attribution / mixed-version peer): fall back to
	// the flat union so a single-engine consumer doesn't regress to no inventory.
	legacy := DirectoryNode{Models: []string{"a", "b", "c"}}
	assert.Equal(t, []string{"a", "b", "c"}, legacy.EngineModels("ollama"), "EngineModels with nil ModelsByEngine")
}

func TestParseTXT_ExtractsNodeIdentityAndServices(t *testing.T) {
	txt := []string{
		"v=1", "uuid=host-abc", "cluster-uuid=clu-xyz", "ip=192.168.1.10",
		"ni=14318", "ol=11434", "er=14319", "wl=14320", "cl=14321", "em=14322",
		"unknown=ignored", "bad=notaport",
	}
	r := ParseTXT(txt)
	assert.Equal(t, "1", r.SchemaVersion, "scalar fields wrong")
	assert.Equal(t, "host-abc", r.HostUUID, "scalar fields wrong")
	assert.Equal(t, "clu-xyz", r.ClusterUUID, "scalar fields wrong")
	assert.Equal(t, "192.168.1.10", r.IP, "scalar fields wrong")
	assert.True(t, r.Clustered(), "Clustered() = false, want true (cluster-uuid present)")
	p, ok := r.Port(ServiceNodeInfo)
	assert.True(t, ok, "ni port")
	assert.Equal(t, 14318, p, "ni port")
	p, ok = r.Port(ServiceCluster)
	assert.True(t, ok, "cl port")
	assert.Equal(t, 14321, p, "cl port")
	p, ok = r.Port(ServiceEngineManager)
	assert.True(t, ok, "em port")
	assert.Equal(t, 14322, p, "em port")
	_, ok = r.Port(ServiceLMStudio)
	assert.False(t, ok, "lm should be absent")
	// "unknown=" is not a service port; "bad=notaport" is skipped.
	assert.NotContains(t, r.Services, ServiceKey("unknown"), "unknown key leaked into Services")
	assert.NotContains(t, r.Services, ServiceKey("bad"), "malformed port leaked into Services")
}

// TestTXTEmitsUnknownServiceKey guards the forward-compat path: an unknown/future
// service key must be emitted even when the total service count is at or below
// the number of known keys (a count-based guard would silently drop it).
func TestTXT_EmitsUnknownServiceKey(t *testing.T) {
	r := NodeRecord{
		SchemaVersion: "1",
		HostUUID:      "host-abc",
		Services:      map[ServiceKey]int{ServiceNodeInfo: 14318, ServiceKey("zz"): 15000},
	}
	txt := r.TXT()
	joined := strings.Join(txt, ";")
	assert.Contains(t, joined, "zz=15000", "unknown service key dropped from TXT")
	// And it survives a round-trip.
	p, ok := ParseTXT(txt).Port(ServiceKey("zz"))
	assert.True(t, ok, "unknown key round-trip")
	assert.Equal(t, 15000, p, "unknown key round-trip")
}

func TestTXT_RoundTripsNodeRecord(t *testing.T) {
	orig := NodeRecord{
		SchemaVersion: "1",
		HostUUID:      "host-abc",
		ClusterUUID:   "clu-xyz",
		IP:            "10.0.0.5",
		Services:      map[ServiceKey]int{ServiceOllama: 11434, ServiceErrors: 14319, ServiceNodeInfo: 14318, ServiceEngineManager: 14322},
	}
	txt := orig.TXT()
	// Schema must be first.
	assert.True(t, strings.HasPrefix(txt[0], "v="), "first TXT entry")
	assert.Equal(t, orig, ParseTXT(txt), "round-trip mismatch")
}

func TestTXT_EmitsFieldsInDeterministicOrder(t *testing.T) {
	r := NodeRecord{
		HostUUID: "h",
		Services: map[ServiceKey]int{ServiceCluster: 14321, ServiceNodeInfo: 14318, ServiceOllama: 11434},
	}
	// Built twice, identical order (map iteration is randomized, so this guards
	// the deterministic emit).
	assert.Equal(t, r.TXT(), r.TXT(), "TXT() is not deterministic")
	assert.Equal(t, []string{"v=1", "uuid=h", "ni=14318", "ol=11434", "cl=14321"}, r.TXT(), "TXT order")
}

func TestTXT_UsesDefaultSchemaWhenUnspecified(t *testing.T) {
	r := NodeRecord{HostUUID: "h", Services: map[ServiceKey]int{}}
	assert.Equal(t, "v="+SchemaVersion, r.TXT()[0], "missing schema not defaulted")
}

func TestServiceTransport_RequiresMutualTLSForClusteredPeerServices(t *testing.T) {
	test := func(name string, svc ServiceKey, want Transport, mtlsClustered, mtlsUnclustered bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, svc.Transport())
			assert.Equal(t, mtlsClustered, svc.UsesMTLS(true), "clustered")
			assert.Equal(t, mtlsUnclustered, svc.UsesMTLS(false), "unclustered")
		})
	}
	test("node info", ServiceNodeInfo, TransportPlain, false, false)
	test("Ollama", ServiceOllama, TransportPlain, false, false)
	test("LM Studio", ServiceLMStudio, TransportPlain, false, false)
	test("llama.cpp", ServiceLlamaCPP, TransportPlain, false, false)
	test("engine manager", ServiceEngineManager, TransportPlain, false, false)
	test("errors", ServiceErrors, TransportMTLSWhenClustered, true, false)
	test("workloads", ServiceWorkload, TransportMTLSWhenClustered, true, false)
	test("cluster", ServiceCluster, TransportSplit, true, false)
}

func TestNodeInfoTransport_UsesPlainHTTPRegardlessOfMembership(t *testing.T) {
	// The subtlest correctness requirement: a clustered node must NOT trick a
	// consumer into dialing node-info over mTLS.
	assert.False(t, ServiceNodeInfo.UsesMTLS(true), "node-info must be plain even when the node is clustered")
}

func TestSubscribe_MatchesRequestedServiceFilters(t *testing.T) {
	n := DirectoryNode{Services: map[ServiceKey]ServiceStatus{
		ServiceOllama:   {Port: 11434},
		ServiceNodeInfo: {Port: 14318},
	}}
	// Empty filter matches everything.
	assert.True(t, (SubscribeParams{}).Matches(n), "empty subscribe filter should match all nodes")
	// A service the node has.
	assert.True(t, (SubscribeParams{Services: []ServiceKey{ServiceOllama}}).Matches(n), "filter for ol should match a node advertising ol")
	// A service the node lacks.
	assert.False(t, (SubscribeParams{Services: []ServiceKey{ServiceErrors}}).Matches(n), "filter for er should not match a node without er")
	// Any-of semantics: one present, one absent.
	assert.True(t, (SubscribeParams{Services: []ServiceKey{ServiceErrors, ServiceNodeInfo}}).Matches(n), "any-of filter should match when one listed service is present")
}

func TestDirectoryNode_ReportsMembershipAndServicePresence(t *testing.T) {
	n := DirectoryNode{ClusterUUID: "clu", Services: map[ServiceKey]ServiceStatus{ServiceCluster: {Port: 14321}}}
	assert.True(t, n.Clustered(), "Clustered() should be true with a cluster-uuid")
	assert.True(t, n.HasService(ServiceCluster), "HasService wrong")
	assert.False(t, n.HasService(ServiceOllama), "HasService wrong")
	assert.False(t, (DirectoryNode{}).Clustered(), "empty node should not be Clustered")
}

func TestDirectoryNode_EqualsComparesValues(t *testing.T) {
	original := DirectoryNode{
		DiscoveryRecordID: "peer:host-1", IdentityVerification: IdentityPinned,
		HostUUID: "host-1", ClusterUUID: "host-1", Trusted: true,
		Name: "host", IP: "192.0.2.20", IPs: []string{"192.0.2.20"},
		Services:       map[ServiceKey]ServiceStatus{ServiceNodeInfo: {Port: 14318}},
		GPUs:           []GPUInfo{{Name: "GPU", VramBytes: 1024}},
		CPU:            &CPUInfo{Name: "CPU", Cores: 8},
		Memory:         &MemoryInfo{TotalBytes: 4096},
		Models:         []string{"model-a"},
		ModelsByEngine: map[string][]string{"ollama": {"model-a"}},
		LoadedByEngine: map[string][]string{"ollama": {"model-a"}},
		LastSeen:       123,
	}
	// Decoding produces separate allocations with the same values.
	body, err := json.Marshal(original)
	require.NoError(t, err)
	var decoded DirectoryNode
	require.NoError(t, json.Unmarshal(body, &decoded))
	assert.True(t, original.Equals(decoded))
	assert.True(t, decoded.Equals(original))
	assert.True(t, (DirectoryNode{}).Equals(DirectoryNode{}))
}

func TestDirectoryNode_EqualsDistinguishesRecordChanges(t *testing.T) {
	test := func(name string, a, b DirectoryNode) {
		t.Run(name, func(t *testing.T) {
			assert.False(t, a.Equals(b))
			assert.False(t, b.Equals(a))
		})
	}
	test("verification changes", DirectoryNode{IdentityVerification: IdentityUnverified}, DirectoryNode{IdentityVerification: IdentityPinned})
	test("address order changes", DirectoryNode{IPs: []string{"192.0.2.20", "192.0.2.21"}}, DirectoryNode{IPs: []string{"192.0.2.21", "192.0.2.20"}})
	test("model list changes", DirectoryNode{Models: []string{"model-a"}}, DirectoryNode{Models: []string{"model-b"}})
	test("model attribution changes", DirectoryNode{ModelsByEngine: map[string][]string{"ollama": {"model-a"}}}, DirectoryNode{ModelsByEngine: map[string][]string{"lmstudio": {"model-a"}}})
	test("loaded models change", DirectoryNode{LoadedByEngine: map[string][]string{"ollama": {"model-a"}}}, DirectoryNode{LoadedByEngine: map[string][]string{"ollama": {"model-b"}}})
	test("timestamp changes", DirectoryNode{LastSeen: 1}, DirectoryNode{LastSeen: 2})
}

func TestDirectoryNode_EqualsDistinguishesAbsentAndEmptyValues(t *testing.T) {
	test := func(name string, a, b DirectoryNode) {
		t.Run(name, func(t *testing.T) {
			assert.False(t, a.Equals(b))
			assert.False(t, b.Equals(a))
		})
	}
	test("addresses", DirectoryNode{}, DirectoryNode{IPs: []string{}})
	test("services", DirectoryNode{}, DirectoryNode{Services: map[ServiceKey]ServiceStatus{}})
	test("GPUs", DirectoryNode{}, DirectoryNode{GPUs: []GPUInfo{}})
	test("CPU", DirectoryNode{}, DirectoryNode{CPU: &CPUInfo{}})
	test("memory", DirectoryNode{}, DirectoryNode{Memory: &MemoryInfo{}})
	test("models", DirectoryNode{}, DirectoryNode{Models: []string{}})
	test("models by engine", DirectoryNode{}, DirectoryNode{ModelsByEngine: map[string][]string{}})
	test("loaded models by engine", DirectoryNode{}, DirectoryNode{LoadedByEngine: map[string][]string{}})
	test("engine model list", DirectoryNode{ModelsByEngine: map[string][]string{"ollama": nil}}, DirectoryNode{ModelsByEngine: map[string][]string{"ollama": {}}})
	test("engine loaded model list", DirectoryNode{LoadedByEngine: map[string][]string{"ollama": nil}}, DirectoryNode{LoadedByEngine: map[string][]string{"ollama": {}}})
}

func TestDirectoryNode_RoundTripsJSON(t *testing.T) {
	orig := DirectoryNode{
		HostUUID: "host-1", Name: "host", IP: "192.168.1.10", ClusterUUID: "clu",
		Trusted: true,
		Services: map[ServiceKey]ServiceStatus{
			ServiceOllama:   {Port: 11434, Probe: ProbeReachable},
			ServiceErrors:   {Port: 14319, Probe: ProbeInaccessible},
			ServiceNodeInfo: {Port: 14318},
		},
		GPUs:     []GPUInfo{{Name: "RTX", VramBytes: 1 << 30}},
		CPU:      &CPUInfo{Name: "cpu", Cores: 8},
		Memory:   &MemoryInfo{TotalBytes: 1 << 34},
		LastSeen: 1234567890,
	}
	b, err := json.Marshal(orig)
	require.NoError(t, err, "marshal")
	var got DirectoryNode
	require.NoError(t, json.Unmarshal(b, &got), "unmarshal")
	assert.Equal(t, orig, got, "round-trip mismatch")
}

func TestValidateTXTSize_RejectsOversizedEntries(t *testing.T) {
	require.NoError(t, ValidateTXTSize([]string{"v=1", "ni=14318"}), "small TXT flagged")
	big := "x=" + strings.Repeat("m", 300)
	assert.Error(t, ValidateTXTSize([]string{"v=1", big}), "oversized TXT entry not flagged")
}

func TestDirectoryNode_JSONRoundTripPreservesIdentityFields(t *testing.T) {
	type nodeIdentityFields struct {
		HostUUID             string `json:"hostUuid"`
		DiscoveryRecordID    string `json:"discoveryRecordId"`
		IdentityVerification string `json:"identityVerification"`
	}

	test := func(name, discoveryRecordID, verification string) {
		t.Run(name, func(t *testing.T) {
			identity := nodeIdentityFields{
				HostUUID:             "host-a",
				DiscoveryRecordID:    discoveryRecordID,
				IdentityVerification: verification,
			}
			body, err := json.Marshal(identity)
			require.NoError(t, err)
			var node DirectoryNode
			require.NoError(t, json.Unmarshal(body, &node))
			body, err = json.Marshal(node)
			require.NoError(t, err)
			var got nodeIdentityFields
			require.NoError(t, json.Unmarshal(body, &got))
			assert.Equal(t, identity, got)
		})
	}

	test("owned local record", "local:host-a", "local")
	test("authenticated peer record", "peer:host-a", "pinned")
	test("unverified discovery record", "observation:candidate-a", "unverified")
}
