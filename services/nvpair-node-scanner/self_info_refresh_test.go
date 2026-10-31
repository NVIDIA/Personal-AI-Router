// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"nvpair-shared/noderec"
)

// TestApplyInfoGuard mirrors TestApplyModelsGuard at the node-info facet: a
// completed fetch is discarded (ok == false) when the node was removed or
// re-addressed while it was in flight, so it can neither resurrect a removed node
// nor write figures read from an address the node no longer answers on.
func TestApplyInfoGuard(t *testing.T) {
	d := newDirectory()
	gpus := []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30}}

	// Removed mid-sweep: node absent -> not resurrected.
	if _, ok := d.applyInfo("ghost", "127.0.0.1", 14323, gpus, nil, nil); ok {
		t.Error("applyInfo on an absent node reported ok, want discarded")
	}
	if _, present := d.get("ghost"); present {
		t.Error("applyInfo must not resurrect a removed node")
	}

	old := []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30, VramUsedBytes: 128 << 20}}
	d.upsert(noderec.DirectoryNode{
		HostUUID: "peer-I",
		IP:       "10.0.0.9",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14323}},
		GPUs:     old,
	})

	// Re-addressed mid-sweep: node present but IP changed -> not overwritten.
	if _, ok := d.applyInfo("peer-I", "127.0.0.1", 14323, gpus, nil, nil); ok {
		t.Error("applyInfo with a stale IP reported ok, want discarded")
	}
	// node-info port changed -> also discarded.
	if _, ok := d.applyInfo("peer-I", "10.0.0.9", 99999, gpus, nil, nil); ok {
		t.Error("applyInfo with a stale node-info port reported ok, want discarded")
	}
	got, _ := d.get("peer-I")
	if len(got.GPUs) != 1 || got.GPUs[0] != old[0] {
		t.Errorf("GPUs after a discarded apply = %v, want retained %v", got.GPUs, old)
	}

	// A node with no node-info service is not a valid target either.
	d.upsert(noderec.DirectoryNode{HostUUID: "peer-NoNI", IP: "10.0.0.10"})
	if _, ok := d.applyInfo("peer-NoNI", "10.0.0.10", 14323, gpus, nil, nil); ok {
		t.Error("applyInfo on a node without node-info reported ok, want discarded")
	}
}

// TestApplyInfoWrites covers the write itself. applyInfo is a plain write behind
// the guard: a valid apply stores exactly the figures handed to it and returns
// them, an identical re-read is just as valid, and a facet the caller no longer
// has is stored as absent rather than carried over from the previous read.
func TestApplyInfoWrites(t *testing.T) {
	d := newDirectory()
	cpu := &noderec.CPUInfo{Name: "Ryzen", Cores: 16}
	mem := &noderec.MemoryInfo{TotalBytes: 64 << 30, UsedBytes: 8 << 30}
	d.upsert(noderec.DirectoryNode{
		HostUUID: "peer-C",
		IP:       "10.0.0.11",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14323}},
		GPUs:     []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30, VramUsedBytes: 128 << 20}},
		CPU:      cpu,
		Memory:   mem,
	})

	// The 0.12 GB -> 9.1 GB case: the new figure is stored and returned.
	filled := []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30, VramUsedBytes: 9100 << 20}}
	node, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, filled, cpu, mem)
	if !ok {
		t.Fatal("applyInfo on a valid node was discarded")
	}
	if node.GPUs[0].VramUsedBytes != 9100<<20 {
		t.Errorf("returned VramUsedBytes = %d, want %d", node.GPUs[0].VramUsedBytes, uint64(9100)<<20)
	}
	stored, _ := d.get("peer-C")
	if stored.GPUs[0].VramUsedBytes != 9100<<20 {
		t.Errorf("stored VramUsedBytes = %d, want %d", stored.GPUs[0].VramUsedBytes, uint64(9100)<<20)
	}

	// An identical re-read is written like any other and leaves the same figures.
	again := []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30, VramUsedBytes: 9100 << 20}}
	if _, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, again, cpu, mem); !ok {
		t.Fatal("applyInfo with identical figures was discarded")
	}
	stored, _ = d.get("peer-C")
	if stored.GPUs[0].VramUsedBytes != 9100<<20 || stored.CPU == nil || stored.Memory == nil {
		t.Errorf("after an identical re-read: GPUs %v, CPU %v, Memory %v", stored.GPUs, stored.CPU, stored.Memory)
	}

	// A facet node-info stopped reporting is written as absent, not carried over.
	if _, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, filled, nil, mem); !ok {
		t.Fatal("applyInfo dropping CPU was discarded")
	}
	stored, _ = d.get("peer-C")
	if stored.CPU != nil {
		t.Errorf("CPU after a nil apply = %+v, want nil", stored.CPU)
	}
}

// TestRefreshSelfInfoOnceConvergesLocalNode is the regression test for the bug
// this path exists to fix: the local node's hardware figures were captured once
// and then never refreshed. onBrowse returns early on our own uuid, so self never
// gets the per-browse re-enrichment every peer gets, and publishSelf only runs
// when the registry moves. node-info served the true value throughout; nothing
// carried it into the directory.
//
// The stub listens on loopback while the node advertises an (unreachable-in-test)
// LAN ip, so this also pins the property publishSelfLocked documents: self
// enrichment dials loopback, never the advertised address.
//
// It pins the other half of the contract too: every valid tick republishes the
// entry, unchanged figures included, as exactly one discovery:node-updated.
func TestRefreshSelfInfoOnceConvergesLocalNode(t *testing.T) {
	var mu sync.Mutex
	served := NodeInfoResponse{
		GPUs:   []GPUInfo{{Name: "NVIDIA GeForce RTX 3060", VramBytes: 12884901888, VramUsedBytes: 128974848}},
		CPU:    &CPUInfo{Name: "AMD Ryzen 7 9800X3D", Cores: 8},
		Memory: &MemoryInfo{TotalBytes: 68719476736, UsedBytes: 8589934592},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		body := served
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split stub addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	d := newSelfTestDaemon("self-uuid", "192.168.1.17")
	var output bytes.Buffer
	d.codec = NewCodec(&output)
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: port})
	d.publishSelf()

	// Guard the rest of the test: if the seed enrichment silently failed, a later
	// refresh would pass for the wrong reason.
	seeded, ok := d.dir.get("self-uuid")
	if !ok {
		t.Fatal("self missing from directory after publishSelf")
	}
	if len(seeded.GPUs) != 1 || seeded.GPUs[0].VramUsedBytes != 128974848 {
		t.Fatalf("seed enrichment did not take: GPUs = %v", seeded.GPUs)
	}
	// publishSelf's own event is not under test.
	output.Reset()

	// Nothing has moved. The tick still refreshes and republishes: there is no
	// compare, and an unchanged node-updated is the intended steady state.
	if !d.refreshSelfInfoOnce() {
		t.Fatal("refreshSelfInfoOnce on a seeded self did not refresh")
	}
	if updates := nodeUpdates(t, &output); len(updates) != 1 {
		t.Fatalf("node-updated events after an unchanged tick = %d, want 1", len(updates))
	}
	output.Reset()

	// A model is loaded and the card fills. This is what used to stay invisible.
	mu.Lock()
	served.GPUs = []GPUInfo{{Name: "NVIDIA GeForce RTX 3060", VramBytes: 12884901888, VramUsedBytes: 9542041600}}
	mu.Unlock()

	if !d.refreshSelfInfoOnce() {
		t.Fatal("refreshSelfInfoOnce did not refresh after the local card filled; the staleness bug is back")
	}
	got, ok := d.dir.get("self-uuid")
	if !ok {
		t.Fatal("self vanished from the directory")
	}
	if len(got.GPUs) != 1 || got.GPUs[0].VramUsedBytes != 9542041600 {
		t.Errorf("self GPUs = %v, want VramUsedBytes 9542041600", got.GPUs)
	}
	if got.IP != "192.168.1.17" {
		t.Errorf("self display IP = %q, want the advertised LAN address left untouched", got.IP)
	}
	updates := nodeUpdates(t, &output)
	if len(updates) != 1 {
		t.Fatalf("node-updated events after the card filled = %d, want 1", len(updates))
	}
	if u := updates[0]; u.HostUUID != "self-uuid" || len(u.GPUs) != 1 || u.GPUs[0].VramUsedBytes != 9542041600 {
		t.Errorf("published self = %+v, want the filled card", u)
	}
}

// nodeUpdates decodes every notification the codec wrote since the buffer was
// last reset and returns the nodes carried by discovery:node-updated, failing on
// any other method.
func nodeUpdates(t *testing.T, out *bytes.Buffer) []noderec.DirectoryNode {
	t.Helper()
	var nodes []noderec.DirectoryNode
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Fatalf("decode notification: %v", err)
		}
		if msg.Method != noderec.NotifyNodeUpdated {
			t.Fatalf("notification method = %q, want %q", msg.Method, noderec.NotifyNodeUpdated)
		}
		var ev noderec.NodeEvent
		if err := json.Unmarshal(msg.Params, &ev); err != nil {
			t.Fatalf("decode node event: %v", err)
		}
		nodes = append(nodes, ev.Node)
	}
	return nodes
}
