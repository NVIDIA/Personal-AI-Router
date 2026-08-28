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
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	var output bytes.Buffer
	d := &daemon{codec: NewCodec(&output), http: server.Client()}
	if !d.refreshNodeTelemetry(context.Background(), "node-a", serverURL.Hostname(), port) {
		t.Fatal("valid node-info response did not emit telemetry")
	}

	var message Message
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &message); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if message.Method != noderec.NotifyNodeTelemetry {
		t.Fatalf("notification method = %q, want %q", message.Method, noderec.NotifyNodeTelemetry)
	}
	var got noderec.NodeTelemetry
	if err := json.Unmarshal(message.Params, &got); err != nil {
		t.Fatalf("decode telemetry: %v", err)
	}
	if got.HostUUID != "node-a" || !got.TelemetryValid {
		t.Fatalf("telemetry identity/validity = %+v", got)
	}
	if got.GPUUtilizationPct != 84 {
		t.Fatalf("GPU utilization = %d, want max 84", got.GPUUtilizationPct)
	}
	if got.MSSince < 137 || got.MSSince >= 3_500 {
		t.Fatalf("age = %d, want node age plus bounded fetch time", got.MSSince)
	}
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
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

	var output bytes.Buffer
	d := &daemon{codec: NewCodec(&output), http: server.Client()}
	if d.refreshNodeTelemetry(context.Background(), "node-a", serverURL.Hostname(), port) {
		t.Fatal("mismatched host emitted telemetry")
	}
	if output.Len() != 0 {
		t.Fatalf("mismatched host wrote notification: %s", output.String())
	}
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
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

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
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &message); err != nil {
		t.Fatalf("decode notification: %v (output %q)", err, output.String())
	}
	var got noderec.NodeTelemetry
	if err := json.Unmarshal(message.Params, &got); err != nil {
		t.Fatalf("decode telemetry: %v", err)
	}
	if got.HostUUID != "node-a" || got.GPUUtilizationPct != 61 || !got.TelemetryValid {
		t.Fatalf("telemetry = %+v, want node-a at 61%% from its second address", got)
	}

	// And the address that answered is remembered — in the memory the enrichment
	// sweeps share — so the sweeps behind this one ask it alone rather than paying
	// the unreachable address on every due telemetry attempt.
	key := hostKey{hostUUID: "node-a", service: noderec.ServiceNodeInfo}
	if remembered := d.enrichHosts.get(key); remembered != serverURL.Hostname() {
		t.Fatalf("remembered address = %q, want %q", remembered, serverURL.Hostname())
	}
}

func TestTelemetryIntervalForNodeIsStableAndBounded(t *testing.T) {
	if got := telemetryIntervalForNode(""); got != telemetryRefreshInterval {
		t.Fatalf("empty identity interval = %v, want %v", got, telemetryRefreshInterval)
	}
	first := telemetryIntervalForNode("node-a")
	if again := telemetryIntervalForNode("node-a"); again != first {
		t.Fatalf("node jitter changed from %v to %v", first, again)
	}
	minimum := telemetryRefreshInterval - telemetryRefreshJitter
	maximum := telemetryRefreshInterval + telemetryRefreshJitter
	if first < minimum || first > maximum {
		t.Fatalf("node-a interval = %v, want within [%v,%v]", first, minimum, maximum)
	}
	if second := telemetryIntervalForNode("node-b"); second == first {
		t.Fatalf("distinct identities received identical test intervals: %v", first)
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
		if got := telemetryRetryDelay(test.failures); got != test.want {
			t.Errorf("telemetryRetryDelay(%d) = %v, want %v", test.failures, got, test.want)
		}
	}
}

func TestTelemetryRetryGateBacksOffAndResets(t *testing.T) {
	var gate telemetryRetryGate
	startedAt := time.Unix(1_000, 0)
	targetKey := telemetryTargetKey([]string{"192.0.2.1"}, 14318)

	first, ok := gate.claim("node-a", targetKey, startedAt)
	if !ok || first == 0 {
		t.Fatal("first telemetry attempt was not due")
	}
	if _, ok := gate.claim("node-a", targetKey, startedAt); ok {
		t.Fatal("a second attempt started while the first was in flight")
	}
	gate.finish("node-a", first, false, startedAt)

	if _, ok := gate.claim("node-a", targetKey, startedAt.Add(4*time.Second-time.Nanosecond)); ok {
		t.Fatal("failed telemetry retried before its first backoff elapsed")
	}
	second, ok := gate.claim("node-a", targetKey, startedAt.Add(4*time.Second))
	if !ok || second == first {
		t.Fatal("failed telemetry was not due at its retry deadline")
	}
	gate.finish("node-a", second, true, startedAt.Add(4*time.Second))

	if _, ok := gate.claim("node-a", targetKey, startedAt.Add(4*time.Second)); !ok {
		t.Fatal("successful telemetry did not reset the retry gate")
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
			if !ok {
				t.Fatal("old endpoint telemetry attempt was not due")
			}
			if _, ok := gate.claim("node-a", changedTarget, now); ok {
				t.Fatal("changed endpoint started while the old attempt was in flight")
			}

			gate.finish("node-a", oldToken, false, now)
			if _, ok := gate.claim("node-a", changedTarget, now); !ok {
				t.Fatal("old endpoint failure backed off the changed endpoint")
			}
		})
	}
}

func TestTelemetryRetryGateRemoveRediscoverPreservesActiveClaim(t *testing.T) {
	var gate telemetryRetryGate
	now := time.Unix(2_000, 0)
	targetKey := telemetryTargetKey([]string{"192.0.2.1"}, 14318)
	token, ok := gate.claim("node-a", targetKey, now)
	if !ok {
		t.Fatal("first telemetry attempt was not due")
	}

	gate.forget("node-a")
	if _, ok := gate.claim("node-a", targetKey, now); ok {
		t.Fatal("re-discovered node started telemetry before its removed attempt finished")
	}
	gate.finish("node-a", token, false, now)
	if _, ok := gate.claim("node-a", targetKey, now); !ok {
		t.Fatal("removed attempt's late failure backed off the re-discovered node")
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
	if err != nil {
		t.Fatalf("parse backed-off server URL: %v", err)
	}
	backedOffPort, err := strconv.Atoi(backedOffURL.Port())
	if err != nil {
		t.Fatalf("parse backed-off server port: %v", err)
	}

	var healthyCalls atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyCalls.Add(1)
		_ = json.NewEncoder(w).Encode(NodeInfoResponse{HostUUID: "node-healthy"})
	}))
	defer healthy.Close()
	healthyURL, err := url.Parse(healthy.URL)
	if err != nil {
		t.Fatalf("parse healthy server URL: %v", err)
	}
	healthyPort, err := strconv.Atoi(healthyURL.Port())
	if err != nil {
		t.Fatalf("parse healthy server port: %v", err)
	}

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
	if !ok {
		t.Fatal("could not seed backed-off peer")
	}
	d.telemetryRetries.finish("node-backed-off", token, false, now)

	d.refreshTelemetryOnce(context.Background())

	if got := backedOffCalls.Load(); got != 0 {
		t.Fatalf("backed-off peer received %d requests, want 0", got)
	}
	if got := healthyCalls.Load(); got != 1 {
		t.Fatalf("healthy peer received %d requests, want 1", got)
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
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

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
	if !ok {
		t.Fatal("could not seed peer retry state")
	}
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

	if got := calls.Load(); got != 1 {
		t.Fatalf("browse enrichment made %d node-info requests, want 1", got)
	}
	if _, ok := d.telemetryRetries.claim("peer-uuid", targetKey, now); ok {
		t.Fatal("TXT-only update cleared telemetry backoff for an unchanged endpoint")
	}
}

func TestBrowseRemovalClearsTelemetryRetry(t *testing.T) {
	d := newSelfTestDaemon("self-uuid", "127.0.0.1")
	now := time.Now()
	host := "192.0.2.81"
	port := 14318
	targetKey := telemetryTargetKey([]string{host}, port)
	token, ok := d.telemetryRetries.claim("peer-uuid", targetKey, now)
	if !ok {
		t.Fatal("could not seed peer retry state")
	}
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

	if _, ok := d.telemetryRetries.claim("peer-uuid", targetKey, now); !ok {
		t.Fatal("removed peer endpoint remained backed off")
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
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	newURL, err := url.Parse(newServer.URL)
	if err != nil {
		t.Fatalf("parse new server URL: %v", err)
	}
	newPort, err := strconv.Atoi(newURL.Port())
	if err != nil {
		t.Fatalf("parse new server port: %v", err)
	}
	oldURL, err := url.Parse(oldServer.URL)
	if err != nil {
		t.Fatalf("parse old server URL: %v", err)
	}
	oldPort, err := strconv.Atoi(oldURL.Port())
	if err != nil {
		t.Fatalf("parse old server port: %v", err)
	}

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
	if _, ok := d.telemetryRetries.claim("peer-uuid", replacementTarget, time.Now()); ok {
		t.Fatal("replacement endpoint started while old endpoint telemetry was in flight")
	}
	release(releaseOld)
	waitFor(telemetryDone, "old endpoint result")

	if _, ok := d.telemetryRetries.claim("peer-uuid", replacementTarget, time.Now()); !ok {
		t.Fatal("old endpoint failure backed off the replacement endpoint")
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
	if err != nil {
		t.Fatalf("parse slow server URL: %v", err)
	}
	slowPort, err := strconv.Atoi(slowURL.Port())
	if err != nil {
		t.Fatalf("parse slow server port: %v", err)
	}

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
	if err != nil {
		t.Fatalf("parse healthy server URL: %v", err)
	}
	healthyPort, err := strconv.Atoi(healthyURL.Port())
	if err != nil {
		t.Fatalf("parse healthy server port: %v", err)
	}

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
		t.Fatal("blocked peer telemetry did not start")
	}
	select {
	case <-healthyThird:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("blocked peer delayed repeated healthy telemetry polls")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("telemetry loop did not drain blocked work after cancellation")
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
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}

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

	if calls.Load() < 2 {
		t.Fatalf("telemetry loop made %d calls, want at least two", calls.Load())
	}
	if maxActive.Load() != 1 {
		t.Fatalf("maximum concurrent requests for one node = %d, want 1", maxActive.Load())
	}
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
		if err != nil {
			t.Fatalf("parse server URL: %v", err)
		}
		port, err := strconv.Atoi(serverURL.Port())
		if err != nil {
			t.Fatalf("parse server port: %v", err)
		}
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
			t.Fatal("telemetry loop did not fill its concurrency allowance")
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
			t.Fatal("queued telemetry did not start when capacity became available")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("telemetry loop did not drain bounded workers after cancellation")
	}

	if overflowed {
		t.Fatalf("more than %d telemetry requests ran concurrently", telemetryRefreshConcurrency)
	}
	if got := maxActive.Load(); got != telemetryRefreshConcurrency {
		t.Fatalf("maximum concurrent telemetry work = %d, want %d", got, telemetryRefreshConcurrency)
	}
}
