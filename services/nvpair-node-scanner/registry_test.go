// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"nvpair-shared/noderec"
)

func TestRegistryRegisterAndRecord(t *testing.T) {
	r := newRegistry("host-1", "clu-1", []string{"192.168.1.10"})

	if !r.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}) {
		t.Fatal("first register should report a change")
	}
	// Idempotent re-register of the same port is not a change.
	if r.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}) {
		t.Error("re-registering the same port should not report a change")
	}
	// A missing service/port is ignored.
	if r.register(noderec.RegisterParams{Service: noderec.ServiceErrors, Port: 0}) {
		t.Error("register with port 0 should be ignored")
	}

	r.register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434})

	rec := r.record()
	if rec.HostUUID != "host-1" || rec.ClusterUUID != "clu-1" || rec.IP != "192.168.1.10" {
		t.Fatalf("identity wrong: %+v", rec)
	}
	if p, ok := rec.Port(noderec.ServiceNodeInfo); !ok || p != 14318 {
		t.Errorf("ni port = %d,%v", p, ok)
	}
	if p, ok := rec.Port(noderec.ServiceOllama); !ok || p != 11434 {
		t.Errorf("ol port = %d,%v", p, ok)
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := newRegistry("h", "", nil)
	r.register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434})
	if !r.unregister(noderec.ServiceOllama) {
		t.Fatal("unregister should report a change")
	}
	if r.unregister(noderec.ServiceOllama) {
		t.Error("unregister of absent service should not report a change")
	}
	if _, ok := r.record().Port(noderec.ServiceOllama); ok {
		t.Error("ol should be gone")
	}
}

func TestRegistrySetIdentity(t *testing.T) {
	r := newRegistry("old", "", []string{"1.1.1.1"})
	if !r.setIdentity("new", "clu", []string{"2.2.2.2"}) {
		t.Fatal("identity change should report a change")
	}
	if r.setIdentity("new", "clu", []string{"2.2.2.2"}) {
		t.Error("no-op identity set should not report a change")
	}
	rec := r.record()
	if rec.HostUUID != "new" || rec.ClusterUUID != "clu" || rec.IP != "2.2.2.2" {
		t.Fatalf("identity not applied: %+v", rec)
	}
}

// TestRegistrySetAddresses covers the periodic re-rank path: the canonical address
// is the list's head, and a reordering IS a change — it names a different
// canonical address, which is exactly what must be republished.
func TestRegistrySetAddresses(t *testing.T) {
	r := newRegistry("h", "", []string{"192.168.240.2", "10.0.0.5"})
	if got := r.record().IP; got != "192.168.240.2" {
		t.Fatalf("canonical = %q, want the list head", got)
	}
	if !r.setAddresses([]string{"10.0.0.5", "192.168.240.2"}) {
		t.Fatal("reordering should report a change")
	}
	if r.setAddresses([]string{"10.0.0.5", "192.168.240.2"}) {
		t.Error("no-op address set should not report a change")
	}
	rec := r.record()
	if rec.IP != "10.0.0.5" {
		t.Errorf("canonical after re-rank = %q, want 10.0.0.5", rec.IP)
	}
	if len(rec.IPs) != 2 || rec.IPs[1] != "192.168.240.2" {
		t.Errorf("candidates = %v, want both addresses in the new order", rec.IPs)
	}
	// A host that loses every address must report that, not keep a stale one.
	if !r.setAddresses(nil) {
		t.Error("dropping every address should report a change")
	}
	if rec := r.record(); rec.IP != "" || len(rec.IPs) != 0 {
		t.Errorf("record with no addresses = %+v, want empty", rec)
	}
}

// TestRegistryCapsAdvertisedAddresses: the registry never holds addresses the wire
// would drop, so what a peer receives is what this node believes it published.
func TestRegistryCapsAdvertisedAddresses(t *testing.T) {
	many := []string{"10.0.0.1", "10.0.1.1", "10.0.2.1", "10.0.3.1", "10.0.4.1", "10.0.5.1"}
	r := newRegistry("h", "", many)
	if got := len(r.record().IPs); got != noderec.MaxAdvertisedIPs {
		t.Fatalf("held %d addresses, want the cap of %d", got, noderec.MaxAdvertisedIPs)
	}
	if got := r.record().IP; got != "10.0.0.1" {
		t.Errorf("canonical = %q, want the highest-ranked address preserved", got)
	}
}

func TestRegistryTXTIsValidRecord(t *testing.T) {
	r := newRegistry("h", "clu", []string{"10.0.0.1", "192.168.240.2"})
	r.register(noderec.RegisterParams{Service: noderec.ServiceErrors, Port: 14319})
	// The built TXT must parse back to an equivalent record.
	got := noderec.ParseTXT(r.txt())
	if got.HostUUID != "h" || got.ClusterUUID != "clu" || got.IP != "10.0.0.1" {
		t.Fatalf("round-trip identity wrong: %+v", got)
	}
	if len(got.IPs) != 2 || got.IPs[0] != "10.0.0.1" || got.IPs[1] != "192.168.240.2" {
		t.Errorf("candidate list lost in round-trip: %v", got.IPs)
	}
	if p, ok := got.Port(noderec.ServiceErrors); !ok || p != 14319 {
		t.Errorf("er port lost in round-trip: %d,%v", p, ok)
	}
}
