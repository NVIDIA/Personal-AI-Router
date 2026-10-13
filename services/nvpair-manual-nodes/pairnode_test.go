// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Two kinds of manual node, told apart by one probe.
//
// A bare Ollama / LM Studio box is what manual nodes were originally for: its
// engines answer plain HTTP on their own ports and it serves no /v1/node-info.
// A PAIR node is the other kind, and it is the one an operator adds when the only
// route between two machines is an overlay network such as a Tailscale tailnet,
// where multicast never arrives and nothing is ever discovered. Its 11434 and
// 1234 carry proxy facades that refuse plaintext from anything but loopback, so
// probing them reports a healthy peer as having no engines.

package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"nvpair-shared/noderec"
)

func strptr(s string) *string { return &s }

// errFakeTimeout stands in for a probe that got no answer.
var errFakeTimeout = errors.New("simulated probe timeout")

// probeSync registers an entry and probes it on this goroutine, so a test reads a
// settled status instead of racing addNode's background probe.
func probeSync(m *Manager, entry ManualEntry) {
	m.mu.Lock()
	m.nodes[nodeID(entry)] = &trackedNode{entry: entry}
	m.mu.Unlock()
	m.probeNode(entry)
}

func statusFor(t *testing.T, m *Manager, id string) ManualNodeStatus {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	tn, ok := m.nodes[id]
	if !ok {
		t.Fatalf("no tracked node %q", id)
	}
	return tn.status
}

// configurePairNode answers /v1/node-info on addr:port as a PAIR node: with a
// service map, which is what identifies it.
func configurePairNode(rt *fakeRoundTripper, addr string, port int, info NodeInfoResponse) {
	host := net.JoinHostPort(addr, strconv.Itoa(port))
	rt.set(http.MethodGet, host, "/v1/node-info", func(*http.Request) (*http.Response, error) {
		data, _ := json.Marshal(info)
		return httpJSON(http.StatusOK, string(data))
	})
}

func pairNodeInfo() NodeInfoResponse {
	return NodeInfoResponse{
		GPUs:           []GPUInfo{{Name: "NVIDIA GeForce RTX 4090", UtilizationPercent: 12}},
		TelemetryValid: true,
		HostUUID:       "peer-host-uuid",
		ClusterUUID:    strptr("peer-cluster-uuid"),
		Services: map[noderec.ServiceKey]int{
			noderec.ServiceNodeInfo:      14318,
			noderec.ServiceOllama:        11434,
			noderec.ServiceEngineManager: 14322,
			noderec.ServiceEngineControl: 14323,
			noderec.ServiceCluster:       14321,
		},
	}
}

// The regression gate: a PAIR node is never probed on its engine ports. A
// plaintext connection there is a guaranteed 403 from the peer's proxy facade,
// and treating that as "engine down" is what made a manually added PAIR peer read
// as having nothing to route to.
func TestProbeNode_PairNodeIsNotProbedOnItsEnginePorts(t *testing.T) {
	m, _, rt := newTestManager()

	var engineProbes atomic.Int32
	for _, port := range []string{"11434", "1234"} {
		host := net.JoinHostPort("gpu-box.tail1234.ts.net", port)
		for _, path := range []string{"/", "/api/tags", "/v1/models"} {
			rt.set(http.MethodGet, host, path, func(*http.Request) (*http.Response, error) {
				engineProbes.Add(1)
				return httpJSON(http.StatusForbidden, `{"error":"loopback-only"}`)
			})
		}
	}
	configurePairNode(rt, "gpu-box.tail1234.ts.net", 14318, pairNodeInfo())

	entry := ManualEntry{Address: "gpu-box.tail1234.ts.net"}
	probeSync(m, entry)

	status := statusFor(t, m, nodeID(entry))
	if !status.PairNode {
		t.Fatal("a node-info answer carrying a service map must mark the node as a PAIR node")
	}
	if n := engineProbes.Load(); n != 0 {
		t.Fatalf("%d plaintext engine probes were made against a PAIR node, want 0", n)
	}
	if status.OllamaUp || status.LMStudioUp {
		t.Fatalf("a PAIR node must report no raw engines (ollama_up=%v lmstudio_up=%v)", status.OllamaUp, status.LMStudioUp)
	}
	if status.Services[noderec.ServiceEngineManager] != 14322 {
		t.Fatalf("services = %v, want the peer's em port carried through", status.Services)
	}
	if status.ClusterUUID == nil || *status.ClusterUUID != "peer-cluster-uuid" {
		t.Fatalf("cluster_uuid = %v, want the peer's principal", status.ClusterUUID)
	}
	if status.HostUUID != "peer-host-uuid" {
		t.Fatalf("hostUuid = %q, want the peer's identity", status.HostUUID)
	}
}

// One missed node-info answer must not turn a peer back into a stranger. Three
// seconds is a routine gap across an overlay network, and without hysteresis that
// gap would probe the peer's proxy facades in plaintext, blank its service map,
// and withdraw it from every consumer for a cycle. Discovery tolerates three
// consecutive misses before evicting an mDNS node; this tolerates the same.
func TestProbeNode_PairNodeSurvivesAMissedProbe(t *testing.T) {
	m, _, rt := newTestManager()

	var engineProbes atomic.Int32
	for _, port := range []string{"11434", "1234"} {
		host := net.JoinHostPort("gpu-box.tail1234.ts.net", port)
		for _, path := range []string{"/", "/api/tags", "/v1/models"} {
			rt.set(http.MethodGet, host, path, func(*http.Request) (*http.Response, error) {
				engineProbes.Add(1)
				return httpJSON(http.StatusForbidden, `{"error":"loopback-only"}`)
			})
		}
	}

	answering := true
	rt.set(http.MethodGet, net.JoinHostPort("gpu-box.tail1234.ts.net", "14318"), "/v1/node-info",
		func(*http.Request) (*http.Response, error) {
			if !answering {
				return nil, errFakeTimeout
			}
			data, _ := json.Marshal(pairNodeInfo())
			return httpJSON(http.StatusOK, string(data))
		})

	entry := ManualEntry{Address: "gpu-box.tail1234.ts.net"}
	probeSync(m, entry)

	answering = false
	for i := 0; i < probeFailThreshold; i++ {
		m.probeNode(entry)
		status := statusFor(t, m, nodeID(entry))
		if !status.PairNode {
			t.Fatalf("probe %d: a missed node-info answer must not un-pair a peer", i+1)
		}
		if status.NodeInfoUp {
			t.Fatalf("probe %d: the node must still read as unreachable", i+1)
		}
		if status.Services[noderec.ServiceEngineManager] != 14322 {
			t.Fatalf("probe %d: services = %v, want the peer's map carried across the gap", i+1, status.Services)
		}
		if status.ClusterUUID == nil || *status.ClusterUUID != "peer-cluster-uuid" {
			t.Fatalf("probe %d: the peer's principal must survive the gap", i+1)
		}
	}
	if n := engineProbes.Load(); n != 0 {
		t.Fatalf("%d plaintext engine probes during the gap, want 0", n)
	}

	// Past the tolerance it does revert: a node that genuinely stopped being a
	// PAIR node must not be remembered as one forever.
	m.probeNode(entry)
	if statusFor(t, m, nodeID(entry)).PairNode {
		t.Fatal("past the failure threshold the node must revert to a bare host")
	}
}

// A node-info answer with no service map is a host too old to report one, or one
// that is not a PAIR node at all. Either way the only thing it can be reached as
// is a bare inference host, so the plain engine probes still run.
func TestProbeNode_NodeInfoWithoutServicesStaysABareHost(t *testing.T) {
	m, _, rt := newTestManager()
	configureHealthyNode(rt, "10.0.0.9", []string{"llama3.2:latest"}, NodeInfoResponse{
		HostUUID:       "bare-host",
		TelemetryValid: true,
	})

	entry := ManualEntry{Address: "10.0.0.9"}
	probeSync(m, entry)

	status := statusFor(t, m, nodeID(entry))
	if status.PairNode {
		t.Fatal("a node-info answer without a service map must not read as a PAIR node")
	}
	if !status.OllamaUp || len(status.OllamaModels) != 1 {
		t.Fatalf("bare host = ollama_up:%v models:%v, want the engine probed", status.OllamaUp, status.OllamaModels)
	}
}

// A paired peer's model inventory is pinned mTLS or nothing. Without a pin the
// node still appears with its hardware; its models arrive once it is paired.
func TestFetchPeerModels_RequiresAPin(t *testing.T) {
	m, _, _ := newTestManager()

	if got := m.fetchPeerModels("gpu-box.tail1234.ts.net", pairNodeInfo()); len(got.Models) != 0 {
		t.Fatalf("models = %v, want none while this node holds no pin for the peer", got.Models)
	}
	// An unclustered peer serves its engine manager over loopback only, so there
	// is nothing to ask for either.
	info := pairNodeInfo()
	info.ClusterUUID = strptr("")
	if got := m.fetchPeerModels("gpu-box.tail1234.ts.net", info); len(got.Models) != 0 {
		t.Fatalf("models = %v, want none for an unclustered peer", got.Models)
	}
}

func TestValidateManualAddress(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"10.0.0.9", "10.0.0.9"},
		{"gpu-box.tail1234.ts.net", "gpu-box.tail1234.ts.net"},
		{"gpu-box", "gpu-box"},
		{"100.101.102.103", "100.101.102.103"},
		{"fd7a:115c:a1e0::1", "fd7a:115c:a1e0::1"},
		{"[fd7a:115c:a1e0::1]", "fd7a:115c:a1e0::1"},
		{"  10.0.0.9  ", "10.0.0.9"},
	} {
		got, err := validateManualAddress(tc.in)
		if err != nil {
			t.Errorf("validateManualAddress(%q) = error %v, want %q", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("validateManualAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// A host:port entry used to be accepted and then read permanently down,
	// because every probe appends its own port to the value.
	for _, bad := range []string{"gpu-box.tail1234.ts.net:14318", "10.0.0.9:11434", "[fd7a::1]:14318"} {
		got, err := validateManualAddress(bad)
		if err == nil {
			t.Errorf("validateManualAddress(%q) = %q, want a rejection", bad, got)
			continue
		}
		if !strings.Contains(err.Error(), "ports") {
			t.Errorf("rejection of %q = %q, want it to point at the ports object", bad, err)
		}
	}

	for _, bad := range []string{"", "   ", "gpu box", "under_score.example"} {
		if _, err := validateManualAddress(bad); err == nil {
			t.Errorf("validateManualAddress(%q) succeeded, want a rejection", bad)
		}
	}
}

func TestManualPorts_OverrideEveryServiceAndDefaultTheRest(t *testing.T) {
	full := ManualEntry{Ports: &ManualPorts{
		NodeInfo: 24318, Cluster: 24321, Ollama: 21434, LMStudio: 2234, VLLM: 8001,
	}}.resolved()
	want := ManualPorts{NodeInfo: 24318, Cluster: 24321, Ollama: 21434, LMStudio: 2234, VLLM: 8001}
	if full != want {
		t.Fatalf("resolved = %+v, want %+v", full, want)
	}

	partial := ManualEntry{Ports: &ManualPorts{Ollama: 21434}}.resolved()
	if partial.Ollama != 21434 {
		t.Errorf("ollama = %d, want the override", partial.Ollama)
	}
	if partial.NodeInfo != defaultNodeInfoPort || partial.LMStudio != defaultLMStudioPort ||
		partial.Cluster != defaultClusterPort || partial.VLLM != defaultVLLMPort {
		t.Errorf("resolved = %+v, want every unset field defaulted", partial)
	}

	if none := (ManualEntry{}).resolved(); none.NodeInfo != defaultNodeInfoPort || none.Ollama != defaultOllamaPort {
		t.Errorf("resolved with no overrides = %+v, want the defaults", none)
	}
}

// Two PAIR nodes can share one loopback when each is addressed by its own ports.
// That is what makes a manual entry usable for a forwarded range, and what lets a
// cross-process test stand up a peer without a second machine.
func TestProbeNode_PortOverridesAddressTheRightService(t *testing.T) {
	m, _, rt := newTestManager()
	configurePairNode(rt, "127.0.0.1", 24318, pairNodeInfo())

	entry := ManualEntry{Address: "127.0.0.1", Name: "peer-b", Ports: &ManualPorts{NodeInfo: 24318}}
	probeSync(m, entry)

	status := statusFor(t, m, "peer-b")
	if !status.NodeInfoUp {
		t.Fatal("node-info on the overridden port must be probed")
	}
	if status.NodeInfoPort != 24318 {
		t.Fatalf("node_info_port = %d, want the override echoed", status.NodeInfoPort)
	}
	if status.Ports == nil || status.Ports.NodeInfo != 24318 {
		t.Fatalf("ports = %+v, want the entry's overrides echoed back", status.Ports)
	}
}

// The engine legs are a table so an engine is one entry rather than a new pair of
// hardcoded calls. A bare host with a relocated engine is reached at its port.
func TestEngineLegs_FollowThePortOverrides(t *testing.T) {
	m, _, rt := newTestManager()
	configureHealthyLMStudio(rt, "10.0.0.9", []string{"qwen2.5-7b-instruct"})
	rt.set(http.MethodGet, net.JoinHostPort("10.0.0.9", "21434"), "/", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{}`)
	})
	rt.set(http.MethodGet, net.JoinHostPort("10.0.0.9", "21434"), "/api/tags", func(*http.Request) (*http.Response, error) {
		return httpJSON(http.StatusOK, `{"models":[{"name":"llama3.2:latest"}]}`)
	})

	entry := ManualEntry{Address: "10.0.0.9", Name: "relocated", Ports: &ManualPorts{Ollama: 21434}}
	probeSync(m, entry)

	status := statusFor(t, m, "relocated")
	if !status.OllamaUp || status.OllamaPort != 21434 {
		t.Fatalf("ollama = up:%v port:%d, want the relocated engine found", status.OllamaUp, status.OllamaPort)
	}
	if !status.LMStudioUp || status.LMStudioPort != defaultLMStudioPort {
		t.Fatalf("lmstudio = up:%v port:%d, want the default port still used", status.LMStudioUp, status.LMStudioPort)
	}
}
