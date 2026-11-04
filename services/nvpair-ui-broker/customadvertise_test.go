// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Advertise-plane contract for Custom endpoints (Task 9).
//
// These pin what this node publishes when Custom endpoints are configured:
//
//   - The custom facade is advertised (cu=<bound port>) only while at least
//     one candidate exists AND the facade is ready. A peer that saw cu and
//     routed into a down facade would dial a dead port, so advertisement
//     follows routability, not configuration.
//   - The model set travels to engine-manager (custom:set-models) so peers'
//     /v1/models fetches attribute custom models, keeping key-presence
//     semantics: a non-empty push sets ByEngine["custom"], a later empty
//     push removes it.
//   - The push is change-gated (model set + worker identity), so steady
//     state keeps the broker↔em wire quiet, while a replaced engine-manager
//     re-receives the current set without any event.

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

type pushedFrame struct {
	method string
	params string
}

// newEngineMgrFaker builds an rpcWorker over one end of a net.Pipe whose other
// end records every incoming notification frame — the frame a real
// engine-manager would decode. Nothing answers: the broker's push is
// fire-and-forget by design, so an ack would be dead weight on the wire.
func newEngineMgrFaker(t *testing.T, seen chan pushedFrame) *rpcWorker {
	t.Helper()
	brokerSide, serverSide := net.Pipe()
	t.Cleanup(func() {
		_ = brokerSide.Close()
		_ = serverSide.Close()
	})
	w := &rpcWorker{name: "engine-manager", peer: NewPeer(NewCodec(brokerSide)), done: make(chan struct{})}
	go func() {
		codec := NewCodec(serverSide)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if msg.ID != nil || msg.Method == "" {
				continue
			}
			select {
			case seen <- pushedFrame{method: msg.Method, params: string(msg.Params)}:
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return w
}

func customAdvertiseBroker(t *testing.T, em *rpcWorker) *Broker {
	t.Helper()
	b := &Broker{
		store:    newDiscoveryStore(),
		regCache: relay.NewRegistrationCache(),
	}
	if em != nil {
		b.setEngineMgr(em)
	}
	return b
}

// readyCustomFacade puts the broker's custom-facade handle in the state a live
// proxy leaves after announcing itself on port.
func readyCustomFacade(b *Broker, port int) {
	b.setEngineProxyHandle(customProxyProfile, &proxyProcess{
		facadeState: readyFacade(customProxyProfile.Name, port),
	})
}

// seedCustomEndpoint writes the store state the custom bridge leaves behind for
// a healthy bridged endpoint — the candidate truth reconcileCustomAdvertise
// reads.
func seedCustomEndpoint(b *Broker, id string, models ...string) {
	b.store.Upsert(
		customToEnriched(customEndpointStatus{ID: id, BaseURL: "http://127.0.0.1:9099", Healthy: true, Models: models}, "127.0.0.1", 9099),
		sourceCustom,
	)
}

func customAdService(b *Broker) (int, bool) {
	for _, p := range b.regCache.Snapshot() {
		if p.Service == noderec.ServiceCustom {
			return p.Port, true
		}
	}
	return 0, false
}

// takePushes drains recorded frames for d, waiting up to 3s for the first so a
// missing push fails as "nothing sent", not an empty-slice coincidence.
func takePushes(t *testing.T, seen chan pushedFrame, d time.Duration) []pushedFrame {
	t.Helper()
	var frames []pushedFrame
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case frame := <-seen:
			frames = append(frames, frame)
		case <-timer.C:
			return frames
		}
	}
}

func decodePushedModels(t *testing.T, frame pushedFrame) []string {
	t.Helper()
	var payload struct {
		Models []string `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(frame.params), &payload), "decode push params (%v)", frame.params)
	return payload.Models
}

// A configured, healthy endpoint with a ready facade is the advertise state:
// cu appears on the node record at the facade's bound port, and the model set
// reaches engine-manager.
func TestReconcileCustomAdvertise_FacadeUpWithCandidatesRegistersAndPushes(t *testing.T) {
	seen := make(chan pushedFrame, 8)
	b := customAdvertiseBroker(t, newEngineMgrFaker(t, seen))
	seedCustomEndpoint(b, "custom:http://127.0.0.1:9099", "m-one")
	readyCustomFacade(b, 8000)

	b.reconcileCustomAdvertise()

	port, registered := customAdService(b)
	require.True(t, registered, "candidates with a ready facade must advertise cu")
	assert.Equal(t, 8000, port, "cu advertises the facade's bound port, never the stock default")

	frames := takePushes(t, seen, 3*time.Second)
	require.Len(t, frames, 1, "the model set must be pushed to engine-manager once")
	assert.Equal(t, "custom:set-models", frames[0].method, "push method")
	assert.Equal(t, []string{"m-one"}, decodePushedModels(t, frames[0]), "pushed model set")
}

// Taking the facade down must retract a previously advertised cu and clear the
// engine-manager inventory — a peer must not keep seeing (or routing toward)
// a custom set this node can no longer serve.
func TestReconcileCustomAdvertise_FacadeDownClearsAdvertiseAndPushesEmptySet(t *testing.T) {
	seen := make(chan pushedFrame, 8)
	b := customAdvertiseBroker(t, newEngineMgrFaker(t, seen))
	seedCustomEndpoint(b, "custom:http://127.0.0.1:9099", "m-one")
	readyCustomFacade(b, 8000)

	b.reconcileCustomAdvertise()
	require.NotEmpty(t, takePushes(t, seen, 3*time.Second), "baseline state must push the model set")
	_, registered := customAdService(b)
	require.True(t, registered, "baseline state must advertise cu")

	// Facade exits (handle cleared, mirroring a supervisor teardown).
	b.setEngineProxyHandle(customProxyProfile, nil)
	b.reconcileCustomAdvertise()

	_, registered = customAdService(b)
	assert.False(t, registered, "a down facade must retract cu")

	frames := takePushes(t, seen, 3*time.Second)
	require.Len(t, frames, 1, "the lost model set must be cleared with an empty push")
	assert.Equal(t, "custom:set-models", frames[0].method, "clear method")
	assert.Empty(t, decodePushedModels(t, frames[0]), "the clear must carry an empty set, not a stale one")
}

// Steady state with nothing configured is silent and unregistered even with a
// ready facade: no candidates means no cu and no wire traffic, forever. The
// change gate keys on (model set + worker identity), so an unchanged quiet
// state must not repaint the wire every five seconds.
func TestReconcileCustomAdvertise_KeepsTheWireQuietWithNothingConfigured(t *testing.T) {
	seen := make(chan pushedFrame, 8)
	b := customAdvertiseBroker(t, newEngineMgrFaker(t, seen))
	readyCustomFacade(b, 8000)

	b.reconcileCustomAdvertise()
	b.reconcileCustomAdvertise()

	assert.Empty(t, takePushes(t, seen, 2*time.Second), "no candidates means no model pushes")
	_, registered := customAdService(b)
	assert.False(t, registered, "no candidates means cu is not advertised")
}

// A replaced engine-manager starts with an empty inventory. The identity half
// of the change gate must re-push the current set to the new worker even
// though nothing about the model set changed — this is what replays custom
// state after an engine-manager respawn without touching the spawn path.
func TestReconcileCustomAdvertise_RepushesForAReplacedEngineManager(t *testing.T) {
	seen1 := make(chan pushedFrame, 8)
	b := customAdvertiseBroker(t, newEngineMgrFaker(t, seen1))
	seedCustomEndpoint(b, "custom:http://127.0.0.1:9099", "m-one")
	readyCustomFacade(b, 8000)

	b.reconcileCustomAdvertise()
	require.NotEmpty(t, takePushes(t, seen1, 3*time.Second), "baseline push to the first engine-manager")

	seen2 := make(chan pushedFrame, 8)
	b.setEngineMgr(newEngineMgrFaker(t, seen2))
	b.reconcileCustomAdvertise()

	frames := takePushes(t, seen2, 3*time.Second)
	require.Len(t, frames, 1, "the replacement engine-manager must receive the current set")
	assert.Equal(t, []string{"m-one"}, decodePushedModels(t, frames[0]), "replayed model set")

	// And the converged state stays quiet on the new worker too.
	b.reconcileCustomAdvertise()
	assert.Empty(t, takePushes(t, seen2, 2*time.Second), "steady state on the new worker must not re-push")
}

// The pushed union is the de-duplicated, sorted set across every configured
// endpoint. Sort is load-bearing: the change gate keys on the joined set, and
// an iteration-order shuffle would flip the key every tick.
func TestCustomModelUnion_DeduplicatesAndSortsAcrossEndpoints(t *testing.T) {
	b := customAdvertiseBroker(t, nil)
	seedCustomEndpoint(b, "custom:http://127.0.0.1:9099", "z-model", "shared")
	seedCustomEndpoint(b, "custom:http://127.0.0.1:9098", "a-model", "shared")

	assert.Equal(t, []string{"a-model", "shared", "z-model"}, b.store.customModelUnion())
}
