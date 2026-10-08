// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func TestRefreshNodeTelemetryEmitsFreshnessAndUtilization(t *testing.T) {
	response := NodeInfoResponse{
		GPUs: []GPUInfo{
			{Name: "GPU 0", UtilizationPercent: 0},
			{Name: "GPU 1", UtilizationPercent: 84},
		},
		TelemetryValid: true,
		MSSince:        137,
		HostUUID:       "node-a",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err, "parse server port")

	var output bytes.Buffer
	d := &daemon{codec: NewCodec(&output), http: server.Client()}
	require.True(t, d.refreshNodeTelemetry(context.Background(), "node-a", serverURL.Hostname(), port), "valid node-info response did not emit telemetry")

	var message Message
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &message), "decode notification")
	require.Equal(t, noderec.NotifyNodeTelemetry, message.Method, "notification method")
	var got noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &got), "decode telemetry")
	require.Equal(t, "node-a", got.HostUUID, "telemetry identity/validity (%v)", got)
	require.True(t, got.TelemetryValid, "telemetry identity/validity (%v)", got)
	require.Equal(t, uint32(84), got.GPUUtilizationPct, "GPU utilization")
	require.GreaterOrEqual(t, got.MSSince, int64(137), "age")
	require.Less(t, got.MSSince, int64(3_500), "age")
}

func TestRefreshNodeTelemetryRejectsMismatchedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{
			HostUUID:       "node-b",
			TelemetryValid: true,
		})
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err, "parse server port")

	var output bytes.Buffer
	d := &daemon{codec: NewCodec(&output), http: server.Client()}
	require.False(t, d.refreshNodeTelemetry(context.Background(), "node-a", serverURL.Hostname(), port), "mismatched host emitted telemetry")
	require.Empty(t, output.Bytes(), "mismatched host wrote notification")
}

// TestRefreshTelemetryFailsOverToAnAnsweringAddress: this sweep is the only source
// of GPU pressure the scheduler gets, and it dialed the canonical address alone.
// A node whose canonical address is a link this host cannot reach therefore
// reported nothing at all, and the scheduler assigned it neutral pressure and
// scheduled it blind — while an address that answers sat unused in the same
// published list.
func TestRefreshTelemetryFailsOverToAnAnsweringAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{
			HostUUID:       "node-a",
			TelemetryValid: true,
			GPUs:           []GPUInfo{{Name: "GPU 0", UtilizationPercent: 61}},
		})
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err, "parse server port")

	var output bytes.Buffer
	d := &daemon{
		codec: NewCodec(&output),
		dir:   newDirectory(),
		// A short client timeout stands in for the daemon's own dial budget, so
		// the address that never answers is written off promptly.
		http: &http.Client{Timeout: 500 * time.Millisecond},
	}
	// TEST-NET-1 (RFC 5737) is routed nowhere, so only the second published
	// address can answer.
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "node-a",
		IP:       "192.0.2.1",
		IPs:      []string{"192.0.2.1", serverURL.Hostname()},
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: port},
		},
	})

	d.refreshTelemetryOnce(context.Background())

	var message Message
	{
		err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &message)
		require.NoError(t, err, "decode notification")
	}
	var got noderec.NodeTelemetry
	require.NoError(t, json.Unmarshal(message.Params, &got), "decode telemetry")
	require.Equal(t, "node-a", got.HostUUID, "telemetry (%v)", got)
	require.Equal(t, uint32(61), got.GPUUtilizationPct, "telemetry (%v)", got)
	require.True(t, got.TelemetryValid, "telemetry (%v)", got)

	// And the address that answered is remembered — in the memory the enrichment
	// sweeps share — so the sweeps behind this one ask it alone rather than paying
	// the unreachable address on every due telemetry attempt.
	key := hostKey{hostUUID: "node-a", service: noderec.ServiceNodeInfo}
	{
		remembered := d.enrichHosts.get(key)
		require.Equal(t, serverURL.Hostname(), remembered, "remembered address (%v)", remembered)
	}
}

func TestTelemetryIntervalForNodeIsStableAndBounded(t *testing.T) {
	{
		got := telemetryIntervalForNode("")
		require.Equal(t, telemetryRefreshInterval, got, "empty identity interval (%v, %v)", got, telemetryRefreshInterval)
	}
	first := telemetryIntervalForNode("node-a")
	{
		again := telemetryIntervalForNode("node-a")
		require.Equal(t, first, again, "node jitter changed from (%v, %v)", first, again)
	}
	minimum := telemetryRefreshInterval - telemetryRefreshJitter
	maximum := telemetryRefreshInterval + telemetryRefreshJitter
	require.GreaterOrEqual(t, first, minimum, "node-a interval (%v, %v, %v)", first, minimum, maximum)
	require.LessOrEqual(t, first, maximum, "node-a interval (%v, %v, %v)", first, minimum, maximum)
	{
		second := telemetryIntervalForNode("node-b")
		require.NotEqual(t, first, second, "distinct identities received identical test intervals (%v)", first)
	}
}

func TestTelemetryRetryDelay(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 30 * time.Second},
		{100, 30 * time.Second},
	}
	for _, test := range cases {
		{
			got := telemetryRetryDelay(test.failures)
			assert.Equal(t, test.want, got, "telemetryRetryDelay (%v)", got)
		}
	}
}

func TestTelemetryRetryGateBacksOffAndResets(t *testing.T) {
	var gate telemetryRetryGate
	startedAt := time.Unix(1_000, 0)
	targetKey := telemetryTargetKey([]string{"192.0.2.1"}, 14318)

	first, ok := gate.claim("node-a", targetKey, startedAt)
	require.True(t, ok, "first telemetry attempt was not due")
	require.NotEqual(t, uint64(0), first, "first telemetry attempt was not due")
	{
		_, ok := gate.claim("node-a", targetKey, startedAt)
		require.False(t, ok, "a second attempt started while the first was in flight")
	}
	gate.finish("node-a", first, false, startedAt)

	{
		_, ok := gate.claim("node-a", targetKey, startedAt.Add(4*time.Second-time.Nanosecond))
		require.False(t, ok, "failed telemetry retried before its first backoff elapsed")
	}
	second, ok := gate.claim("node-a", targetKey, startedAt.Add(4*time.Second))
	require.True(t, ok, "failed telemetry was not due at its retry deadline")
	require.NotEqual(t, first, second, "failed telemetry was not due at its retry deadline")
	gate.finish("node-a", second, true, startedAt.Add(4*time.Second))

	{
		_, ok := gate.claim("node-a", targetKey, startedAt.Add(4*time.Second))
		require.True(t, ok, "successful telemetry did not reset the retry gate")
	}
}

func TestTelemetryRetryGateChangedTargetWaitsForActiveAttempt(t *testing.T) {
	oldTarget := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
	changedTargets := map[string]string{
		"hosts": telemetryTargetKey([]string{"10.0.0.1"}, 14318),
		"port":  telemetryTargetKey([]string{"192.0.2.1"}, 14319),
	}
	for name, changedTarget := range changedTargets {
		t.Run(name, func(t *testing.T) {
			var gate telemetryRetryGate
			now := time.Unix(1_500, 0)
			oldToken, ok := gate.claim("node-a", oldTarget, now)
			require.True(t, ok, "old endpoint telemetry attempt was not due")
			{
				_, ok := gate.claim("node-a", changedTarget, now)
				require.False(t, ok, "changed endpoint started while the old attempt was in flight")
			}

			gate.finish("node-a", oldToken, false, now)
			{
				_, ok := gate.claim("node-a", changedTarget, now)
				require.True(t, ok, "old endpoint failure backed off the changed endpoint")
			}
		})
	}
}

func TestTelemetryRetryGateRemoveRediscoverPreservesActiveClaim(t *testing.T) {
	var gate telemetryRetryGate
	now := time.Unix(2_000, 0)
	targetKey := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
	token, ok := gate.claim("node-a", targetKey, now)
	require.True(t, ok, "first telemetry attempt was not due")

	gate.forget("node-a")
	{
		_, ok := gate.claim("node-a", targetKey, now)
		require.False(t, ok, "re-discovered node started telemetry before its removed attempt finished")
	}
	gate.finish("node-a", token, false, now)
	{
		_, ok := gate.claim("node-a", targetKey, now)
		require.True(t, ok, "removed attempt's late failure backed off the re-discovered node")
	}
}

func TestRefreshTelemetrySkipsBackedOffPeerWithoutDelayingHealthyPeer(t *testing.T) {
	var backedOffCalls atomic.Int32
	backedOff := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backedOffCalls.Add(1)
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "node-backed-off"})
	}))
	defer backedOff.Close()
	backedOffURL, err := url.Parse(backedOff.URL)
	require.NoError(t, err, "parse backed-off server URL")
	backedOffPort, err := strconv.Atoi(backedOffURL.Port())
	require.NoError(t, err, "parse backed-off server port")

	var healthyCalls atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyCalls.Add(1)
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "node-healthy"})
	}))
	defer healthy.Close()
	healthyURL, err := url.Parse(healthy.URL)
	require.NoError(t, err, "parse healthy server URL")
	healthyPort, err := strconv.Atoi(healthyURL.Port())
	require.NoError(t, err, "parse healthy server port")

	var output bytes.Buffer
	d := &daemon{
		codec: NewCodec(&output),
		dir:   newDirectory(),
		http:  &http.Client{Timeout: time.Second},
	}
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "node-backed-off",
		IP:       backedOffURL.Hostname(),
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: backedOffPort},
		},
	})
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "node-healthy",
		IP:       healthyURL.Hostname(),
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: healthyPort},
		},
	})
	now := time.Now()
	targetKey := telemetryTargetKey([]string{backedOffURL.Hostname()}, backedOffPort)
	token, ok := d.telemetryRetries.claim("node-backed-off", targetKey, now)
	require.True(t, ok, "could not seed backed-off peer")
	d.telemetryRetries.finish("node-backed-off", token, false, now)

	d.refreshTelemetryOnce(context.Background())

	{
		got := backedOffCalls.Load()
		require.Equal(t, int32(0), got, "backed-off peer received")
	}
	{
		got := healthyCalls.Load()
		require.Equal(t, int32(1), got, "healthy peer received")
	}
}

func TestBrowseTXTUpdatePreservesTelemetryRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "peer-uuid"})
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err, "parse server port")

	d := newSelfTestDaemon("self-uuid", "127.0.0.1")
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "peer-uuid",
		IP:       serverURL.Hostname(),
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: port},
		},
	})
	now := time.Now()
	targetKey := telemetryTargetKey([]string{serverURL.Hostname()}, port)
	token, ok := d.telemetryRetries.claim("peer-uuid", targetKey, now)
	require.True(t, ok, "could not seed peer retry state")
	d.telemetryRetries.finish("peer-uuid", token, false, now)

	d.onBrowse(DiscoveryEvent{
		Type: "updated",
		Node: RawNode{
			ID:        "peer",
			Addresses: []string{serverURL.Hostname()},
			TXT: []string{
				"v=1",
				"uuid=peer-uuid",
				"ip=" + serverURL.Hostname(),
				"ni=" + strconv.Itoa(port),
				"peer-controlled-field=changed",
			},
		},
	})

	{
		got := calls.Load()
		require.Equal(t, int32(1), got, "browse enrichment made")
	}
	{
		_, ok := d.telemetryRetries.claim("peer-uuid", targetKey, now)
		require.False(t, ok, "TXT-only update cleared telemetry backoff for an unchanged endpoint")
	}
}

func TestBrowseRemovalClearsTelemetryRetry(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "127.0.0.1")
	now := time.Now()
	host := "192.0.2.81"
	port := 14318
	targetKey := telemetryTargetKey([]string{host}, port)
	token, ok := d.telemetryRetries.claim("peer-uuid", targetKey, now)
	require.True(t, ok, "could not seed peer retry state")
	d.telemetryRetries.finish("peer-uuid", token, false, now)

	d.onBrowse(DiscoveryEvent{
		Type: "removed",
		Node: RawNode{
			ID:        "peer",
			Addresses: []string{host},
			TXT: []string{
				"v=1",
				"uuid=peer-uuid",
				"ip=" + host,
				"ni=" + strconv.Itoa(port),
			},
		},
	})

	{
		_, ok := d.telemetryRetries.claim("peer-uuid", targetKey, now)
		require.True(t, ok, "removed peer endpoint remained backed off")
	}
}

func TestBrowseEndpointUpdateWaitsForOldTelemetryBeforeClaimingReplacement(t *testing.T) {
	newEntered := make(chan struct{}, 1)
	releaseNew := make(chan struct{}, 1)
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		newEntered <- struct{}{}
		<-releaseNew
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "peer-uuid"})
	}))
	oldEntered := make(chan struct{}, 1)
	releaseOld := make(chan struct{}, 1)
	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		oldEntered <- struct{}{}
		<-releaseOld
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	release := func(ch chan<- struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	defer func() {
		release(releaseNew)
		release(releaseOld)
		newServer.Close()
		oldServer.Close()
	}()
	waitFor := func(ch <-chan struct{}, what string) {
		select {
		case <-ch:
		case <-time.After(time.Second):
			require.FailNow(t, "test expectation failed", "timed out waiting for (%v)", what)
		}
	}

	newURL, err := url.Parse(newServer.URL)
	require.NoError(t, err, "parse new server URL")
	newPort, err := strconv.Atoi(newURL.Port())
	require.NoError(t, err, "parse new server port")
	oldURL, err := url.Parse(oldServer.URL)
	require.NoError(t, err, "parse old server URL")
	oldPort, err := strconv.Atoi(oldURL.Port())
	require.NoError(t, err, "parse old server port")

	d := newSelfTestDaemon("self-uuid", "127.0.0.1")
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "peer-uuid",
		IP:       oldURL.Hostname(),
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: oldPort},
		},
	})

	browseDone := make(chan struct{})
	go func() {
		d.onBrowse(DiscoveryEvent{
			Type: "updated",
			Node: RawNode{
				ID:        "peer",
				Addresses: []string{newURL.Hostname()},
				TXT: []string{
					"v=1",
					"uuid=peer-uuid",
					"ip=" + newURL.Hostname(),
					"ni=" + strconv.Itoa(newPort),
				},
			},
		})
		close(browseDone)
	}()
	waitFor(newEntered, "replacement endpoint enrichment")

	telemetryDone := make(chan struct{})
	go func() {
		d.refreshTelemetryOnce(context.Background())
		close(telemetryDone)
	}()
	waitFor(oldEntered, "old endpoint telemetry")

	release(releaseNew)
	waitFor(browseDone, "directory update")
	replacementTarget := telemetryTargetKey([]string{newURL.Hostname()}, newPort)
	{
		_, ok := d.telemetryRetries.claim("peer-uuid", replacementTarget, time.Now())
		require.False(t, ok, "replacement endpoint started while old endpoint telemetry was in flight")
	}
	release(releaseOld)
	waitFor(telemetryDone, "old endpoint result")

	{
		_, ok := d.telemetryRetries.claim("peer-uuid", replacementTarget, time.Now())
		require.True(t, ok, "old endpoint failure backed off the replacement endpoint")
	}
}

func TestTelemetryLoopKeepsHealthyNodeOnCadenceWhilePeerIsBlocked(t *testing.T) {
	slowStarted := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case slowStarted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer slow.Close()
	slowURL, err := url.Parse(slow.URL)
	require.NoError(t, err, "parse slow server URL")
	slowPort, err := strconv.Atoi(slowURL.Port())
	require.NoError(t, err, "parse slow server port")

	healthyThird := make(chan struct{}, 1)
	var healthyCalls atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthyCalls.Add(1) >= 3 {
			select {
			case healthyThird <- struct{}{}:
			default:
			}
		}
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "node-healthy"})
	}))
	defer healthy.Close()
	healthyURL, err := url.Parse(healthy.URL)
	require.NoError(t, err, "parse healthy server URL")
	healthyPort, err := strconv.Atoi(healthyURL.Port())
	require.NoError(t, err, "parse healthy server port")

	d := &daemon{
		dir:  newDirectory(),
		http: &http.Client{Timeout: time.Second},
	}
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "node-slow",
		IP:       slowURL.Hostname(),
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: slowPort},
		},
	})
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "node-healthy",
		IP:       healthyURL.Hostname(),
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: healthyPort},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		d.runTelemetryLoop(ctx, 5*time.Millisecond)
		close(done)
	}()

	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "test expectation failed", "blocked peer telemetry did not start")
	}
	select {
	case <-healthyThird:
	case <-time.After(500 * time.Millisecond):
		require.FailNow(t, "test expectation failed", "blocked peer delayed repeated healthy telemetry polls")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "test expectation failed", "telemetry loop did not drain blocked work after cancellation")
	}
}

func TestTelemetryLoopDoesNotOverlapNodePolls(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		calls.Add(1)
		for {
			seen := maxActive.Load()
			if current <= seen || maxActive.CompareAndSwap(seen, current) {
				break
			}
		}
		select {
		case <-time.After(20 * time.Millisecond):
			_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "node-a"})
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(serverURL.Port())
	require.NoError(t, err, "parse server port")

	d := &daemon{
		dir:  newDirectory(),
		http: server.Client(),
	}
	d.dir.upsert(noderec.DirectoryNode{
		HostUUID: "node-a",
		IP:       serverURL.Hostname(),
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceNodeInfo: {Port: port},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	d.runTelemetryLoop(ctx, 5*time.Millisecond)

	require.GreaterOrEqual(t, calls.Load(), int32(2), "telemetry loop request count")
	require.Equal(t, int32(1), maxActive.Load(), "maximum concurrent requests for one node")
}

func TestTelemetryLoopBoundsConcurrentWorkAcrossTicks(t *testing.T) {
	const nodeCount = telemetryRefreshConcurrency + 1
	entered := make(chan struct{}, nodeCount)
	releaseWork := make(chan struct{})
	var releaseOnce sync.Once
	var active atomic.Int32
	var maxActive atomic.Int32
	d := &daemon{
		dir:  newDirectory(),
		http: &http.Client{Timeout: time.Second},
	}

	for i := range nodeCount {
		nodeID := "node-" + strconv.Itoa(i)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				seen := maxActive.Load()
				if current <= seen || maxActive.CompareAndSwap(seen, current) {
					break
				}
			}
			entered <- struct{}{}
			<-releaseWork
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(server.Close)
		serverURL, err := url.Parse(server.URL)
		require.NoError(t, err, "parse server URL")
		port, err := strconv.Atoi(serverURL.Port())
		require.NoError(t, err, "parse server port")
		d.dir.upsert(noderec.DirectoryNode{
			HostUUID: nodeID,
			IP:       serverURL.Hostname(),
			Services: map[noderec.ServiceKey]noderec.ServiceStatus{
				noderec.ServiceNodeInfo: {Port: port},
			},
		})
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseWork) })
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.runTelemetryLoop(ctx, 5*time.Millisecond)
		close(done)
	}()

	for range telemetryRefreshConcurrency {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			releaseOnce.Do(func() { close(releaseWork) })
			cancel()
			<-done
			require.FailNow(t, "test expectation failed", "telemetry loop did not fill its concurrency allowance")
		}
	}
	overflowed := false
	select {
	case <-entered:
		overflowed = true
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(releaseWork) })
	if !overflowed {
		select {
		case <-entered:
		case <-time.After(time.Second):
			cancel()
			<-done
			require.FailNow(t, "test expectation failed", "queued telemetry did not start when capacity became available")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "test expectation failed", "telemetry loop did not drain bounded workers after cancellation")
	}

	require.False(t, overflowed, "more than (%v)", telemetryRefreshConcurrency)
	{
		got := maxActive.Load()
		require.Equal(t, int32(telemetryRefreshConcurrency), got, "maximum concurrent telemetry work (%v, %v)", got, telemetryRefreshConcurrency)
	}
}
