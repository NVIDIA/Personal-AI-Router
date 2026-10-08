// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestRegistryRegisterAndRecord(t *testing.T) {
	r := newRegistry("host-1", "clu-1", []string{"192.168.1.10"})

	require.True(t, r.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}), "first register should report a change")
	// Idempotent re-register of the same port is not a change.
	assert.False(t, r.register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318}), "re-registering the same port should not report a change")
	// A missing service/port is ignored.
	assert.False(t, r.register(noderec.RegisterParams{Service: noderec.ServiceErrors, Port: 0}), "register with port 0 should be ignored")

	r.register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434})

	rec := r.record()
	require.Equal(t, "host-1", rec.HostUUID, "identity wrong (%v)", rec)
	require.Equal(t, "clu-1", rec.ClusterUUID, "identity wrong (%v)", rec)
	require.Equal(t, "192.168.1.10", rec.IP, "identity wrong (%v)", rec)
	{
		p, ok := rec.Port(noderec.ServiceNodeInfo)
		assert.True(t, ok, "ni port (%v, %v)", p, ok)
		assert.Equal(t, 14318, p, "ni port (%v, %v)", p, ok)
	}
	{
		p, ok := rec.Port(noderec.ServiceOllama)
		assert.True(t, ok, "ol port (%v, %v)", p, ok)
		assert.Equal(t, 11434, p, "ol port (%v, %v)", p, ok)
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := newRegistry("h", "", nil)
	r.register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434})
	require.True(t, r.unregister(noderec.ServiceOllama), "unregister should report a change")
	assert.False(t, r.unregister(noderec.ServiceOllama), "unregister of absent service should not report a change")
	{
		_, ok := r.record().Port(noderec.ServiceOllama)
		assert.False(t, ok, "ol should be gone")
	}
}

func TestRegistrySetIdentity(t *testing.T) {
	r := newRegistry("old", "", []string{"1.1.1.1"})
	require.True(t, r.setIdentity("new", "clu", []string{"2.2.2.2"}), "identity change should report a change")
	assert.False(t, r.setIdentity("new", "clu", []string{"2.2.2.2"}), "no-op identity set should not report a change")
	rec := r.record()
	require.Equal(t, "new", rec.HostUUID, "identity not applied (%v)", rec)
	require.Equal(t, "clu", rec.ClusterUUID, "identity not applied (%v)", rec)
	require.Equal(t, "2.2.2.2", rec.IP, "identity not applied (%v)", rec)
}

// TestRegistrySetAddresses covers the periodic re-rank path: the canonical address
// is the list's head, and a reordering IS a change — it names a different
// canonical address, which is exactly what must be republished.
func TestRegistrySetAddresses(t *testing.T) {
	r := newRegistry("h", "", []string{"192.168.240.2", "10.0.0.5"})
	{
		got := r.record().IP
		require.Equal(t, "192.168.240.2", got, "canonical")
	}
	require.True(t, r.setAddresses([]string{"10.0.0.5", "192.168.240.2"}), "reordering should report a change")
	assert.False(t, r.setAddresses([]string{"10.0.0.5", "192.168.240.2"}), "no-op address set should not report a change")
	rec := r.record()
	assert.Equal(t, "10.0.0.5", rec.IP, "canonical after re-rank")
	require.Len(t, rec.IPs, 2)
	assert.Equal(t, "192.168.240.2", rec.IPs[1])
	// A host that loses every address must report that, not keep a stale one.
	assert.True(t, r.setAddresses(nil), "dropping every address should report a change")
	{
		rec := r.record()
		assert.Equal(t, "", rec.IP, "record with no addresses (%v)", rec)
		assert.Len(t, rec.IPs, 0, "record with no addresses (%v)", rec)
	}
}

// TestRegistryCapsAdvertisedAddresses: the registry never holds addresses the wire
// would drop, so what a peer receives is what this node believes it published.
func TestRegistryCapsAdvertisedAddresses(t *testing.T) {
	many := []string{"10.0.0.1", "10.0.1.1", "10.0.2.1", "10.0.3.1", "10.0.4.1", "10.0.5.1"}
	r := newRegistry("h", "", many)
	{
		got := len(r.record().IPs)
		require.True(t, got == noderec.MaxAdvertisedIPs, "held (%v)", got)
	}
	{
		got := r.record().IP
		assert.Equal(t, "10.0.0.1", got, "canonical")
	}
}

func TestRegistryTXTIsValidRecord(t *testing.T) {
	r := newRegistry("h", "clu", []string{"10.0.0.1", "192.168.240.2"})
	r.register(noderec.RegisterParams{Service: noderec.ServiceErrors, Port: 14319})
	// The built TXT must parse back to an equivalent record.
	got := noderec.ParseTXT(r.txt())
	require.Equal(t, "h", got.HostUUID, "round-trip identity wrong (%v)", got)
	require.Equal(t, "clu", got.ClusterUUID, "round-trip identity wrong (%v)", got)
	require.Equal(t, "10.0.0.1", got.IP, "round-trip identity wrong (%v)", got)
	assert.Equal(t, []string{"10.0.0.1", "192.168.240.2"}, got.IPs, "candidate list lost in round-trip")
	{
		p, ok := got.Port(noderec.ServiceErrors)
		assert.True(t, ok, "er port lost in round-trip (%v, %v)", p, ok)
		assert.Equal(t, 14319, p, "er port lost in round-trip (%v, %v)", p, ok)
	}
}
