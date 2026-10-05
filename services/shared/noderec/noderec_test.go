// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"encoding/json"

	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngineModels(t *testing.T) {
	// Attribution present: each engine gets exactly its own list, never the union.
	attributed := DirectoryNode{
		Models: []string{"a", "b", "c"},
		ModelsByEngine: map[string][]string{
			"ollama":   {"a", "b"},
			"lmstudio": {"c"},
		},
	}
	{
		got := attributed.EngineModels("ollama")
		assert.Equal(t, []string{"a", "b"}, got)
	}
	{
		got := attributed.EngineModels("lmstudio")
		assert.Equal(t, []string{"c"}, got)
	}

	// Attribution present but this engine has no entry: authoritatively empty —
	// NOT the cross-engine union (the whole point of per-engine attribution).
	assert.Len(t, attributed.EngineModels("llamacpp"), 0, "EngineModels(missing engine, attribution present)")

	// No attribution at all (pre-attribution / mixed-version peer): fall back to
	// the flat union so a single-engine consumer doesn't regress to no inventory.
	legacy := DirectoryNode{Models: []string{"a", "b", "c"}}
	{
		got := legacy.EngineModels("ollama")
		assert.Equal(t, []string{"a", "b", "c"}, got, "EngineModels with nil ModelsByEngine")
	}
}

func TestParseTXT(t *testing.T) {
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
	{
		p, ok := r.Port(ServiceNodeInfo)
		assert.True(t, ok, "ni port")
		assert.Equal(t, 14318, p, "ni port")
	}
	{
		p, ok := r.Port(ServiceCluster)
		assert.True(t, ok, "cl port")
		assert.Equal(t, 14321, p, "cl port")
	}
	{
		p, ok := r.Port(ServiceEngineManager)
		assert.True(t, ok, "em port")
		assert.Equal(t, 14322, p, "em port")
	}
	{
		_, ok := r.Port(ServiceLMStudio)
		assert.False(t, ok, "lm should be absent")
	}
	// "unknown=" is not a service port; "bad=notaport" is skipped.
	{
		_, ok := r.Services["unknown"]
		assert.False(t, ok, "unknown key leaked into Services")
	}
	{
		_, ok := r.Services["bad"]
		assert.False(t, ok, "malformed port leaked into Services")
	}
}

// TestTXTEmitsUnknownServiceKey guards the forward-compat path: an unknown/future
// service key must be emitted even when the total service count is at or below
// the number of known keys (a count-based guard would silently drop it).
func TestTXTEmitsUnknownServiceKey(t *testing.T) {
	r := NodeRecord{
		SchemaVersion: "1",
		HostUUID:      "host-abc",
		Services:      map[ServiceKey]int{ServiceNodeInfo: 14318, ServiceKey("zz"): 15000},
	}
	txt := r.TXT()
	joined := strings.Join(txt, ";")
	assert.Contains(t, joined, "zz=15000", "unknown service key dropped from TXT")
	// And it survives a round-trip.
	{
		p, ok := ParseTXT(txt).Port(ServiceKey("zz"))
		assert.True(t, ok, "unknown key round-trip")
		assert.Equal(t, 15000, p, "unknown key round-trip")
	}
}

func TestTXTRoundTrip(t *testing.T) {
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
	got := ParseTXT(txt)
	assert.Equal(t, got, orig, "round-trip mismatch:\n orig")
}

func TestTXTDeterministicOrder(t *testing.T) {
	r := NodeRecord{
		HostUUID: "h",
		Services: map[ServiceKey]int{ServiceCluster: 14321, ServiceNodeInfo: 14318, ServiceOllama: 11434},
	}
	// Built twice, identical order (map iteration is randomized, so this guards
	// the deterministic emit).
	assert.Equal(t, r.TXT(), r.TXT(), "TXT() is not deterministic")
	got := r.TXT()
	want := []string{"v=1", "uuid=h", "ni=14318", "ol=11434", "cl=14321"}
	assert.Equal(t, want, got, "TXT order")
}

func TestTXTDefaultsSchema(t *testing.T) {
	r := NodeRecord{HostUUID: "h", Services: map[ServiceKey]int{}}
	{
		got := r.TXT()[0]
		assert.Equal(t, "v="+SchemaVersion, got, "missing schema not defaulted")
	}
}

func TestTransportPolicy(t *testing.T) {
	cases := []struct {
		svc       ServiceKey
		want      Transport
		mtlsClu   bool // UsesMTLS when target clustered
		mtlsUnclu bool // UsesMTLS when target not clustered
	}{
		{ServiceNodeInfo, TransportPlain, false, false},
		{ServiceOllama, TransportPlain, false, false},
		{ServiceLMStudio, TransportPlain, false, false},
		{ServiceLlamaCPP, TransportPlain, false, false},
		{ServiceEngineManager, TransportPlain, false, false},
		{ServiceErrors, TransportMTLSWhenClustered, true, false},
		{ServiceWorkload, TransportMTLSWhenClustered, true, false},
		{ServiceCluster, TransportSplit, true, false},
	}
	for _, c := range cases {
		{
			got := c.svc.Transport()
			assert.Equal(t, c.want, got)
		}
		{
			got := c.svc.UsesMTLS(true)
			assert.Equal(t, c.mtlsClu, got)
		}
		{
			got := c.svc.UsesMTLS(false)
			assert.Equal(t, c.mtlsUnclu, got)
		}
	}
}

func TestNodeInfoAlwaysPlainEvenClustered(t *testing.T) {
	// The subtlest correctness requirement: a clustered node must NOT trick a
	// consumer into dialing node-info over mTLS.
	assert.False(t, ServiceNodeInfo.UsesMTLS(true), "node-info must be plain even when the node is clustered")
}

func TestSubscribeMatches(t *testing.T) {
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

func TestDirectoryNodeHelpers(t *testing.T) {
	n := DirectoryNode{ClusterUUID: "clu", Services: map[ServiceKey]ServiceStatus{ServiceCluster: {Port: 14321}}}
	assert.True(t, n.Clustered(), "Clustered() should be true with a cluster-uuid")
	assert.True(t, n.HasService(ServiceCluster), "HasService wrong")
	assert.False(t, n.HasService(ServiceOllama), "HasService wrong")
	assert.False(t, (DirectoryNode{}).Clustered(), "empty node should not be Clustered")
}

func TestDirectoryNodeJSONRoundTrip(t *testing.T) {
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
	assert.Equal(t, got, orig, "round-trip mismatch:\n orig")
}

func TestValidateTXTSize(t *testing.T) {
	assert.NoError(t, ValidateTXTSize([]string{"v=1", "ni=14318"}), "small TXT flagged")
	big := "x=" + strings.Repeat("m", 300)
	assert.Error(t, ValidateTXTSize([]string{"v=1", big}), "oversized TXT entry not flagged")
}
