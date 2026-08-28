// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
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
	if got := attributed.EngineModels("ollama"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("EngineModels(ollama) = %v, want [a b]", got)
	}
	if got := attributed.EngineModels("lmstudio"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("EngineModels(lmstudio) = %v, want [c]", got)
	}

	// Attribution present but this engine has no entry: authoritatively empty —
	// NOT the cross-engine union (the whole point of per-engine attribution).
	if got := attributed.EngineModels("llamacpp"); len(got) != 0 {
		t.Errorf("EngineModels(missing engine, attribution present) = %v, want empty", got)
	}

	// No attribution at all (pre-attribution / mixed-version peer): fall back to
	// the flat union so a single-engine consumer doesn't regress to no inventory.
	legacy := DirectoryNode{Models: []string{"a", "b", "c"}}
	if got := legacy.EngineModels("ollama"); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("EngineModels with nil ModelsByEngine = %v, want union [a b c]", got)
	}
}

func TestParseTXT(t *testing.T) {
	txt := []string{
		"v=1", "uuid=host-abc", "cluster-uuid=clu-xyz", "ip=192.168.1.10",
		"ni=14318", "ol=11434", "er=14319", "wl=14320", "cl=14321", "em=14322",
		"unknown=ignored", "bad=notaport",
	}
	r := ParseTXT(txt)
	if r.SchemaVersion != "1" || r.HostUUID != "host-abc" || r.ClusterUUID != "clu-xyz" || r.IP != "192.168.1.10" {
		t.Fatalf("scalar fields wrong: %+v", r)
	}
	if !r.Clustered() {
		t.Error("Clustered() = false, want true (cluster-uuid present)")
	}
	if p, ok := r.Port(ServiceNodeInfo); !ok || p != 14318 {
		t.Errorf("ni port = %d,%v want 14318,true", p, ok)
	}
	if p, ok := r.Port(ServiceCluster); !ok || p != 14321 {
		t.Errorf("cl port = %d,%v want 14321,true", p, ok)
	}
	if p, ok := r.Port(ServiceEngineManager); !ok || p != 14322 {
		t.Errorf("em port = %d,%v want 14322,true", p, ok)
	}
	if _, ok := r.Port(ServiceLMStudio); ok {
		t.Error("lm should be absent")
	}
	// "unknown=" is not a service port; "bad=notaport" is skipped.
	if _, ok := r.Services["unknown"]; ok {
		t.Error("unknown key leaked into Services")
	}
	if _, ok := r.Services["bad"]; ok {
		t.Error("malformed port leaked into Services")
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
	if !strings.Contains(joined, "zz=15000") {
		t.Fatalf("unknown service key dropped from TXT: %v", txt)
	}
	// And it survives a round-trip.
	if p, ok := ParseTXT(txt).Port(ServiceKey("zz")); !ok || p != 15000 {
		t.Errorf("unknown key round-trip = %d,%v want 15000,true", p, ok)
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
	if !strings.HasPrefix(txt[0], "v=") {
		t.Errorf("first TXT entry = %q, want schema", txt[0])
	}
	got := ParseTXT(txt)
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("round-trip mismatch:\n orig=%+v\n got =%+v", orig, got)
	}
}

func TestTXTDeterministicOrder(t *testing.T) {
	r := NodeRecord{
		HostUUID: "h",
		Services: map[ServiceKey]int{ServiceCluster: 14321, ServiceNodeInfo: 14318, ServiceOllama: 11434},
	}
	// Built twice, identical order (map iteration is randomized, so this guards
	// the deterministic emit).
	if !reflect.DeepEqual(r.TXT(), r.TXT()) {
		t.Fatal("TXT() is not deterministic")
	}
	got := r.TXT()
	want := []string{"v=1", "uuid=h", "ni=14318", "ol=11434", "cl=14321"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TXT order = %v, want %v", got, want)
	}
}

func TestTXTDefaultsSchema(t *testing.T) {
	r := NodeRecord{HostUUID: "h", Services: map[ServiceKey]int{}}
	if got := r.TXT()[0]; got != "v="+SchemaVersion {
		t.Errorf("missing schema not defaulted: %q", got)
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
		{ServiceEngineManager, TransportPlain, false, false},
		{ServiceErrors, TransportMTLSWhenClustered, true, false},
		{ServiceWorkload, TransportMTLSWhenClustered, true, false},
		{ServiceCluster, TransportSplit, true, false},
	}
	for _, c := range cases {
		if got := c.svc.Transport(); got != c.want {
			t.Errorf("%s Transport = %v, want %v", c.svc, got, c.want)
		}
		if got := c.svc.UsesMTLS(true); got != c.mtlsClu {
			t.Errorf("%s UsesMTLS(clustered) = %v, want %v", c.svc, got, c.mtlsClu)
		}
		if got := c.svc.UsesMTLS(false); got != c.mtlsUnclu {
			t.Errorf("%s UsesMTLS(unclustered) = %v, want %v", c.svc, got, c.mtlsUnclu)
		}
	}
}

func TestNodeInfoAlwaysPlainEvenClustered(t *testing.T) {
	// The subtlest correctness requirement: a clustered node must NOT trick a
	// consumer into dialing node-info over mTLS.
	if ServiceNodeInfo.UsesMTLS(true) {
		t.Fatal("node-info must be plain even when the node is clustered")
	}
}

func TestSubscribeMatches(t *testing.T) {
	n := DirectoryNode{Services: map[ServiceKey]ServiceStatus{
		ServiceOllama:   {Port: 11434},
		ServiceNodeInfo: {Port: 14318},
	}}
	// Empty filter matches everything.
	if !(SubscribeParams{}).Matches(n) {
		t.Error("empty subscribe filter should match all nodes")
	}
	// A service the node has.
	if !(SubscribeParams{Services: []ServiceKey{ServiceOllama}}).Matches(n) {
		t.Error("filter for ol should match a node advertising ol")
	}
	// A service the node lacks.
	if (SubscribeParams{Services: []ServiceKey{ServiceErrors}}).Matches(n) {
		t.Error("filter for er should not match a node without er")
	}
	// Any-of semantics: one present, one absent.
	if !(SubscribeParams{Services: []ServiceKey{ServiceErrors, ServiceNodeInfo}}).Matches(n) {
		t.Error("any-of filter should match when one listed service is present")
	}
}

func TestDirectoryNodeHelpers(t *testing.T) {
	n := DirectoryNode{ClusterUUID: "clu", Services: map[ServiceKey]ServiceStatus{ServiceCluster: {Port: 14321}}}
	if !n.Clustered() {
		t.Error("Clustered() should be true with a cluster-uuid")
	}
	if !n.HasService(ServiceCluster) || n.HasService(ServiceOllama) {
		t.Error("HasService wrong")
	}
	if (DirectoryNode{}).Clustered() {
		t.Error("empty node should not be Clustered")
	}
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
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got DirectoryNode
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("round-trip mismatch:\n orig=%+v\n got =%+v", orig, got)
	}
}

func TestValidateTXTSize(t *testing.T) {
	if err := ValidateTXTSize([]string{"v=1", "ni=14318"}); err != nil {
		t.Errorf("small TXT flagged: %v", err)
	}
	big := "x=" + strings.Repeat("m", 300)
	if err := ValidateTXTSize([]string{"v=1", big}); err == nil {
		t.Error("oversized TXT entry not flagged")
	}
}
