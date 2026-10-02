// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
	if _, changed, ok := d.applyInfo("ghost", "127.0.0.1", 14323, gpus, nil, nil); ok || changed {
		t.Errorf("applyInfo on an absent node = (changed %v, ok %v), want (false, false)", changed, ok)
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
	if _, changed, ok := d.applyInfo("peer-I", "127.0.0.1", 14323, gpus, nil, nil); ok || changed {
		t.Errorf("applyInfo with a stale IP = (changed %v, ok %v), want (false, false)", changed, ok)
	}
	// node-info port changed -> also discarded.
	if _, changed, ok := d.applyInfo("peer-I", "10.0.0.9", 99999, gpus, nil, nil); ok || changed {
		t.Errorf("applyInfo with a stale node-info port = (changed %v, ok %v), want (false, false)", changed, ok)
	}
	got, _ := d.get("peer-I")
	if len(got.GPUs) != 1 || got.GPUs[0] != old[0] {
		t.Errorf("GPUs after a discarded apply = %v, want retained %v", got.GPUs, old)
	}

	// A node with no node-info service is not a valid target either.
	d.upsert(noderec.DirectoryNode{HostUUID: "peer-NoNI", IP: "10.0.0.10"})
	if _, changed, ok := d.applyInfo("peer-NoNI", "10.0.0.10", 14323, gpus, nil, nil); ok || changed {
		t.Errorf("applyInfo on a node without node-info = (changed %v, ok %v), want (false, false)", changed, ok)
	}
}

// TestApplyInfoChangedCheck covers the reason applyInfo exists rather than a
// plain write: a caller polling on a timer must be able to tell a real change
// from an identical re-read, so it republishes once per change and not once per
// tick. It also pins the nil-is-not-zero rule, because a node that stopped
// reporting a facet must not look like one reporting idle hardware.
func TestApplyInfoChangedCheck(t *testing.T) {
	d := newDirectory()
	gpus := []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30, VramUsedBytes: 128 << 20}}
	cpu := &noderec.CPUInfo{Name: "Ryzen", Cores: 16}
	mem := &noderec.MemoryInfo{TotalBytes: 64 << 30, UsedBytes: 8 << 30}
	d.upsert(noderec.DirectoryNode{
		HostUUID: "peer-C",
		IP:       "10.0.0.11",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceNodeInfo: {Port: 14323}},
		GPUs:     gpus,
		CPU:      cpu,
		Memory:   mem,
	})

	// Identical re-read: valid, but nothing to announce.
	same := []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30, VramUsedBytes: 128 << 20}}
	if _, changed, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, same,
		&noderec.CPUInfo{Name: "Ryzen", Cores: 16},
		&noderec.MemoryInfo{TotalBytes: 64 << 30, UsedBytes: 8 << 30}); !ok || changed {
		t.Errorf("applyInfo with identical figures = (changed %v, ok %v), want (false, true)", changed, ok)
	}

	// A single moved number is a change: this is the 0.12 GB -> 9.1 GB case.
	filled := []noderec.GPUInfo{{Name: "RTX 3060", VramBytes: 12 << 30, VramUsedBytes: 9100 << 20}}
	node, changed, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, filled, cpu, mem)
	if !ok || !changed {
		t.Fatalf("applyInfo with a filled card = (changed %v, ok %v), want (true, true)", changed, ok)
	}
	if node.GPUs[0].VramUsedBytes != 9100<<20 {
		t.Errorf("returned VramUsedBytes = %d, want %d", node.GPUs[0].VramUsedBytes, uint64(9100)<<20)
	}
	stored, _ := d.get("peer-C")
	if stored.GPUs[0].VramUsedBytes != 9100<<20 {
		t.Errorf("stored VramUsedBytes = %d, want %d", stored.GPUs[0].VramUsedBytes, uint64(9100)<<20)
	}

	// Dropping a facet entirely is a change, and is not the same as zeroing it.
	if _, changed, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, filled, nil, mem); !ok || !changed {
		t.Errorf("applyInfo dropping CPU = (changed %v, ok %v), want (true, true)", changed, ok)
	}
	if _, changed, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, filled, &noderec.CPUInfo{}, mem); !ok || !changed {
		t.Errorf("applyInfo replacing a nil CPU with a zeroed one = (changed %v, ok %v), want (true, true)", changed, ok)
	}

	// A card appearing or vanishing is a change even when the rest matches.
	if _, changed, ok := d.applyInfo("peer-C", "10.0.0.11", 14323, nil, &noderec.CPUInfo{}, mem); !ok || !changed {
		t.Errorf("applyInfo dropping the GPU list = (changed %v, ok %v), want (true, true)", changed, ok)
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
// It asserts the other half of the contract too: a re-read that finds the same
// figures reports no change, so the record is not republished on every tick.
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
	d.reg.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: port})
	d.publishSelf()

	// Guard the rest of the test: if the seed enrichment silently failed, a later
	// "no change" would pass for the wrong reason.
	seeded, ok := d.dir.get("self-uuid")
	if !ok {
		t.Fatal("self missing from directory after publishSelf")
	}
	if len(seeded.GPUs) != 1 || seeded.GPUs[0].VramUsedBytes != 128974848 {
		t.Fatalf("seed enrichment did not take: GPUs = %v", seeded.GPUs)
	}

	// Nothing has moved: a tick must not republish.
	if changed := d.refreshSelfInfoOnce(); changed {
		t.Error("refreshSelfInfoOnce on unchanged figures reported a change; self would be republished every tick")
	}

	// A model is loaded and the card fills. This is what used to stay invisible.
	mu.Lock()
	served.GPUs = []GPUInfo{{Name: "NVIDIA GeForce RTX 3060", VramBytes: 12884901888, VramUsedBytes: 9542041600}}
	mu.Unlock()

	if changed := d.refreshSelfInfoOnce(); !changed {
		t.Fatal("refreshSelfInfoOnce did not notice the local card filling; the staleness bug is back")
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

	// Converged: the next tick is quiet again.
	if changed := d.refreshSelfInfoOnce(); changed {
		t.Error("refreshSelfInfoOnce reported a second change for the same reading")
	}
}
