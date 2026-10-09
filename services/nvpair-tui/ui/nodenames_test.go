// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NotEqual(t, "workstation-01", n.name(sampleNodeUUID), "resolved before learning anything")

	n.learnDiscovered([]availableNode{{HostUUID: sampleNodeUUID, Name: "workstation-01"}})
	assert.Equal(t, "workstation-01", n.name(sampleNodeUUID), "name must come from discovery")
}

// TestNamerResolvesFromMembership covers a peer known from the cluster roster
// but absent from the current discovery snapshot.
func TestNamerResolvesFromMembership(t *testing.T) {
	n := newNodeNamer()
	n.learnMembers([]clusterNode{{NodeUUID: sampleNodeUUID, Name: "peer-a"}})
	assert.Equal(t, "peer-a", n.name(sampleNodeUUID), "name must come from membership")
}

// TestNamerFallsBackToShortID checks an unresolved id is shortened rather than
// printed in full, and is still distinguishable from a real name.
func TestNamerFallsBackToShortID(t *testing.T) {
	n := newNodeNamer()
	got := n.name(sampleNodeUUID)

	assert.NotEqual(t, sampleNodeUUID, got, "unresolved id must not render in full")
	// Counted in runes: the truncation marker is multi-byte, and the budget is
	// about how many columns the cell occupies.
	assert.LessOrEqual(t, utf8.RuneCountInString(got), shortNodeIDLen, "fallback must fit its column budget")
	assert.True(t, strings.HasPrefix(sampleNodeUUID, strings.TrimSuffix(got, "…")), "fallback must be a prefix of the id")
}

func TestNamerHandlesEmptyReference(t *testing.T) {
	n := newNodeNamer()
	assert.Equal(t, unknownNodeLabel, n.name(""))
}

// TestNamerRetainsNamesAfterNodeLeaves checks a completed job keeps a readable
// origin after the machine that ran it drops out of discovery.
func TestNamerRetainsNamesAfterNodeLeaves(t *testing.T) {
	n := newNodeNamer()
	n.learnDiscovered([]availableNode{{HostUUID: sampleNodeUUID, Name: "workstation-01"}})
	n.learnDiscovered(nil) // node gone from the snapshot

	assert.Equal(t, "workstation-01", n.name(sampleNodeUUID), "a finished job outlives its node's reachability")
}

// TestNamerIgnoresBlankLearnings checks a payload missing either half does not
// poison the map with an empty name.
func TestNamerIgnoresBlankLearnings(t *testing.T) {
	n := newNodeNamer()
	n.learnDiscovered([]availableNode{
		{HostUUID: sampleNodeUUID, Name: ""},
		{HostUUID: "", Name: "nameless"},
	})
	assert.NotEmpty(t, n.name(sampleNodeUUID), "resolved to an empty name")
}

func TestNamerSelf(t *testing.T) {
	n := newNodeNamer()
	n.setSelf(clusterIdentity{NodeUUID: sampleNodeUUID, Name: "this-host"})

	assert.Equal(t, "this-host", n.name(sampleNodeUUID))
	// Falls back to nodeId when the manager reports no friendly name.
	n2 := newNodeNamer()
	n2.setSelf(clusterIdentity{NodeUUID: "u", NodeID: "host-b"})
	assert.Equal(t, "host-b", n2.name("u"), "nodeId fallback")
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
	require.Len(t, rows, 1)
	assert.Equal(t, "laptop", rows[0][4])
	assert.Equal(t, "gpu-box", rows[0][5])
}

// TestJobsUnplacedWorkShowsPending checks an active job with no target yet says
// so, instead of rendering a blank that reads as "ran nowhere".
func TestJobsUnplacedWorkShowsPending(t *testing.T) {
	v := newJobsView(nil)

	queued := workload{ID: "w1", State: "queued", OriginatedFrom: "o"}
	v.upsert(queued)
	assert.NotEqual(t, unknownNodeLabel, v.ranOn(v.byKey[workloadKey(queued)]), "an active unplaced job should say a node is being chosen")

	done := workload{ID: "w2", State: "completed", OriginatedFrom: "o"}
	v.upsert(done)
	assert.Equal(t, unknownNodeLabel, v.ranOn(v.byKey[workloadKey(done)]), "a finished job has no target")
}

// TestClusterLabelIsShown is the guard for a write-only setting: the cluster
// name must appear somewhere once set, or naming a cluster has no visible
// effect anywhere in the interface.
func TestClusterLabelIsShown(t *testing.T) {
	v := newNodesView(nil)
	v.identity = clusterIdentity{ClusterID: "abcdef0123456789", Name: "host-a"}

	// With no label, the id stands in — it is what anything operational uses.
	assert.Contains(t, v.clusterLine(), "abcdef", "cluster line must show the id when no label is set")

	v.clusterName = "Lab 3 desks"
	assert.Contains(t, v.clusterLine(), "Lab 3 desks", "cluster line must show the label that was set")
}

// TestClusterLabelFromIdentityPush checks the label follows the notification, so
// renaming on one machine is reflected without a restart.
func TestClusterLabelFromIdentityPush(t *testing.T) {
	v := newNodesView(nil)
	v.Update(NotificationMsg{Msg: identityChanged("cid-1", "Lab 3 desks")})

	assert.Equal(t, "Lab 3 desks", v.clusterName)
	assert.Equal(t, "cid-1", v.identity.ClusterID)
}

// TestRenameOnServiceTabReachesNodesTab is the regression guard for a rename
// that looked unsaved. The settings worker sends no push when the name
// changes, and the Nodes tab read it once at startup, so flipping back after a
// rename showed the old name.
func TestRenameOnServiceTabReachesNodesTab(t *testing.T) {
	nodes := newNodesView(nil)
	nodes.Update(clusterNameMsg{name: "old name"})

	svc := newServiceView(nil)
	idx := -1
	for i, it := range svc.items {
		if it.setMethod == setClusterNameMethod {
			idx = i
		}
	}
	require.GreaterOrEqual(t, idx, 0, "the Service tab has no cluster-name row")
	saved := settingSavedMsg{idx: idx, method: setClusterNameMethod, value: "new name"}
	svc.Update(saved)
	nodes.Update(saved)

	assert.Equal(t, "new name", nodes.clusterName)

	// A failed save changes nothing.
	nodes.Update(settingSavedMsg{idx: idx, method: setClusterNameMethod, value: "bad", err: errFake{}})
	assert.Equal(t, "new name", nodes.clusterName, "a failed save must preserve the name")
}

// TestServiceHidesUnusedSettings checks the two settings nothing acts on are not
// offered, so the list does not imply an effect they do not have.
func TestServiceHidesUnusedSettings(t *testing.T) {
	v := newServiceView(nil)
	for _, it := range v.items {
		for _, method := range []string{it.getMethod, it.setMethod} {
			assert.NotContains(t, []string{"settings/get-force-ports", "settings/set-force-ports", "settings/get-cluster-auto-sync", "settings/set-cluster-auto-sync"}, method, "%q must not offer a setting nothing acts on", it.label)
		}
	}
	require.NotEmpty(t, v.items, "no configuration rows at all")
}
