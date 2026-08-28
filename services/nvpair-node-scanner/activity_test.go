// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"nvpair-shared/noderec"
)

// The probes this file is about are the ones that fail on a node BECAUSE it is
// working: a machine saturated by its own inference load has no CPU left to
// answer a node-info GET or accept a TCP connect within a second, so the
// eviction path reads it as gone and drops it mid-request. The proxies, meanwhile,
// are receiving that node's response bytes. These tests pin that evidence beating
// the probe.

// activityDaemon is the minimum daemon the activity path needs. It deliberately
// has no HTTP client and no registry: the point of every test here is what
// happens when nothing answers.
func activityDaemon() *daemon {
	return &daemon{lastActivityAt: make(map[string]time.Time)}
}

// unreachableNode advertises a node-info port nothing is listening on, so the
// TCP sweep in reachable is guaranteed to fail.
func unreachableNode(t *testing.T, hostUUID string) RawNode {
	t.Helper()
	port := closedPort(t)
	return RawNode{
		ID:        hostUUID,
		Addresses: []string{"127.0.0.1"},
		TXT:       []string{"v=1", "uuid=" + hostUUID, "ip=127.0.0.1", fmt.Sprintf("ni=%d", port)},
	}
}

// TestFreshActivityKeepsANodeNoProbeCanReach is the fix. Without it a node that
// is streaming a generation back to us is evicted for failing a probe it was too
// busy to answer.
func TestFreshActivityKeepsANodeNoProbeCanReach(t *testing.T) {
	d := activityDaemon()
	n := unreachableNode(t, "busy-node")

	if d.reachable(n) {
		t.Fatal("precondition: a node with no listener must fail the probe when nothing vouches for it")
	}

	d.noteActivity("busy-node", 0)
	if !d.reachable(n) {
		t.Fatal("a node that just returned inference bytes must be kept despite an unanswerable probe")
	}
}

// Evidence has to expire, or a node that has genuinely gone would be held by a
// report from an hour ago and never age out.
func TestStaleActivityDoesNotKeepANode(t *testing.T) {
	d := activityDaemon()
	n := unreachableNode(t, "departed-node")

	d.noteActivity("departed-node", (activityFreshness + time.Second).Milliseconds())
	if d.reachable(n) {
		t.Fatal("activity older than activityFreshness must not keep an unreachable node")
	}
}

// Activity is credited per node. One busy peer vouching for itself must not
// vouch for a different peer that really has left.
func TestActivityIsCreditedPerNode(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("busy-node", 0)

	if d.reachable(unreachableNode(t, "other-node")) {
		t.Fatal("one node's activity must not keep a different node alive")
	}
}

// The report carries an age rather than a timestamp so the two processes need no
// agreement about clocks, which only works if the age is actually applied.
func TestNoteActivityAppliesTheReportedAge(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("node", 5_000)

	since, ok := d.activitySince("node")
	if !ok {
		t.Fatal("activity was not recorded")
	}
	if since < 5*time.Second {
		t.Fatalf("reported age was ignored: recorded %s ago, want at least 5s", since)
	}
}

// Both proxies report independently and their notifications race through the
// broker, so an older report arriving second must not pull the node's evidence
// backwards and expose it to eviction.
func TestOutOfOrderReportsKeepTheNewest(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("node", 0)
	d.noteActivity("node", 30_000)

	since, ok := d.activitySince("node")
	if !ok {
		t.Fatal("activity was not recorded")
	}
	if since > time.Second {
		t.Fatalf("a later-arriving older report overwrote the newest: recorded %s ago", since)
	}
}

// A negative age would place the observation in the future and keep the node
// past activityFreshness. The wire value comes from another process, so it is
// clamped rather than trusted.
func TestNegativeReportedAgeIsClamped(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("node", -60_000)

	since, ok := d.activitySince("node")
	if !ok {
		t.Fatal("activity was not recorded")
	}
	if since < 0 {
		t.Fatalf("negative age was not clamped: recorded %s ago", since)
	}
}

func TestUnreportedNodeHasNoActivity(t *testing.T) {
	d := activityDaemon()
	if _, ok := d.activitySince("never-seen"); ok {
		t.Fatal("a node nothing has reported must have no activity evidence")
	}
}

// A proxy resolves targets by URL and port and cannot tell which uuid is this
// machine's, so it reports self too. Self is never evicted by a browse miss, so
// recording it would only grow the map.
func TestSelfActivityIsDropped(t *testing.T) {
	d := activityDaemon()
	d.reg = newRegistry("self-uuid", "", []string{"127.0.0.1"})

	d.noteActivity("self-uuid", 0)
	if _, ok := d.activitySince("self-uuid"); ok {
		t.Fatal("this node's own uuid must not be recorded as peer activity")
	}
}

// Eviction clears a node's caches; leaving activity behind would let a stale
// report vouch for the node the moment it was rediscovered.
func TestForgetClearsActivity(t *testing.T) {
	d := activityDaemon()
	d.lastInfo = make(map[string]NodeInfoResponse)
	d.lastInfoAt = make(map[string]time.Time)
	d.lastModels = make(map[string][]string)
	d.lastModelsByEngine = make(map[string]map[string][]string)
	d.lastLoadedByEngine = make(map[string]map[string][]string)
	d.nodeInfoDown = make(map[string]bool)

	d.noteActivity("node", 0)
	d.forget("node")
	if _, ok := d.activitySince("node"); ok {
		t.Fatal("forget must clear a node's activity evidence")
	}
}

// The relay arrives as a JSON-RPC frame from the broker, so the handler has to
// claim the method and decode it — a silently unhandled method would leave the
// whole signal dead with nothing failing.
func TestHandleNodeActivityRecordsTheReport(t *testing.T) {
	d := activityDaemon()

	params, err := json.Marshal(noderec.NodeActivityParams{HostUUID: "node", MSSince: 1_000})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !d.handle(&Message{Method: noderec.MethodNodeActivity, Params: params}) {
		t.Fatal("the scanner must claim discovery:node-activity")
	}

	since, ok := d.activitySince("node")
	if !ok {
		t.Fatal("the relayed report was not recorded")
	}
	if since < time.Second {
		t.Fatalf("relayed age was ignored: recorded %s ago, want at least 1s", since)
	}
}
