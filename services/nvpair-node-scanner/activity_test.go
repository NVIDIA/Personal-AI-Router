// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// activityDaemon initializes only the state used to record inference activity.
func activityDaemon() *daemon {
	return &daemon{lastActivityAt: make(map[string]time.Time)}
}

// An authenticated peer may be too busy serving inference to answer node-info.
func TestFreshActivity_KeepsANodeNoProbeCanReach(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "busy-node")
	f.discover(t)
	d, n := f.daemon, f.raw
	f.server.Close()
	delete(d.lastInfoAt, "busy-node")

	require.False(t, d.reachable(n), "precondition: a node with no listener must fail the probe when nothing vouches for it")

	d.noteActivity("busy-node", 0)
	require.True(t, d.reachable(n), "a node that just returned inference bytes must be kept despite an unanswerable probe")
}

// Activity is credited per node. One busy peer vouching for itself must not
// vouch for a different peer that really has left.
func TestActivity_IsCreditedPerNode(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("busy-node", 0)

	_, ok := d.activitySince("other-node")
	assert.False(t, ok, "one node's report must not record activity for another")
}

// The report carries an age rather than a timestamp so the two processes need no
// agreement about clocks, which only works if the age is actually applied.
func TestNoteActivity_AppliesTheReportedAge(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("node", 5_000)

	since, ok := d.activitySince("node")
	require.True(t, ok, "activity was not recorded")
	require.GreaterOrEqual(t, since, 5*time.Second, "reported age was ignored: recorded")
}

// Both proxies report independently and their notifications race through the
// broker, so an older report arriving second must not pull the node's evidence
// backwards and expose it to eviction.
func TestActivityReports_KeepNewestTimestampWhenReportsArriveOutOfOrder(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("node", 0)
	d.noteActivity("node", 30_000)

	since, ok := d.activitySince("node")
	require.True(t, ok, "activity was not recorded")
	require.LessOrEqual(t, since, time.Second, "a later-arriving older report overwrote the newest: recorded")
}

// A negative age would place the observation in the future and keep the node
// past activityFreshness. The wire value comes from another process, so it is
// clamped rather than trusted.
func TestNegativeReportedAge_IsClamped(t *testing.T) {
	d := activityDaemon()
	d.noteActivity("node", -60_000)

	since, ok := d.activitySince("node")
	require.True(t, ok, "activity was not recorded")
	require.GreaterOrEqual(t, since, time.Duration(0), "negative age was not clamped")
}

func TestUnreportedNode_HasNoActivity(t *testing.T) {
	d := activityDaemon()
	_, ok := d.activitySince("never-seen")
	require.False(t, ok, "a node nothing has reported must have no activity evidence")
}

// A proxy resolves targets by URL and port and cannot tell which uuid is this
// machine's, so it reports self too. Self is never evicted by a browse miss, so
// recording it would only grow the map.
func TestSelfActivity_IsDropped(t *testing.T) {
	d := activityDaemon()
	d.reg = newRegistry("self-uuid", "", []string{"127.0.0.1"})

	d.noteActivity("self-uuid", 0)
	_, ok := d.activitySince("self-uuid")
	require.False(t, ok, "this node's own uuid must not be recorded as peer activity")
}

// Eviction clears a node's caches; leaving activity behind would let a stale
// report vouch for the node the moment it was rediscovered.
func TestForget_ClearsActivity(t *testing.T) {
	d := activityDaemon()
	d.lastInfoAt = make(map[string]time.Time)
	d.nodeInfoDown = make(map[string]bool)

	d.noteActivity("node", 0)
	d.forget("node")
	_, ok := d.activitySince("node")
	require.False(t, ok, "forget must clear a node's activity evidence")
}

// The relay arrives as a JSON-RPC frame from the broker, so the handler has to
// claim the method and decode it — a silently unhandled method would leave the
// whole signal dead with nothing failing.
func TestHandleNodeActivity_RecordsTheReport(t *testing.T) {
	d := activityDaemon()

	params, err := json.Marshal(noderec.NodeActivityParams{HostUUID: "node", MSSince: 1_000})
	require.NoError(t, err, "marshal")
	require.True(t, d.handle(&Message{Method: noderec.MethodNodeActivity, Params: params}), "the scanner must claim discovery:node-activity")

	since, ok := d.activitySince("node")
	require.True(t, ok, "the relayed report was not recorded")
	require.GreaterOrEqual(t, since, time.Second, "relayed age was ignored: recorded")
}
