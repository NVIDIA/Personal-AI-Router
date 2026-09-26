// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/reach"
)

// These tests use synthetic OS facts, local temporary trust fixtures, and
// in-memory HTTP transports only. They never run Manager.Run or start a server.
type cableTestTransport func(*http.Request) (*http.Response, error)

func (f cableTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func cableTestSelection() cableprobe.ReviewRequest {
	return cableprobe.ReviewRequest{
		NodeIDs: []string{"host-owner", "host-peer"},
		Ports: []cableprobe.PortRef{
			{NodeID: "host-owner", SwitchID: "0011223344556677", PortName: "p0"},
			{NodeID: "host-peer", SwitchID: "0011223344556688", PortName: "p1"},
		},
	}
}

func cableTestInfo(t *testing.T, now time.Time, aliases int) cableNodeInfo {
	t.Helper()
	interfaces := []map[string]any{}
	for i := aliases; i > 0; i-- {
		interfaces = append(interfaces, map[string]any{
			"name": fmt.Sprintf("eth%d", i), "index": i,
			"mac": fmt.Sprintf("02:00:00:00:00:%02x", i), "physical": true,
			"physicalPort": map[string]string{"source": "linux-sysfs", "switchId": "0011223344556677", "portName": "p0"},
		})
	}
	body, err := json.Marshal(map[string]any{
		"hostUuid":    "host-owner",
		"connections": map[string]any{"source": "host-os", "status": "observed", "observedAt": now.UnixMilli(), "interfaces": interfaces},
	})
	if err != nil {
		t.Fatal(err)
	}
	var result cableNodeInfo
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func cableTestSnapshot(t *testing.T, now time.Time, aliases int) cablePortSnapshot {
	t.Helper()
	result, err := projectCablePorts(cableTestInfo(t, now, aliases), "host-owner", "principal-owner", now)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func cableTestResponse(t *testing.T, value any) *http.Response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
}

func cableTestManager(t *testing.T, read func(*http.Request) (*http.Response, error)) (*Manager, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "synthetic-cluster", "principal-owner", "principal-peer")
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), NewExecutor(nil, nil, nil, t.TempDir()), clustertrust.Open(dir))
	m.cableLocal = &cableLocalFacts{nodeID: "host-owner", port: 14318, http: &http.Client{Transport: cableTestTransport(read)}}
	t.Cleanup(func() { m.remoteHTTP.CloseIdle(); m.readyHTTP.CloseIdle() })
	return m, &out, dir
}

func TestCableSelectionValidation(t *testing.T) {
	if !validateCableSelection(cableTestSelection()) {
		t.Fatal("bounded selection rejected")
	}
	for _, name := range []string{"one-node", "four-nodes", "duplicate-node", "empty-node", "blank-node", "control-node", "oversize-node", "duplicate-port", "foreign-port", "missing-port", "three-ports", "empty-switch", "empty-port"} {
		t.Run(name, func(t *testing.T) {
			p := cableTestSelection()
			switch name {
			case "one-node":
				p.NodeIDs = p.NodeIDs[:1]
			case "four-nodes":
				p.NodeIDs = append(p.NodeIDs, "third", "fourth")
			case "duplicate-node":
				p.NodeIDs[1] = p.NodeIDs[0]
			case "empty-node":
				p.NodeIDs[0] = ""
			case "blank-node":
				p.NodeIDs[0] = " "
			case "control-node":
				p.NodeIDs[0] = "node\x1b"
			case "oversize-node":
				p.NodeIDs[0] = strings.Repeat("x", 129)
			case "duplicate-port":
				p.Ports = append(p.Ports, p.Ports[0])
			case "foreign-port":
				p.Ports[0].NodeID = "foreign"
			case "missing-port":
				p.Ports = p.Ports[:1]
			case "three-ports":
				p.Ports = append(p.Ports, cableprobe.PortRef{NodeID: "host-owner", SwitchID: "second", PortName: "p1"}, cableprobe.PortRef{NodeID: "host-owner", SwitchID: "third", PortName: "p2"})
			case "empty-switch":
				p.Ports[0].SwitchID = ""
			case "empty-port":
				p.Ports[0].PortName = ""
			}
			if validateCableSelection(p) {
				t.Fatal("invalid selection accepted")
			}
		})
	}
}

func TestCablePortProjection(t *testing.T) {
	now := time.UnixMilli(100_000)
	got := cableTestSnapshot(t, now, 4)
	if got.NodeID != "host-owner" || got.Principal != "principal-owner" || got.AgeMs != 0 || len(got.Ports) != 1 || len(got.Ports[0].Interfaces) != 4 {
		t.Fatalf("incorrect physical-port projection: %+v", got)
	}
	for i, alias := range got.Ports[0].Interfaces {
		if alias.Index != i+1 {
			t.Fatal("native aliases were not deterministically ordered")
		}
	}
	for _, name := range []string{"host-mismatch", "missing-connections", "wrong-source", "unavailable", "stale", "future", "zero-time", "truncated", "missing-interfaces", "oversize-interfaces", "five-aliases", "nonphysical", "untrusted-port-source", "missing-switch", "invalid-mac", "zero-mac", "multicast-mac", "invalid-index", "duplicate-index", "duplicate-name"} {
		t.Run(name, func(t *testing.T) {
			info := cableTestInfo(t, now, 2)
			c := info.Connections
			switch name {
			case "host-mismatch":
				info.HostUUID = "foreign"
			case "missing-connections":
				info.Connections = nil
			case "wrong-source":
				c.Source = "cache"
			case "unavailable":
				c.Status = "unavailable"
			case "stale":
				c.ObservedAt -= 2000
			case "future":
				c.ObservedAt++
			case "zero-time":
				c.ObservedAt = 0
			case "truncated":
				c.Truncated = true
			case "missing-interfaces":
				c.Interfaces = nil
			case "oversize-interfaces":
				for len(c.Interfaces) < 129 {
					c.Interfaces = append(c.Interfaces, c.Interfaces[0])
				}
			case "five-aliases":
				info = cableTestInfo(t, now, 5)
			case "nonphysical":
				c.Interfaces[0].Physical = false
			case "untrusted-port-source":
				c.Interfaces[0].PhysicalPort.Source = "inferred"
			case "missing-switch":
				c.Interfaces[0].PhysicalPort.SwitchID = ""
			case "invalid-mac":
				c.Interfaces[0].MAC = "not-a-mac"
			case "zero-mac":
				c.Interfaces[0].MAC = "00:00:00:00:00:00"
			case "multicast-mac":
				c.Interfaces[0].MAC = "01:00:00:00:00:01"
			case "invalid-index":
				c.Interfaces[0].Index = 0
			case "duplicate-index":
				c.Interfaces[1].Index = c.Interfaces[0].Index
			case "duplicate-name":
				c.Interfaces[1].Name = c.Interfaces[0].Name
			}
			if _, err := projectCablePorts(info, "host-owner", "principal-owner", now); err == nil {
				t.Fatal("invalid native facts accepted")
			}
		})
	}
	t.Run("missing-physical-group-stays-unknown", func(t *testing.T) {
		info := cableTestInfo(t, now, 1)
		info.Connections.Interfaces[0].PhysicalPort = nil
		facts, err := projectCablePorts(info, "host-owner", "principal-owner", now)
		if err != nil || len(facts.Ports) != 0 {
			t.Fatalf("missing group fabricated a port: %+v %v", facts, err)
		}
		if _, err := selectCablePorts(facts, "host-owner", "principal-owner", cableTestSelection().Ports, 0); err == nil {
			t.Fatal("missing selected physical group accepted")
		}
	})
}

func TestNodeInfoConnectionContractFeedsDirectAndRingSelections(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	body, err := os.ReadFile(filepath.Join("..", "shared", "testdata", "node-info-connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	var info cableNodeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	facts, err := projectCablePorts(info, "host-owner", "principal-owner", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, ports := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d-port", ports), func(t *testing.T) {
			selection := []cableprobe.PortRef{}
			for port := 0; port < ports; port++ {
				name := fmt.Sprintf("p%d", port)
				selection = append(selection, cableprobe.PortRef{NodeID: "host-owner", SwitchID: "0011223344556677", PortName: name})
			}
			selected, err := selectCablePorts(facts, "host-owner", "principal-owner", selection, 0)
			if err != nil || len(selected) != ports {
				t.Fatalf("selected=%+v err=%v", selected, err)
			}
			for _, port := range selected {
				if len(port.Interfaces) != 2 {
					t.Fatalf("physical aliases=%+v", port)
				}
			}
			missing := append([]cableprobe.PortRef(nil), selection...)
			missing[0].SwitchID = "missing-switch"
			if _, err := selectCablePorts(facts, "host-owner", "principal-owner", missing, 0); err == nil {
				t.Fatal("missing physical port was accepted")
			}
		})
	}
}

func TestNodeInfoConnectionContractFeedsFabricSelections(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "shared", "testdata", "node-info-connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		HostUUID    string `json:"hostUuid"`
		Connections *struct {
			ObservedAt int64                     `json:"observedAt"`
			Interfaces []fabricObservedInterface `json:"interfaces"`
		} `json:"connections"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.Connections == nil {
		t.Fatal("shared node-info connection contract is invalid")
	}
	facts := fabricInventory{NodeID: wire.HostUUID, Principal: "principal-owner", ObservedAt: wire.Connections.ObservedAt, Interfaces: wire.Connections.Interfaces, Routes: []string{}}
	p0 := cableprobe.PortRef{NodeID: wire.HostUUID, SwitchID: "0011223344556677", PortName: "p0"}
	direct, err := fabricSelectedTarget(facts, p0)
	if err != nil || len(direct.Interfaces) != 2 {
		t.Fatalf("direct target=%+v err=%v", direct, err)
	}
	p1 := cableprobe.PortRef{NodeID: wire.HostUUID, SwitchID: p0.SwitchID, PortName: "p1"}
	ring, err := fabricSelectedRingTarget(facts, []cableprobe.PortRef{p0, p1})
	if err != nil || len(ring.Interfaces) != 2 || ring.Interfaces[0].Index != 2 || ring.Interfaces[1].Index != 3 {
		t.Fatalf("ring target=%+v err=%v", ring, err)
	}
	p0.SwitchID = "missing-switch"
	if _, err := fabricSelectedTarget(facts, p0); err == nil {
		t.Fatal("missing fabric physical port was accepted")
	}
}

func TestCablePortSelection(t *testing.T) {
	now := time.UnixMilli(100_000)
	for _, name := range []string{"valid-four-aliases", "final-millisecond", "host-mismatch", "principal-mismatch", "negative-age", "stale", "transit-expired", "negative-transit", "missing-ports", "empty-ports", "missing-selected-port", "five-aliases", "empty-aliases", "duplicate-port", "duplicate-index", "duplicate-name", "invalid-mac"} {
		t.Run(name, func(t *testing.T) {
			facts := cableTestSnapshot(t, now, 4)
			transit := time.Duration(0)
			switch name {
			case "final-millisecond":
				facts.AgeMs = 1999
			case "host-mismatch":
				facts.NodeID = "foreign"
			case "principal-mismatch":
				facts.Principal = "other-principal"
			case "negative-age":
				facts.AgeMs = -1
			case "stale":
				facts.AgeMs = 2000
			case "transit-expired":
				facts.AgeMs, transit = 1999, time.Millisecond
			case "negative-transit":
				transit = -time.Millisecond
			case "missing-ports":
				facts.Ports = nil
			case "empty-ports":
				facts.Ports = []cableprobe.Port{}
			case "missing-selected-port":
				facts.Ports[0].PortName = "other"
			case "five-aliases":
				facts.Ports[0].Interfaces = append(facts.Ports[0].Interfaces, cableprobe.Interface{Name: "eth5", Index: 5, MAC: "02:00:00:00:00:05"})
			case "empty-aliases":
				facts.Ports[0].Interfaces = nil
			case "duplicate-port":
				facts.Ports = append(facts.Ports, facts.Ports[0])
			case "duplicate-index":
				facts.Ports[0].Interfaces[1].Index = facts.Ports[0].Interfaces[0].Index
			case "duplicate-name":
				facts.Ports[0].Interfaces[1].Name = facts.Ports[0].Interfaces[0].Name
			case "invalid-mac":
				facts.Ports[0].Interfaces[0].MAC = "00:00:00:00:00:00"
			}
			ports, err := selectCablePorts(facts, "host-owner", "principal-owner", cableTestSelection().Ports, transit)
			valid := name == "valid-four-aliases" || name == "final-millisecond"
			if valid && (err != nil || len(ports) != 1 || len(ports[0].Interfaces) != 4) {
				t.Fatalf("valid facts rejected: %+v %v", ports, err)
			}
			if !valid && err == nil {
				t.Fatal("invalid participant facts selected")
			}
		})
	}
}

type cableTestReadBody struct {
	io.Reader
	closed bool
}

func (b *cableTestReadBody) Close() error { b.closed = true; return nil }

func TestCableReadJSON(t *testing.T) {
	for _, name := range []string{"valid", "exact-limit", "redirect", "http-error", "malformed", "trailing-json", "oversized", "transport-error"} {
		t.Run(name, func(t *testing.T) {
			payload, status := `{"ok":true}`, http.StatusOK
			switch name {
			case "exact-limit":
				payload += strings.Repeat(" ", cableFactsMaxBytes-len(payload))
			case "redirect":
				status = http.StatusFound
			case "http-error":
				status = http.StatusServiceUnavailable
			case "malformed":
				payload = `{"ok":`
			case "trailing-json":
				payload += `{}`
			case "oversized":
				payload = strings.Repeat(" ", cableFactsMaxBytes+1)
			}
			body := &cableTestReadBody{Reader: strings.NewReader(payload)}
			calls := 0
			client := &http.Client{Transport: cableTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Host != "synthetic.invalid" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if name == "transport-error" {
					return nil, errors.New("synthetic private transport detail")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://redirect.invalid/"}}, Body: body}, nil
			})}
			var result struct {
				OK bool `json:"ok"`
			}
			err := readCableJSON(context.Background(), client, "https://synthetic.invalid/facts", &result)
			valid := name == "valid" || name == "exact-limit"
			if valid && (err != nil || !result.OK) {
				t.Fatalf("valid bounded body rejected: %+v %v", result, err)
			}
			if !valid && err == nil {
				t.Fatal("invalid response accepted")
			}
			if calls != 1 {
				t.Fatalf("redirect or retry caused %d requests", calls)
			}
			if name != "transport-error" && !body.closed {
				t.Fatal("response body was not closed")
			}
			if err != nil && strings.Contains(err.Error(), "private transport detail") {
				t.Fatal("transport detail leaked into the returned error")
			}
		})
	}
}

func TestCableRemoteAgeRequired(t *testing.T) {
	for _, age := range []string{"absent", "null"} {
		t.Run(age, func(t *testing.T) {
			body, err := json.Marshal(cableTestSnapshot(t, time.Now(), 1))
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatal(err)
			}
			delete(wire, "ageMs")
			if age == "null" {
				wire["ageMs"] = json.RawMessage("null")
			}
			client := &http.Client{Transport: cableTestTransport(func(*http.Request) (*http.Response, error) { return cableTestResponse(t, wire), nil })}
			facts := cablePortSnapshot{AgeMs: -1}
			if err := readCableJSON(context.Background(), client, "https://synthetic.invalid/facts", &facts); err != nil {
				t.Fatal(err)
			}
			if _, err := selectCablePorts(facts, "host-owner", "principal-owner", cableTestSelection().Ports, 0); err == nil {
				t.Fatal("absent age became a fresh observation")
			}
		})
	}
}

func TestCableReviewBlockedParticipants(t *testing.T) {
	for _, name := range []string{"missing-peer", "unpinned-peer", "unclustered", "local-identity-mismatch", "local-stale", "local-truncated"} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			m, _, _ := cableTestManager(t, func(r *http.Request) (*http.Response, error) {
				reads++
				if r.Method != http.MethodGet || r.URL.String() != "http://127.0.0.1:14318/v1/node-info" {
					t.Errorf("unbounded local request: %s %s", r.Method, r.URL)
				}
				info := cableTestInfo(t, time.Now(), 1)
				switch name {
				case "local-identity-mismatch":
					info.HostUUID = "foreign"
				case "local-stale":
					info.Connections.ObservedAt -= 2000
				case "local-truncated":
					info.Connections.Truncated = true
				}
				return cableTestResponse(t, info), nil
			})
			if name == "unpinned-peer" {
				m.peers.peers["host-peer"] = ecPeer{nodeID: "host-peer", clusterUUID: "unpinned"}
			}
			if name == "unclustered" {
				m.mesh = clustertrust.Open(t.TempDir())
			}
			review, err := m.reviewCables(context.Background(), cableTestSelection())
			if err != nil {
				t.Fatal(err)
			}
			if review.Available || review.ConsumedRunID != "" || review.ReviewID == "" || review.OwnerNodeID != "host-owner" || review.RemainingMs <= 0 || review.RemainingMs > 30000 || len(review.Targets) != 2 || review.Reason != cableReviewBlocker {
				t.Fatalf("invalid unavailable review: %+v", review)
			}
			for _, target := range review.Targets {
				if target.RawPrivilege != "unknown" || target.Reason == "" {
					t.Fatalf("unsupported privilege or missing blocker: %+v", target)
				}
			}
			if len(review.Targets[1].Ports) != 0 || review.Targets[1].Principal != "" {
				t.Fatal("missing/unpinned peer acquired facts")
			}
			validLocal := name == "missing-peer" || name == "unpinned-peer"
			if validLocal && (len(review.Targets[0].Ports) != 1 || review.Targets[0].Principal != "principal-owner") {
				t.Fatal("current local physical facts lost")
			}
			if !validLocal && (len(review.Targets[0].Ports) != 0 || review.Targets[0].Principal != "") {
				t.Fatal("invalid local facts became a target")
			}
			if name == "unclustered" && reads != 0 {
				t.Fatal("unclustered review read a service")
			}
			if name == "missing-peer" {
				body, err := json.Marshal(review)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("SYNTHETIC_CABLE_REVIEW_FIXTURE=%s", body)
			}
		})
	}
}

func TestCableReviewDuplicatePrincipalsBeforeReads(t *testing.T) {
	for _, name := range []string{"owner-alias", "two-peer-aliases"} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			m, _, _ := cableTestManager(t, func(*http.Request) (*http.Response, error) { reads++; return nil, errors.New("unexpected read") })
			selection := cableTestSelection()
			m.peers.peers["host-peer"] = ecPeer{nodeID: "host-peer", clusterUUID: "principal-owner"}
			if name == "two-peer-aliases" {
				selection.NodeIDs[0], selection.Ports[0].NodeID = "host-other", "host-other"
				m.peers.peers["host-other"] = ecPeer{nodeID: "host-other", clusterUUID: "principal-peer"}
				m.peers.peers["host-peer"] = ecPeer{nodeID: "host-peer", clusterUUID: "principal-peer"}
			}
			if _, err := m.reviewCables(context.Background(), selection); err == nil {
				t.Fatal("distinct hosts shared a paired principal")
			}
			if reads != 0 {
				t.Fatal("duplicate identities reached a facts read")
			}
		})
	}
}

func TestCableReviewRPCBoundaries(t *testing.T) {
	valid, err := json.Marshal(cableTestSelection())
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{}`, `{"nodeIds":`, string(valid) + ` {}`, strings.TrimSuffix(string(valid), "}") + `,"principal":"supplied"}`, strings.Repeat(" ", 4097)} {
		var out bytes.Buffer
		m := NewManager(NewCodec(&out), NewExecutor(nil, nil, nil, t.TempDir()), nil)
		id := json.RawMessage("1")
		m.runCableReview(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:cable-review", Params: json.RawMessage(payload)})
		var reply struct {
			Error *RPCError `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Error == nil || reply.Error.Code != -32602 {
			t.Fatalf("invalid review request not rejected: %s", out.Bytes())
		}
	}
	for _, method := range []string{"engine:cable-start", "engine:cable-status", "engine:cable-cancel"} {
		var out bytes.Buffer
		m := NewManager(NewCodec(&out), NewExecutor(nil, nil, nil, t.TempDir()), nil)
		id := json.RawMessage("1")
		params := json.RawMessage(`{"reviewId":"synthetic"}`)
		if method == "engine:cable-cancel" {
			params = json.RawMessage(`{"runId":"synthetic"}`)
		}
		m.runCableProduct(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
		var reply struct {
			Error *RPCError `json:"error"`
		}
		if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Error == nil || reply.Error.Code != -32000 {
			t.Fatalf("execution lifecycle was not blocked: %s", out.Bytes())
		}
	}
}

func TestCableControlMuxReadOnlyAndPinned(t *testing.T) {
	reads := 0
	m, _, dir := cableTestManager(t, func(r *http.Request) (*http.Response, error) {
		reads++
		if r.URL.String() != "http://127.0.0.1:14318/v1/node-info" {
			t.Errorf("unexpected local endpoint: %s", r.URL)
		}
		return cableTestResponse(t, cableTestInfo(t, time.Now(), 1)), nil
	})
	identity, err := clustertrust.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(identity.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	strangerDir := t.TempDir()
	clustertrusttest.WriteKeypair(t, strangerDir, "principal-stranger")
	stranger, err := clustertrust.LoadIdentity(strangerDir)
	if err != nil {
		t.Fatal(err)
	}
	strangerLeaf, err := x509.ParseCertificate(stranger.Cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	mux := (&controlServer{mesh: m.mesh, cableLocal: m.cableLocal}).mux()
	for _, tc := range []struct {
		name, method string
		cert         *x509.Certificate
		status       int
	}{
		{"no-client", http.MethodGet, nil, http.StatusForbidden},
		{"unpinned-client", http.MethodGet, strangerLeaf, http.StatusForbidden},
		{"post", http.MethodPost, leaf, http.StatusMethodNotAllowed},
		{"get", http.MethodGet, leaf, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := reads
			r := httptest.NewRequest(tc.method, "https://synthetic.invalid"+controlCablePortsPath, nil)
			if tc.cert != nil {
				r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{tc.cert}}
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want %d", w.Code, tc.status)
			}
			if tc.status != http.StatusOK && reads != before {
				t.Fatal("rejected caller reached the facts service")
			}
			if tc.status == http.StatusMethodNotAllowed && w.Header().Get("Allow") != "GET" {
				t.Fatal("GET-only contract missing")
			}
			if tc.status == http.StatusOK {
				var facts cablePortSnapshot
				if err := json.Unmarshal(w.Body.Bytes(), &facts); err != nil {
					t.Fatal(err)
				}
				if facts.NodeID != "host-owner" || facts.Principal != "principal-owner" || len(facts.Ports) != 1 || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("incorrect read-only response: %+v", facts)
				}
				if strings.Contains(w.Body.String(), "rawPrivilege") {
					t.Fatal("passive facts asserted raw privilege")
				}
			}
		})
	}
}

func cableTestRemoteRead(t *testing.T, m *Manager, read cableTestTransport) {
	t.Helper()
	// Even a future multi-address fixture cannot reach a real dialer.
	m.addrs = reach.NewChooser(reach.WithDial(func(string, string, time.Duration) (net.Conn, error) {
		return nil, errors.New("synthetic address chooser; sockets prohibited")
	}))
	m.peers.peers["host-peer"] = ecPeer{nodeID: "host-peer", clusterUUID: "principal-peer", addresses: []string{"192.0.2.7"}, port: 14323}
	client, ok := m.remoteHTTP.Client("principal-peer")
	if !ok {
		t.Fatal("synthetic peer pin unavailable")
	}
	client.Transport = read
}

func cableTestPeerSnapshot(t *testing.T) cablePortSnapshot {
	t.Helper()
	facts := cableTestSnapshot(t, time.Now(), 1)
	facts.NodeID, facts.Principal = "host-peer", "principal-peer"
	facts.Ports[0].SwitchID, facts.Ports[0].PortName = "0011223344556688", "p1"
	return facts
}

func TestCableLocalSnapshotSamePrincipalRotation(t *testing.T) {
	var dir string
	m, _, fixtureDir := cableTestManager(t, func(*http.Request) (*http.Response, error) {
		clustertrusttest.WriteKeypair(t, dir, "principal-owner")
		return cableTestResponse(t, cableTestInfo(t, time.Now(), 1)), nil
	})
	dir = fixtureDir
	if _, err := m.cableLocal.snapshot(context.Background(), m.mesh); err == nil {
		t.Fatal("same-principal self certificate replacement retained the local snapshot")
	}
}

func TestCableReviewSamePrincipalRotations(t *testing.T) {
	for _, changed := range []string{"peer", "owner"} {
		t.Run(changed, func(t *testing.T) {
			m, _, dir := cableTestManager(t, func(*http.Request) (*http.Response, error) {
				return cableTestResponse(t, cableTestInfo(t, time.Now(), 1)), nil
			})
			reads := 0
			cableTestRemoteRead(t, m, func(r *http.Request) (*http.Response, error) {
				reads++
				if r.Method != http.MethodGet || r.URL.String() != "https://192.0.2.7:14323/v1/cables/ports" {
					t.Errorf("unexpected peer request: %s %s", r.Method, r.URL)
				}
				if changed == "peer" {
					clustertrusttest.WritePeerPin(t, dir, "principal-peer")
				} else {
					clustertrusttest.WriteKeypair(t, dir, "principal-owner")
				}
				return cableTestResponse(t, cableTestPeerSnapshot(t)), nil
			})
			review, err := m.reviewCables(context.Background(), cableTestSelection())
			if err != nil {
				t.Fatal(err)
			}
			if reads != 1 || review.Available {
				t.Fatalf("invalid read-only review: reads=%d available=%v", reads, review.Available)
			}
			for _, target := range review.Targets {
				if changed == "owner" || target.NodeID == "host-peer" {
					if target.Principal != "" || len(target.Ports) != 0 {
						t.Fatalf("same-principal %s certificate replacement retained target %s", changed, target.NodeID)
					}
				} else if len(target.Ports) != 1 {
					t.Fatal("unaffected owner port facts were lost")
				}
			}
		})
	}
}

func TestCableReviewLateParticipantExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _, _ := cableTestManager(t, func(*http.Request) (*http.Response, error) {
			return cableTestResponse(t, cableTestInfo(t, time.Now().Add(-time.Second), 1)), nil
		})
		cableTestRemoteRead(t, m, func(*http.Request) (*http.Response, error) {
			// Virtual time only: the earlier one-second-old snapshot expires,
			// while this request stays within the two-second transit budget.
			time.Sleep(1500 * time.Millisecond)
			return cableTestResponse(t, cableTestPeerSnapshot(t)), nil
		})
		review, err := m.reviewCables(context.Background(), cableTestSelection())
		if err != nil {
			t.Fatal(err)
		}
		if len(review.Targets[0].Ports) != 0 || !strings.Contains(review.Targets[0].Reason, "expired") {
			t.Fatal("earlier physical-port facts survived their lease while a later peer was read")
		}
		if len(review.Targets[1].Ports) != 1 || review.Targets[1].Principal != "principal-peer" {
			t.Fatal("later participant's fresh bounded facts were lost")
		}
		if review.Available {
			t.Fatal("passive review granted probe eligibility")
		}
	})
}
