// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// setLevelFrame and identityFrame build the two stdin notifications this service
// receives, so a test drives the real handler rather than a hand-built struct.
func identityFrame(t *testing.T, clusterUUID string) applog.StdinMessage {
	t.Helper()
	params, err := json.Marshal(noderec.ClusterIdentityParams{ClusterUUID: clusterUUID})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
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
	if uuid, told := identity.get(); told || uuid != "" {
		t.Fatalf("fresh identity = (%q, told=%v), want an empty principal and told=false", uuid, told)
	}

	// A departure is a real value and must report as present-and-empty.
	identity.set("")
	uuid, told := identity.get()
	if !told {
		t.Error("identity pushed as empty still reports as not told")
	}
	if uuid != "" {
		t.Errorf("uuid = %q, want empty", uuid)
	}
}

// TestHandleClusterIdentityAppliesPush drives the real stdin handler for the
// three cases it sees: a principal, a departure, and a frame that is not ours.
func TestHandleClusterIdentityAppliesPush(t *testing.T) {
	var identity clusterIdentity

	handleClusterIdentity(identityFrame(t, "our-principal"), &identity)
	if uuid, told := identity.get(); !told || uuid != "our-principal" {
		t.Fatalf("after push = (%q, told=%v), want (our-principal, true)", uuid, told)
	}

	handleClusterIdentity(identityFrame(t, ""), &identity)
	if uuid, told := identity.get(); !told || uuid != "" {
		t.Fatalf("after departure = (%q, told=%v), want an empty principal and told=true", uuid, told)
	}

	// Another method must not touch it. log/set-level never reaches this handler
	// in production (applog dispatches it first), but a wrong method here would
	// silently reset membership, so the guard is worth pinning.
	handleClusterIdentity(identityFrame(t, "restored"), &identity)
	handleClusterIdentity(applog.StdinMessage{Method: applog.SetLevelMethod}, &identity)
	if uuid, _ := identity.get(); uuid != "restored" {
		t.Errorf("uuid = %q after an unrelated frame, want restored", uuid)
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

	if uuid, told := identity.get(); !told || uuid != "our-principal" {
		t.Errorf("after malformed push = (%q, told=%v), want the prior value kept", uuid, told)
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
		if err := json.Unmarshal(buildResponse(nil, nil, 0, statsSnapshot{}, "host", clusterUUID, nil), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	if _, present := raw(nil)["clusterUuid"]; present {
		t.Error("unknown membership emitted a clusterUuid key; a peer would read it as a claim")
	}

	unclustered := ""
	got := raw(&unclustered)
	value, present := got["clusterUuid"]
	if !present {
		t.Fatal("unclustered membership omitted clusterUuid; a peer cannot tell it apart from unknown")
	}
	if value != "" {
		t.Errorf("clusterUuid = %v, want empty", value)
	}

	principal := "our-principal"
	if got := raw(&principal)["clusterUuid"]; got != "our-principal" {
		t.Errorf("clusterUuid = %v, want our-principal", got)
	}
}
