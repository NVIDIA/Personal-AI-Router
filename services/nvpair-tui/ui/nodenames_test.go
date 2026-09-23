// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"nvpair-tui/rpc"
)

// identityChanged builds the cluster:identity-changed push the manager emits.
func identityChanged(id, friendly string) *rpc.Message {
	params, _ := json.Marshal(map[string]string{
		"clusterId":           id,
		"clusterFriendlyName": friendly,
	})
	return &rpc.Message{Method: "cluster:identity-changed", Params: params}
}

// A realistic value: the broker stamps its resolveLocalNodeID, a nodeid UUID.
const sampleNodeUUID = "3f2a91c4-7b1e-4d55-9a02-8c6f1e2b7d40"

// TestNamerResolvesFromDiscovery is the regression guard for the jobs list
// showing a random-looking string: discovery carries hostUuid alongside name, so
// the id the workload manager reports is resolvable without any extra request.
func TestNamerResolvesFromDiscovery(t *testing.T) {
	n := newNodeNamer()
	if got := n.name(sampleNodeUUID); got == "workstation-01" {
		t.Fatal("resolved before learning anything")
	}

	n.learnDiscovered([]availableNode{{HostUUID: sampleNodeUUID, Name: "workstation-01"}})
	if got := n.name(sampleNodeUUID); got != "workstation-01" {
		t.Errorf("name = %q, want the discovered name", got)
	}
}

// TestNamerResolvesFromMembership covers a peer known from the cluster roster
// but absent from the current discovery snapshot.
func TestNamerResolvesFromMembership(t *testing.T) {
	n := newNodeNamer()
	n.learnMembers([]clusterNode{{NodeUUID: sampleNodeUUID, Name: "peer-a"}})
	if got := n.name(sampleNodeUUID); got != "peer-a" {
		t.Errorf("name = %q, want the member name", got)
	}
}

// TestNamerFallsBackToShortID checks an unresolved id is shortened rather than
// printed in full, and is still distinguishable from a real name.
func TestNamerFallsBackToShortID(t *testing.T) {
	n := newNodeNamer()
	got := n.name(sampleNodeUUID)

	if got == sampleNodeUUID {
		t.Error("unresolved id rendered in full")
	}
	// Counted in runes: the truncation marker is multi-byte, and the budget is
	// about how many columns the cell occupies.
	if width := utf8.RuneCountInString(got); width > shortNodeIDLen {
		t.Errorf("fallback %q is %d runes, over the %d-column budget", got, width, shortNodeIDLen)
	}
	if !strings.HasPrefix(sampleNodeUUID, strings.TrimSuffix(got, "…")) {
		t.Errorf("fallback %q is not a prefix of the id", got)
	}
}

func TestNamerHandlesEmptyReference(t *testing.T) {
	n := newNodeNamer()
	if got := n.name(""); got != unknownNodeLabel {
		t.Errorf("empty reference = %q, want %q", got, unknownNodeLabel)
	}
}

// TestNamerRetainsNamesAfterNodeLeaves checks a completed job keeps a readable
// origin after the machine that ran it drops out of discovery.
func TestNamerRetainsNamesAfterNodeLeaves(t *testing.T) {
	n := newNodeNamer()
	n.learnDiscovered([]availableNode{{HostUUID: sampleNodeUUID, Name: "workstation-01"}})
	n.learnDiscovered(nil) // node gone from the snapshot

	if got := n.name(sampleNodeUUID); got != "workstation-01" {
		t.Errorf("name = %q; a finished job outlives its node's reachability", got)
	}
}

// TestNamerIgnoresBlankLearnings checks a payload missing either half does not
// poison the map with an empty name.
func TestNamerIgnoresBlankLearnings(t *testing.T) {
	n := newNodeNamer()
	n.learnDiscovered([]availableNode{
		{HostUUID: sampleNodeUUID, Name: ""},
		{HostUUID: "", Name: "nameless"},
	})
	if got := n.name(sampleNodeUUID); got == "" {
		t.Error("resolved to an empty name")
	}
}

func TestNamerSelf(t *testing.T) {
	n := newNodeNamer()
	n.setSelf(clusterIdentity{NodeUUID: sampleNodeUUID, Name: "this-host"})

	if got := n.name(sampleNodeUUID); got != "this-host" {
		t.Errorf("self name = %q", got)
	}
	// Falls back to nodeId when the manager reports no friendly name.
	n2 := newNodeNamer()
	n2.setSelf(clusterIdentity{NodeUUID: "u", NodeID: "host-b"})
	if got := n2.name("u"); got != "host-b" {
		t.Errorf("name = %q, want the nodeId fallback", got)
	}
}

// TestJobsRendersNodeNames is the end-to-end guard: a job's origin and target
// must reach the table as names, not as the UUIDs the backend stamps.
func TestJobsRendersNodeNames(t *testing.T) {
	v := newJobsView(nil)
	v.namer.learnDiscovered([]availableNode{
		{HostUUID: "origin-uuid", Name: "laptop"},
		{HostUUID: "target-uuid", Name: "gpu-box"},
	})
	v.upsert(workload{
		ID:             "w1",
		Model:          "llama3.2",
		Engine:         "ollama",
		State:          "running",
		OriginatedFrom: "origin-uuid",
		ScheduledOn:    "target-uuid",
	})

	rows := v.table.Rows()
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0][3] != "laptop" {
		t.Errorf("FROM = %q, want laptop", rows[0][3])
	}
	if rows[0][4] != "gpu-box" {
		t.Errorf("RAN ON = %q, want gpu-box", rows[0][4])
	}
}

// TestJobsUnplacedWorkShowsPending checks an active job with no target yet says
// so, instead of rendering a blank that reads as "ran nowhere".
func TestJobsUnplacedWorkShowsPending(t *testing.T) {
	v := newJobsView(nil)

	v.upsert(workload{ID: "w1", State: "queued", OriginatedFrom: "o"})
	if got := v.ranOn(v.byKey[workloadKey("o", "w1")]); got == unknownNodeLabel {
		t.Error("an active unplaced job should say a node is being chosen")
	}

	v.upsert(workload{ID: "w2", State: "completed", OriginatedFrom: "o"})
	if got := v.ranOn(v.byKey[workloadKey("o", "w2")]); got != unknownNodeLabel {
		t.Errorf("a finished job with no target = %q, want %q", got, unknownNodeLabel)
	}
}

// TestClusterLabelIsShown is the guard for a write-only setting: the cluster
// name must appear somewhere once set, or naming a cluster has no visible
// effect anywhere in the interface.
func TestClusterLabelIsShown(t *testing.T) {
	v := newNodesView(nil)
	v.identity = clusterIdentity{ClusterID: "abcdef0123456789", Name: "host-a"}

	// With no label, the id stands in — it is what anything operational uses.
	if got := v.clusterLine(); !contains(got, "abcdef") {
		t.Errorf("cluster line %q shows neither a label nor the id", got)
	}

	v.clusterName = "Lab 3 desks"
	got := v.clusterLine()
	if !contains(got, "Lab 3 desks") {
		t.Errorf("cluster line %q omits the label that was set", got)
	}
}

// TestClusterLabelFromIdentityPush checks the label follows the notification, so
// renaming on one machine is reflected without a restart.
func TestClusterLabelFromIdentityPush(t *testing.T) {
	v := newNodesView(nil)
	v.Update(NotificationMsg{Msg: identityChanged("cid-1", "Lab 3 desks")})

	if v.clusterName != "Lab 3 desks" {
		t.Errorf("clusterName = %q after the push", v.clusterName)
	}
	if v.identity.ClusterID != "cid-1" {
		t.Errorf("clusterId = %q after the push", v.identity.ClusterID)
	}
}

// TestServiceHidesUnusedSettings checks the two settings nothing acts on are not
// offered, so the list does not imply an effect they do not have.
func TestServiceHidesUnusedSettings(t *testing.T) {
	v := newServiceView(nil)
	for _, it := range v.items {
		switch it.suffix {
		case "force-ports", "cluster-auto-sync":
			t.Errorf("%q is shown but nothing acts on it", it.label)
		}
	}
	if len(v.items) == 0 {
		t.Fatal("no configuration rows at all")
	}
}
