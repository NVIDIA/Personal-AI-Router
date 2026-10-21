// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

// One authenticated read must update the peer's metadata and hardware and send
// that same sample to scheduling, rather than waiting for another inventory read.
func TestTelemetryDispatch_UpdatesInventoryAndSchedulingTogether(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "peer-uuid")
	f.discover(t)
	f.info.Peer.Name = "updated peer"
	f.info.Peer.Services[noderec.ServiceOllama] = 11435
	f.info.CPU = &CPUInfo{Name: "updated CPU", Cores: 16}
	f.info.GPUs[0].UtilizationPercent = 95
	var reads atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	releaseRead := sync.OnceFunc(func() { close(release) })
	defer releaseRead()
	f.respond = func(http.ResponseWriter, *http.Request) bool {
		reads.Add(1)
		close(started)
		<-release
		return false
	}
	var output bytes.Buffer
	f.daemon.codec = NewCodec(&output)
	finished := make(chan struct{})
	go func() {
		f.daemon.refreshTelemetryOnce(context.Background())
		close(finished)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("authenticated sample request did not start")
	}
	releaseRead()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("authenticated sample did not finish")
	}

	assert.Equal(t, int32(1), reads.Load())
	node, present := f.daemon.dir.get("peer-uuid")
	require.True(t, present)
	assert.Equal(t, "updated peer", node.Name)
	assert.Equal(t, 11435, node.Services[noderec.ServiceOllama].Port)
	assert.Equal(t, f.info.CPU, node.CPU)
	assert.Equal(t, f.info.GPUs, node.GPUs)
	var telemetry noderec.NodeTelemetry
	var snapshot noderec.NodeInfoEvent
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var message Message
		require.NoError(t, decoder.Decode(&message))
		switch message.Method {
		case noderec.NotifyNodeTelemetry:
			require.NoError(t, json.Unmarshal(message.Params, &telemetry))
		case noderec.NotifyNodeInfo:
			require.NoError(t, json.Unmarshal(message.Params, &snapshot))
		}
	}
	assert.Equal(t, "peer-uuid", telemetry.HostUUID)
	assert.True(t, telemetry.TelemetryValid)
	assert.Equal(t, uint32(95), telemetry.GPUUtilizationPct)
	assert.Equal(t, f.info.CPU, snapshot.Info.CPU)
	assert.Equal(t, f.info.GPUs, snapshot.Info.GPUs)
}

func TestTelemetryDispatch_BackoffDoesNotInterruptHealthyPeer(t *testing.T) {
	failing := newDiscoveryPeerFixture(t, "failing-peer")
	failing.discover(t)
	d := failing.daemon
	healthy := newDiscoveryPeerFixture(t, "healthy-peer")
	healthy.attach(t, d)
	var failedReads, healthyReads atomic.Int32
	failing.respond = func(w http.ResponseWriter, _ *http.Request) bool {
		failedReads.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	healthy.respond = func(http.ResponseWriter, *http.Request) bool {
		healthyReads.Add(1)
		return false
	}
	var output bytes.Buffer
	d.codec = NewCodec(&output)
	dispatch := func(at time.Time) {
		var workers sync.WaitGroup
		d.dispatchTelemetry(context.Background(), at, make(chan struct{}, telemetryRefreshConcurrency), &workers)
		workers.Wait()
	}

	startedAt := time.Now()
	dispatch(startedAt)
	require.Equal(t, int32(1), failedReads.Load())
	require.Equal(t, int32(1), healthyReads.Load())
	// The next normal dispatch precedes the failed peer's four-second retry.
	// Advance the dispatch clock rather than waiting for the backoff in real time.
	dispatch(startedAt.Add(2 * time.Second))

	assert.Equal(t, int32(1), failedReads.Load(), "failed peer must wait for its retry deadline")
	assert.Equal(t, int32(2), healthyReads.Load(), "healthy peer must keep its normal cadence")
	var sampledHosts []string
	decoder := json.NewDecoder(&output)
	for decoder.More() {
		var message Message
		require.NoError(t, decoder.Decode(&message))
		if message.Method != noderec.NotifyNodeTelemetry {
			continue
		}
		var sample noderec.NodeTelemetry
		require.NoError(t, json.Unmarshal(message.Params, &sample))
		sampledHosts = append(sampledHosts, sample.HostUUID)
		assert.True(t, sample.TelemetryValid)
		assert.Equal(t, uint32(fakePeerGPUUtilization), sample.GPUUtilizationPct)
	}
	assert.Equal(t, []string{"healthy-peer", "healthy-peer"}, sampledHosts)
}

func TestTelemetryDispatch_AnnouncementUpdatePreservesBackoff(t *testing.T) {
	test := func(name string, extraTXT []string) {
		t.Run(name, func(t *testing.T) {
			f := newDiscoveryPeerFixture(t, "failing-peer")
			f.discover(t)
			var reads atomic.Int32
			f.respond = func(w http.ResponseWriter, _ *http.Request) bool {
				reads.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			dispatch := func(at time.Time) {
				var workers sync.WaitGroup
				f.daemon.dispatchTelemetry(context.Background(), at, make(chan struct{}, telemetryRefreshConcurrency), &workers)
				workers.Wait()
			}
			startedAt := time.Now()
			dispatch(startedAt)
			require.Equal(t, int32(1), reads.Load())

			updated := f.raw
			updated.TXT = append(append([]string(nil), f.raw.TXT...), extraTXT...)
			f.daemon.onBrowse(DiscoveryEvent{Type: "updated", Node: updated})
			dispatch(startedAt.Add(2 * time.Second))

			assert.Equal(t, int32(1), reads.Load(), "an unchanged node-info target must retain its retry deadline")
		})
	}
	test("same announcement", nil)
	test("another service port changes", []string{"er=14319"})
}

func TestTelemetryIntervalForNode_IsStableAndBounded(t *testing.T) {
	assert.Equal(t, telemetryRefreshInterval, telemetryIntervalForNode(""))
	first := telemetryIntervalForNode("node-a")
	require.Equal(t, first, telemetryIntervalForNode("node-a"), "node jitter must be stable")
	minimum := telemetryRefreshInterval - telemetryRefreshJitter
	maximum := telemetryRefreshInterval + telemetryRefreshJitter
	require.GreaterOrEqual(t, first, minimum, "node-a interval (%v, %v, %v)", first, minimum, maximum)
	require.LessOrEqual(t, first, maximum, "node-a interval (%v, %v, %v)", first, minimum, maximum)
	require.NotEqual(t, first, telemetryIntervalForNode("node-b"), "distinct identities received identical test intervals (%v)", first)
}

func TestTelemetryRetryDelay_BoundsBackoff(t *testing.T) {
	test := func(name string, failures int, expected time.Duration) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, expected, telemetryRetryDelay(failures))
		})
	}
	test("no failures", 0, 2*time.Second)
	test("first failure", 1, 4*time.Second)
	test("second failure", 2, 8*time.Second)
	test("third failure", 3, 16*time.Second)
	test("fourth failure reaches maximum", 4, 30*time.Second)
	test("later failures retain maximum", 100, 30*time.Second)
}

func TestTelemetryRetryGate_BacksOffAndResets(t *testing.T) {
	var gate telemetryRetryGate
	startedAt := time.Unix(1_000, 0)
	targetKey := telemetryTargetKey([]string{"192.0.2.1"}, 14318)

	first, ok := gate.claim("node-a", targetKey, startedAt)
	require.True(t, ok, "first telemetry attempt was not due")
	require.NotEqual(t, uint64(0), first, "first telemetry attempt was not due")
	_, ok = gate.claim("node-a", targetKey, startedAt)
	require.False(t, ok, "a second attempt started while the first was in flight")
	gate.finish("node-a", first, false, startedAt)

	_, ok = gate.claim("node-a", targetKey, startedAt.Add(4*time.Second-time.Nanosecond))
	require.False(t, ok, "failed telemetry retried before its first backoff elapsed")
	second, ok := gate.claim("node-a", targetKey, startedAt.Add(4*time.Second))
	require.True(t, ok, "failed telemetry was not due at its retry deadline")
	require.NotEqual(t, first, second, "failed telemetry was not due at its retry deadline")
	gate.finish("node-a", second, true, startedAt.Add(4*time.Second))

	_, ok = gate.claim("node-a", targetKey, startedAt.Add(4*time.Second))
	require.True(t, ok, "successful telemetry did not reset the retry gate")
}

func TestTelemetryRetryGateChangedTarget_WaitsForActiveAttempt(t *testing.T) {
	oldTarget := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
	test := func(name, changedTarget string) {
		t.Run(name, func(t *testing.T) {
			var gate telemetryRetryGate
			now := time.Unix(1_500, 0)
			oldToken, ok := gate.claim("node-a", oldTarget, now)
			require.True(t, ok, "old endpoint telemetry attempt was not due")
			_, ok = gate.claim("node-a", changedTarget, now)
			require.False(t, ok, "changed endpoint started while the old attempt was in flight")

			gate.finish("node-a", oldToken, false, now)
			_, ok = gate.claim("node-a", changedTarget, now)
			require.True(t, ok, "old endpoint failure backed off the changed endpoint")
		})
	}
	test("address changes", telemetryTargetKey([]string{"10.0.0.1"}, 14318))
	test("port changes", telemetryTargetKey([]string{"192.0.2.1"}, 14319))
}

func TestTelemetryRetryGateRemoveRediscover_PreservesActiveClaim(t *testing.T) {
	var gate telemetryRetryGate
	now := time.Unix(2_000, 0)
	targetKey := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
	token, ok := gate.claim("node-a", targetKey, now)
	require.True(t, ok, "first telemetry attempt was not due")

	gate.forget("node-a")
	_, ok = gate.claim("node-a", targetKey, now)
	require.False(t, ok, "re-discovered node started telemetry before its removed attempt finished")
	gate.finish("node-a", token, false, now)
	_, ok = gate.claim("node-a", targetKey, now)
	require.True(t, ok, "removed attempt's late failure backed off the re-discovered node")
}
