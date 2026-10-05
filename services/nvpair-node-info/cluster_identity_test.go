// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// setLevelFrame and identityFrame build the two stdin notifications this service
// receives, so a test drives the real handler rather than a hand-built struct.
func identityFrame(t *testing.T, clusterUUID string) applog.StdinMessage {
	t.Helper()
	params, err := json.Marshal(noderec.ClusterIdentityParams{ClusterUUID: clusterUUID})
	require.NoError(t, err, "marshal params")
	return applog.StdinMessage{Method: noderec.MethodSetClusterIdentity, Params: params}
}

// TestClusterIdentityStartsUnknown pins the producer half of the absent-vs-empty
// contract. Before the broker has pushed anything this service has no basis to
// claim either state, and a peer reads a reported empty principal as "belongs to
// no cluster" — which would have it clear a correct annotation and offer an invite
// its target rejects. So "not yet told" has to stay absent on the wire, not
// collapse into the empty string it happens to be stored as.
func TestClusterIdentityStartsUnknown(t *testing.T) {
	var identity clusterIdentity
	{
		uuid, told := identity.get()
		require.False(t, told, "fresh identity (%v, %v)", uuid, told)
		require.Equal(t, "", uuid, "fresh identity (%v, %v)", uuid, told)
	}

	// A departure is a real value and must report as present-and-empty.
	identity.set("")
	uuid, told := identity.get()
	assert.True(t, told, "identity pushed as empty still reports as not told")
	assert.Equal(t, "", uuid, "uuid")
}

// TestHandleClusterIdentityAppliesPush drives the real stdin handler for the
// three cases it sees: a principal, a departure, and a frame that is not ours.
func TestHandleClusterIdentityAppliesPush(t *testing.T) {
	var identity clusterIdentity

	handleClusterIdentity(identityFrame(t, "our-principal"), &identity)
	{
		uuid, told := identity.get()
		require.True(t, told, "after push (%v, %v)", uuid, told)
		require.Equal(t, "our-principal", uuid, "after push (%v, %v)", uuid, told)
	}

	handleClusterIdentity(identityFrame(t, ""), &identity)
	{
		uuid, told := identity.get()
		require.True(t, told, "after departure (%v, %v)", uuid, told)
		require.Equal(t, "", uuid, "after departure (%v, %v)", uuid, told)
	}

	// Another method must not touch it. log/set-level never reaches this handler
	// in production (applog dispatches it first), but a wrong method here would
	// silently reset membership, so the guard is worth pinning.
	handleClusterIdentity(identityFrame(t, "restored"), &identity)
	handleClusterIdentity(applog.StdinMessage{Method: applog.SetLevelMethod}, &identity)
	{
		uuid, _ := identity.get()
		assert.Equal(t, "restored", uuid, "uuid")
	}
}

// TestHandleClusterIdentityIgnoresMalformed keeps a bad payload from latching a
// wrong membership: the broker re-pushes on every change, so dropping it is
// recoverable while acting on it is not.
func TestHandleClusterIdentityIgnoresMalformed(t *testing.T) {
	var identity clusterIdentity
	handleClusterIdentity(identityFrame(t, "our-principal"), &identity)

	handleClusterIdentity(applog.StdinMessage{
		Method: noderec.MethodSetClusterIdentity,
		Params: json.RawMessage(`"not-an-object"`),
	}, &identity)

	{
		uuid, told := identity.get()
		assert.True(t, told, "after malformed push (%v, %v)", uuid, told)
		assert.Equal(t, "our-principal", uuid, "after malformed push (%v, %v)", uuid, told)
	}
}

// TestBuildResponseClusterUUIDWireStates is the encoder half of the contract the
// consumer depends on, asserted through the real JSON path. A consumer treats an
// absent clusterUuid as "unknown, leave the annotation alone" and a present-empty
// one as "unclustered", so adding omitempty to a non-pointer field — or dropping
// the pointer — would silently stop peers converging while every other test in the
// tree still passed.
func TestBuildResponseClusterUUIDWireStates(t *testing.T) {
	raw := func(clusterUUID *string) map[string]any {
		t.Helper()
		var out map[string]any
		require.NoError(t, json.Unmarshal(buildResponse(nil, nil, 0, statsSnapshot{}, "host", clusterUUID), &out), "decode")
		return out
	}

	{
		_, present := raw(nil)["clusterUuid"]
		assert.False(t, present, "unknown membership emitted a clusterUuid key; a peer would read it as a claim")
	}

	unclustered := ""
	got := raw(&unclustered)
	value, present := got["clusterUuid"]
	require.True(t, present, "unclustered membership omitted clusterUuid; a peer cannot tell it apart from unknown")
	assert.Equal(t, "", value, "clusterUuid")

	principal := "our-principal"
	{
		got := raw(&principal)["clusterUuid"]
		assert.Equal(t, "our-principal", got, "clusterUuid")
	}
}
