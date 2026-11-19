// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
)

func newHardwarePreviewDiscovery(t *testing.T, handler http.HandlerFunc) (*daemon, RawNode) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return hardwarePreviewDiscoveryAt(t, server.URL)
}

func hardwarePreviewDiscoveryAt(t *testing.T, serverURL string) (*daemon, RawNode) {
	t.Helper()
	raw := RawNode{
		ID: "unpaired host", Host: "unpaired.local.", Addresses: []string{"127.0.0.1"},
		TXT: []string{"uuid=advertised-host", "cluster-uuid=advertised-principal", fmt.Sprintf("ni=%d", portFromURL(t, serverURL))},
	}
	d := newSelfTestDaemon("self-uuid", "192.0.2.17")
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	return d, raw
}

// newBlockedPreviewServer pauses responses until release. Tests can wait until
// the expected requests arrive before changing discovery state or sampling peers.
func newBlockedPreviewServer(t *testing.T, info noderec.NodeInfo, expectedRequests int32) (server *httptest.Server, waitForRequests, release func()) {
	t.Helper()
	started, released := make(chan struct{}), make(chan struct{})
	release = sync.OnceFunc(func() { close(released) })
	var requests atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == expectedRequests {
			close(started)
		}
		select {
		case <-released:
		case <-r.Context().Done():
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(info))
	}))
	t.Cleanup(func() {
		release()
		server.Close()
	})
	waitForRequests = func() {
		t.Helper()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("public hardware requests did not reach the server")
		}
	}
	return server, waitForRequests, release
}

func dispatchHardwarePreviewsForTest(d *daemon, now time.Time) {
	var workers sync.WaitGroup
	d.dispatchHardwarePreviews(context.Background(), now, make(chan struct{}, noderec.HardwarePreviewConcurrency), &workers)
	workers.Wait()
}

// Cancel an attempt waiting for a reader. Its claim must be released without
// failure backoff, so the same discovery can be read immediately afterward.
func TestDispatchHardwarePreviews_CanceledAttemptCanRetryImmediately(t *testing.T) {
	var reads atomic.Int32
	d, _ := newHardwarePreviewDiscovery(t, func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		assert.NoError(t, json.NewEncoder(w).Encode(noderec.NodeInfo{}))
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reader := make(chan struct{}, 1)
	reader <- struct{}{}
	var workers sync.WaitGroup
	now := time.Now()

	d.dispatchHardwarePreviews(ctx, now, reader, &workers)
	cancel()
	workers.Wait()
	require.Zero(t, reads.Load())

	dispatchHardwarePreviewsForTest(d, now)

	assert.Equal(t, int32(1), reads.Load())
}

func hardwarePreviewEvents(t *testing.T, output *bytes.Buffer) []noderec.NodeInfoEvent {
	t.Helper()
	var events []noderec.NodeInfoEvent
	decoder := json.NewDecoder(output)
	for decoder.More() {
		var message Message
		require.NoError(t, decoder.Decode(&message))
		assert.NotEqual(t, noderec.NotifyNodeTelemetry, message.Method, "a public preview must never reach scheduling")
		if message.Method == noderec.NotifyNodeInfo {
			var event noderec.NodeInfoEvent
			require.NoError(t, json.Unmarshal(message.Params, &event))
			events = append(events, event)
		}
	}
	return events
}

// A public responder supplies plausible hardware alongside forged identity and
// service metadata. Emit only a display preview for the existing discovery;
// neither directory state nor scheduling and liveness evidence may change.
func TestDispatchHardwarePreviews_EmitsDisplayDataWithoutChangingServiceState(t *testing.T) {
	claimedPrincipal := "forged-principal"
	info := noderec.NodeInfo{
		HostUUID: "forged-host", ClusterUUID: &claimedPrincipal,
		GPUs:           []noderec.GPUInfo{{Name: "preview GPU", UtilizationPercent: 97}},
		CPU:            &noderec.CPUInfo{Name: "preview CPU", Cores: 12},
		Memory:         &noderec.MemoryInfo{TotalBytes: 64000, UsedBytes: 32000},
		TelemetryValid: true, MSSince: 1200,
		Peer: &noderec.PeerMetadata{Name: "forged name", Addresses: []string{"192.0.2.99"}, Services: map[noderec.ServiceKey]int{noderec.ServiceOllama: 11434}},
	}
	d, raw := newHardwarePreviewDiscovery(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/node-info", r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(info))
	})
	before, present := d.dir.get(nodeDiscoveryRecordID(raw))
	require.True(t, present)
	var output bytes.Buffer
	d.codec = NewCodec(&output)

	dispatchHardwarePreviewsForTest(d, time.Now())

	events := hardwarePreviewEvents(t, &output)
	require.Len(t, events, 1)
	preview := events[0]
	assert.Equal(t, before.DiscoveryRecordID, preview.DiscoveryRecordID)
	assert.Equal(t, "advertised-host", preview.HostUUID)
	assert.Equal(t, noderec.IdentityUnverified, preview.IdentityVerification)
	assert.Empty(t, preview.ClusterUUID)

	// The event keeps the discovery record's claims for display, while its
	// hardware body discards the public responder's identity and registry data.
	assert.Empty(t, preview.Info.HostUUID)
	assert.Nil(t, preview.Info.ClusterUUID)
	assert.Nil(t, preview.Info.Peer)

	assert.Equal(t, info.GPUs, preview.Info.GPUs)
	assert.Equal(t, info.CPU, preview.Info.CPU)
	assert.Equal(t, info.Memory, preview.Info.Memory)

	assert.True(t, preview.Info.TelemetryValid)
	assert.GreaterOrEqual(t, preview.Info.MSSince, int64(1200))

	after, present := d.dir.get(before.DiscoveryRecordID)
	require.True(t, present)
	assert.Equal(t, before, after, "public display data must not populate directory hardware or metadata")

	assert.Empty(t, d.nodeInfoStatus, "a preview is not authenticated proof of availability")
}

// Delay a public response while its discovery is removed, changed, or verified.
// The late response must not update the changed record, even if the original
// endpoint disappears and is rediscovered with the same address.
func TestDispatchHardwarePreviews_DiscardsResponsesWhenDiscoveryChanges(t *testing.T) {
	test := func(name string, change func(*testing.T, *daemon, RawNode)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			server, waitForRequest, releaseRead := newBlockedPreviewServer(t,
				noderec.NodeInfo{GPUs: []noderec.GPUInfo{{Name: "obsolete GPU"}}}, 1)
			d, raw := hardwarePreviewDiscoveryAt(t, server.URL)
			var output bytes.Buffer
			d.codec = NewCodec(&output)
			var workers sync.WaitGroup
			d.dispatchHardwarePreviews(context.Background(), time.Now(), make(chan struct{}, noderec.HardwarePreviewConcurrency), &workers)
			t.Cleanup(func() {
				releaseRead()
				workers.Wait()
			})
			waitForRequest()

			change(t, d, raw)
			releaseRead()
			workers.Wait()

			assert.Empty(t, hardwarePreviewEvents(t, &output))
		})
	}

	test("announcement removed", func(_ *testing.T, d *daemon, raw RawNode) {
		d.onBrowse(DiscoveryEvent{Type: "removed", Node: raw})
	})
	test("same endpoint rediscovered", func(_ *testing.T, d *daemon, raw RawNode) {
		d.onBrowse(DiscoveryEvent{Type: "removed", Node: raw})
		d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	})
	test("advertised identity replaced", func(_ *testing.T, d *daemon, raw RawNode) {
		raw.TXT = append(append([]string(nil), raw.TXT...), "uuid=replacement-host")
		d.onBrowse(DiscoveryEvent{Type: "updated", Node: raw})
	})
	test("secondary address replaced", func(t *testing.T, d *daemon, raw RawNode) {
		d.peerMu.Lock()
		defer d.peerMu.Unlock()
		node, present := d.dir.get(nodeDiscoveryRecordID(raw))
		require.True(t, present)
		node.IPs = []string{"127.0.0.1", "192.0.2.42"}
		d.dir.upsert(node)
	})
	test("node-info port replaced", func(t *testing.T, d *daemon, raw RawNode) {
		d.peerMu.Lock()
		defer d.peerMu.Unlock()
		node, present := d.dir.get(nodeDiscoveryRecordID(raw))
		require.True(t, present)
		node.Services = maps.Clone(node.Services)
		node.Services[noderec.ServiceNodeInfo] = noderec.ServiceStatus{Port: 1}
		d.dir.upsert(node)
	})
	test("discovery promoted to verified peer", func(t *testing.T, d *daemon, raw RawNode) {
		d.peerMu.Lock()
		defer d.peerMu.Unlock()
		node, present := d.dir.get(nodeDiscoveryRecordID(raw))
		require.True(t, present)
		node.IdentityVerification = noderec.IdentityPinned
		node.DiscoveryRecordID = "verified-record"
		d.dir.promote(nodeDiscoveryRecordID(raw), node)
	})
}

func TestDispatchHardwarePreviews_SkipsLocalAndPinnedRecords(t *testing.T) {
	test := func(name, hostUUID string, verification noderec.IdentityVerification) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			d, raw := newHardwarePreviewDiscovery(t, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				assert.NoError(t, json.NewEncoder(w).Encode(noderec.NodeInfo{GPUs: []noderec.GPUInfo{}}))
			})
			node, present := d.dir.get(nodeDiscoveryRecordID(raw))
			require.True(t, present)
			// Verified records use host UUID keys. Remove the unverified
			// candidate so this case contains no eligible public-preview target.
			d.dir.remove(node.DiscoveryRecordID)
			node.HostUUID = hostUUID
			node.IdentityVerification = verification
			d.dir.upsert(node)

			dispatchHardwarePreviewsForTest(d, time.Now())

			assert.Zero(t, requests.Load(), "local and pinned measurements must not use public HTTP")
		})
	}

	test("local record uses owned IPC", "self-uuid", noderec.IdentityLocal)
	test("pinned peer uses authenticated collection", "advertised-host", noderec.IdentityPinned)
}

// Preview dispatch must apply retry backoff per record while healthy previews
// keep running. Exact delay boundaries are covered by the retry-gate tests;
// this test checks that public preview results use that gate correctly.
func TestDispatchHardwarePreviews_BacksOffFailuresWithoutDelayingHealthyDiscoveries(t *testing.T) {
	var failedReads, healthyReads atomic.Int32
	d, _ := newHardwarePreviewDiscovery(t, func(w http.ResponseWriter, _ *http.Request) {
		failedReads.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyReads.Add(1)
		assert.NoError(t, json.NewEncoder(w).Encode(noderec.NodeInfo{GPUs: []noderec.GPUInfo{}}))
	}))
	t.Cleanup(healthyServer.Close)
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{
		ID: "healthy preview", Host: "healthy.local.", Addresses: []string{"127.0.0.1"},
		TXT: []string{"uuid=healthy-host", fmt.Sprintf("ni=%d", portFromURL(t, healthyServer.URL))},
	}})
	initialDispatchAt := time.Now()

	dispatchHardwarePreviewsForTest(d, initialDispatchAt)

	assert.Equal(t, int32(1), failedReads.Load())
	assert.Equal(t, int32(1), healthyReads.Load())

	// Reuse a time from before the first batch completed. The failed record
	// cannot be due yet, regardless of how long those HTTP requests took.
	dispatchHardwarePreviewsForTest(d, initialDispatchAt)

	assert.Equal(t, int32(1), failedReads.Load())
	assert.Equal(t, int32(2), healthyReads.Load())

	// Both batches have finished, so this time is beyond any retry deadline
	// capped at 30 seconds. No assumption about request duration is needed.
	retryAfterCompletion := time.Now().Add(30 * time.Second)
	dispatchHardwarePreviewsForTest(d, retryAfterCompletion)

	assert.Equal(t, int32(2), failedReads.Load())
	assert.Equal(t, int32(3), healthyReads.Load())
}

// Occupy every public-preview reader with a blocked HTTP response. Authenticated
// peer sampling must still complete using its separate collection capacity, so
// slow unpaired nodes cannot consume the readers reserved for verified peers.
func TestDispatchHardwarePreviews_SamplesVerifiedPeersWhilePreviewWorkersAreBusy(t *testing.T) {
	f := newDiscoveryPeerFixture(t, "verified-peer")
	f.discover(t)
	var verifiedReads atomic.Int32
	f.respond = func(http.ResponseWriter, *http.Request) bool {
		verifiedReads.Add(1)
		return false
	}
	server, waitForReaders, releaseReads := newBlockedPreviewServer(t,
		noderec.NodeInfo{GPUs: []noderec.GPUInfo{}}, noderec.HardwarePreviewConcurrency)
	for i := range noderec.HardwarePreviewConcurrency {
		f.daemon.onBrowse(DiscoveryEvent{Type: "discovered", Node: RawNode{
			ID: fmt.Sprintf("slow preview %d", i), Host: "unpaired.local.", Addresses: []string{"127.0.0.1"},
			TXT: []string{fmt.Sprintf("uuid=unpaired-%d", i), fmt.Sprintf("ni=%d", portFromURL(t, server.URL))},
		}})
	}
	var workers sync.WaitGroup
	f.daemon.dispatchHardwarePreviews(context.Background(), time.Now(), make(chan struct{}, noderec.HardwarePreviewConcurrency), &workers)
	t.Cleanup(func() {
		releaseReads()
		workers.Wait()
	})
	waitForReaders()

	f.daemon.refreshTelemetryOnce(context.Background())

	assert.Equal(t, int32(1), verifiedReads.Load())

	releaseReads()
	workers.Wait()
}

// Each discovery has two responding addresses. Holding their responses open must
// still allow only four HTTP reads in total, rather than four nodes each reading
// several addresses. Releasing them must let every discovery publish a preview.
func TestDispatchHardwarePreviews_LimitsConcurrentReadsAcrossNodesAndAddresses(t *testing.T) {
	const readerLimit = 4
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	releaseReads := sync.OnceFunc(func() { close(release) })
	var requests atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(noderec.NodeInfo{GPUs: []noderec.GPUInfo{}}))
	}
	d, raw := newHardwarePreviewDiscovery(t, handler)
	port := noderec.ParseTXT(raw.TXT).Services[noderec.ServiceNodeInfo]
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", fmt.Sprint(port)))
	require.NoError(t, err)
	alternative := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	require.NoError(t, alternative.Listener.Close())
	alternative.Listener = listener
	alternative.Start()
	t.Cleanup(alternative.Close)
	d.onBrowse(DiscoveryEvent{Type: "removed", Node: raw})
	raw.Addresses = []string{"127.0.0.1", "127.0.0.2"}
	d.onBrowse(DiscoveryEvent{Type: "discovered", Node: raw})
	for i := range 4 {
		alias := raw
		alias.ID = fmt.Sprintf("additional unpaired host %d", i)
		d.onBrowse(DiscoveryEvent{Type: "discovered", Node: alias})
	}
	var output bytes.Buffer
	d.codec = NewCodec(&output)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	d.dispatchHardwarePreviews(ctx, time.Now(), make(chan struct{}, noderec.HardwarePreviewConcurrency), &workers)
	t.Cleanup(func() {
		cancel()
		releaseReads()
		workers.Wait()
	})

	for range readerLimit {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("four preview requests did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("a fifth preview request started while all four readers were busy")
	case <-time.After(100 * time.Millisecond):
	}

	releaseReads()
	workers.Wait()

	assert.GreaterOrEqual(t, requests.Load(), int32(5))
	assert.Len(t, hardwarePreviewEvents(t, &output), 5, "every discovery must receive a turn")
}
