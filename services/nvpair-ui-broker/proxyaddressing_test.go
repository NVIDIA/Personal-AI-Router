// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Coverage for the engine addressing on the broker↔proxy wire.
//
// One process per engine made the engine implicit: whichever read pump
// delivered a notification decided which engine it belonged to, and whichever
// process a call was sent to decided which engine applied it. A process hosting
// several facades has neither property, so both directions carry an explicit
// engine and these tests pin what happens when it is absent, present, or wrong.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/engines"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// The relay must address what it sends downward, not just strip the client's
// prefix.
//
// Nothing else catches this: dropping profile.addressed() in relayToEngineProxy
// compiles, and the child answers -32002 because an unaddressed facade-scoped
// method resolves to no facade — which looks like an unavailable proxy rather
// than a broker bug. The two prefixes are deliberately different strings, so
// the relay is a translation and this asserts both halves of it.
func TestRelay_AddressesForwardedMethodToRequestedFacade(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() {
				_ = client.Close()
				_ = server.Close()
			})
			proxy := &proxyProcess{peer: NewPeer(NewCodec(client))}
			go proxy.peer.Serve(nil, nil)

			seen := make(chan string, 1)
			go func() {
				codec := NewCodec(server)
				msg, err := codec.Read()
				if !assertRPCRead(t, err) {
					return
				}
				seen <- msg.Method
				assert.NoError(t, codec.Respond(msg.ID, map[string][]string{"nodes": {}}))
			}()

			b := &Broker{codec: NewCodec(rwDiscard{})}
			b.setEngineProxyHandle(profile, proxy)

			id := json.RawMessage(`1`)
			b.relayToEngineProxy(profile, &Message{
				Method: profile.ComponentName() + ":nodes/list",
				ID:     &id,
			})

			select {
			case got := <-seen:
				require.Equal(t, profile.addressed("nodes/list"), got, "relayed method")
			case <-time.After(2 * time.Second):
				require.FailNow(t, "relay sent nothing downward")
			}
		})
	}
}

// rwDiscard is a client transport that reads nothing and swallows writes, for a
// relay whose reply nobody inspects.
type rwDiscard struct{}

func (rwDiscard) Read([]byte) (int, error)    { return 0, io.EOF }
func (rwDiscard) Write(p []byte) (int, error) { return len(p), nil }
func (rwDiscard) Close() error                { return nil }

// Giving up the managed claim keeps the OLLAMA_HOST alias reservation, so a
// spawn whose every facade failed still has it when the supervisor retries.
//
// This is the half of the block/finish split that the enabled==0 path depends
// on: it may run the block, and must not run the finish. Releasing the alias
// there was a regression, because the retry still needs it and engine-manager
// could take that port in between, so an attempt that would otherwise have
// succeeded comes up without the inherited endpoint.
//
// The call-site ordering itself is not reachable from a unit test — spawnProxy
// launches a real binary — so this pins the property that makes the ordering
// matter, and the sibling test below pins the other side of it.
func TestBlockingAManagedFacade_KeepsTheAliasForTheRetry(t *testing.T) {
	isolateOllamaHostTestConfig(t)
	b := &Broker{
		nodeID:            "local-node",
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	alias := ollamaHostAlias{Address: "127.0.0.1:11433", Port: 11433}
	b.setOllamaHostAlias(alias)
	b.ollamaState().managedFacade.Store(true)

	b.blockManagedOllamaFacade("the Ollama proxy facade could not be brought up")

	got := b.currentOllamaHostAlias()
	assert.Equal(t, alias.Port, got.Port, "blocking released the alias a retry still needs (%v)", got)
}

// An engine that failed inside a process that is staying gets terminal
// treatment, which is what finally returns the alias.
//
// Without this the broker would hold a reservation for a facade that is never
// coming back, and engine-manager could never be given that port.
func TestSurvivingProcess_ReleasesTheAliasForAFailedFacade(t *testing.T) {
	b := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	alias := ollamaHostAlias{Address: "127.0.0.1:11433", Port: 11433}
	b.setOllamaHostAlias(alias)
	got := b.currentOllamaHostAlias()
	require.Equal(t, alias.Port, got.Port, "alias not established for the test (%v)", got)

	b.blockAndFinishEngineProxy(ollamaProxyProfile)
	got = b.currentOllamaHostAlias()
	assert.Equal(t, 0, got.Port, "terminal treatment did not release the alias (%v)", got)
}

// A facade that reported ready is kept when its enable call went unanswered,
// and disowned when the child answered with a rejection.
//
// Both halves matter and neither runs on the ordinary error return, so without
// this test a later edit can either strand a live listener the broker reports
// as unavailable, or publish a dead port the broker will never retry.
func TestFacadeEnable_TrustsReadinessOnlyWhenRequestWasUnanswered(t *testing.T) {
	engine := ollamaProxyProfile.Name
	ready := &proxyProcess{facadeState: readyFacade(engine, 11434)}
	notReady := &proxyProcess{}

	test := func(name string, pp *proxyProcess, err error, want bool) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, facadeCameUpAnyway(pp, engine, err))
		})
	}
	// The child does the bind, notifications and serve inside an unanswered call,
	// so it may genuinely be serving despite a timeout or closed pipe.
	test("unanswered enable, facade reported ready", ready, context.DeadlineExceeded, true)
	test("unanswered enable, facade never reported ready", notReady, context.DeadlineExceeded, false)
	// An answered rejection withdraws the facade, making an earlier ready stale.
	test("answered rejection, stale ready ignored", ready, fmt.Errorf("%w: rejected", errRPCAnswered), false)
	test("answered bind failure, stale ready ignored", ready, fmt.Errorf("%w: %w: taken", errRPCAnswered, errFacadeBindFailed), false)
}

// readyFacade is the handle state one engine's facade would leave behind after
// announcing itself ready on a port.
func readyFacade(engine string, port int) map[string]proxyFacadeState {
	return map[string]proxyFacadeState{
		engine: {ready: true, port: port},
	}
}

// Readiness is tracked per engine because one process hosts several facades on
// different ports. A single port for the process would be whichever facade
// readied last, so get-status would hand a client the other engine's port —
// and its inference traffic with it.
func TestReadiness_IsTrackedPerEngine(t *testing.T) {
	all := engines.All()
	if len(all) < 2 {
		t.Skip("needs at least two engines")
	}
	first, second := all[0], all[1]

	p := &proxyProcess{}
	p.handleNotify(
		engines.AddressMethod(first.Name, "ready"),
		json.RawMessage(`{"version":"test","port":11434}`),
	)
	p.handleNotify(
		engines.AddressMethod(second.Name, "ready"),
		json.RawMessage(`{"version":"test","port":1234}`),
	)

	for engine, wantPort := range map[string]int{first.Name: 11434, second.Name: 1234} {
		ready, port := p.Status(engine)
		assert.True(t, ready, "engine %s must be ready", engine)
		assert.Equal(t, wantPort, port, "engine %s facade port", engine)
		assert.NotNil(t, p.ReadyParams(engine), "engine %s ready params", engine)
	}

	// An engine that never announced itself is not ready, rather than
	// inheriting a sibling's port.
	ready, port := p.Status("vllm")
	assert.False(t, ready, "an unannounced engine reported ready")
	assert.Equal(t, 0, port, "an unannounced engine reported a port")
}

func TestBrokerFacadeMethods_FollowEngineProfile(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() {
				_ = client.Close()
				_ = server.Close()
			})
			payload := json.RawMessage(fmt.Sprintf(`{"version":"test","port":%d}`, profile.FacadePort))
			proxy := &proxyProcess{facadeState: map[string]proxyFacadeState{
				profile.Name: {
					ready:  true,
					port:   profile.FacadePort,
					params: payload,
				},
			}}
			b := &Broker{codec: NewCodec(server)}
			b.setEngineProxyHandle(profile, proxy)
			reader := NewCodec(client)
			nextID := 0
			call := func(method string, frameCount int) []*Message {
				t.Helper()
				nextID++
				id := json.RawMessage(fmt.Sprintf("%d", nextID))
				done := make(chan struct{})
				go func() {
					b.handleMessage(&Message{JSONRPC: "2.0", ID: &id, Method: profile.ComponentName() + ":" + method})
					close(done)
				}()
				frames := make([]*Message, 0, frameCount)
				for range frameCount {
					require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)), "set read deadline")
					frame, err := reader.Read()
					require.NoError(t, err, "read %s frame", method)
					frames = append(frames, frame)
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					require.FailNowf(t, "handler did not finish", "%s after %d frame(s)", method, frameCount)
				}
				return frames
			}

			statusFrames := call("get-status", 1)
			var status ProxyStatusResult
			require.NoError(t, json.Unmarshal(statusFrames[0].Result, &status), "decode status")
			assert.True(t, status.Ready)
			assert.Equal(t, profile.FacadePort, status.Port)

			subscribeFrames := call("subscribe", 2)
			var subscribed SubscriptionResult
			require.NoError(t, json.Unmarshal(subscribeFrames[0].Result, &subscribed), "subscribe response")
			assert.True(t, subscribed.Subscribed)
			assert.Equal(t, profile.ComponentName()+":ready", subscribeFrames[1].Method, "ready baseline after response")
			assert.Equal(t, string(payload), string(subscribeFrames[1].Params), "ready baseline")

			unsubscribeFrames := call("unsubscribe", 1)
			require.NoError(t, json.Unmarshal(unsubscribeFrames[0].Result, &subscribed), "unsubscribe response")
			assert.False(t, subscribed.Subscribed)
			b.proxyMu.Lock()
			stillSubscribed := b.engineProxySubscribed(profile)
			b.proxyMu.Unlock()
			assert.False(t, stillSubscribed, "facade remained subscribed after unsubscribe")
		})
	}
}

// Each facade subscribes for its own engine's discovery service, so the broker
// has to track a subscription per engine. A single id per process let the second
// facade's subscribe replace the first's, which unsubscribed a live facade and
// left that engine with a routing set that never updated again.
func TestSubscriptions_AreTrackedPerEngine(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	// Drain whatever the directory pushes, so Deliver cannot block on the pipe.
	go func() {
		codec := NewCodec(server)
		for {
			if _, err := codec.Read(); err != nil {
				return
			}
		}
	}()

	dir := relay.NewDirectory()
	p := &proxyProcess{peer: NewPeer(NewCodec(client)), relayDir: dir}

	subscribe := func(engine string, service noderec.ServiceKey) {
		params, err := json.Marshal(noderec.SubscribeParams{
			Services: []noderec.ServiceKey{service},
		})
		require.NoError(t, err, "marshal subscribe for %s", engine)
		p.handleSubscribe(engine, params)
	}

	for _, e := range engines.All() {
		subscribe(e.Name, e.DiscoveryService)
	}

	p.subMu.Lock()
	defer p.subMu.Unlock()
	require.Len(t, p.subIDs, len(engines.All()), "tracked")
	seen := map[int]string{}
	for engine, id := range p.subIDs {
		assert.NotEqual(t, 0, id, "engine (%v)", engine)
		assert.NotContains(t, seen, id, "engine %s reused a subscription ID", engine)
		seen[id] = engine
	}
}

// The read pump reports ready and wires subscriptions by matching bare method
// names, so it has to see through the address. An unaddressed notification is
// process-scoped and passes through untouched.
func TestFacadeMethodFor_StripsOnlyItsOwnEngine(t *testing.T) {
	test := func(name string, profile engineProxyProfile, method, want string, wantOK bool) {
		t.Run(name, func(t *testing.T) {
			got, ok := facadeMethodFor(profile, method)
			require.Equal(t, wantOK, ok)
			if ok {
				require.Equal(t, want, got)
			}
		})
	}
	test("own address is stripped", ollamaProxyProfile, ollamaProxyProfile.addressed("ready"), "ready", true)
	// Another engine's address means routing broke: forwarding would file one
	// engine's event under another.
	test("another engine's address is dropped", ollamaProxyProfile, lmstudioProxyProfile.addressed("ready"), "", false)
	test("process-scoped method passes through", ollamaProxyProfile, "workload:started", "workload:started", true)
	// The errors relay matches bare names, so an addressed error must come out bare.
	test("addressed error becomes a bare errors method", ollamaProxyProfile, ollamaProxyProfile.addressed(methodErrorsReport), methodErrorsReport, true)
	test("unaddressed error is untouched", lmstudioProxyProfile, methodErrorsReport, methodErrorsReport, true)
}

// A proxy with a full stdin pipe must not hold discovery processing or prevent
// pin withdrawal. After recovery it needs the latest empty routing snapshot,
// rather than the intermediate records accumulated while it was not reading.
func TestProxyDiscovery_CoalescesWhilePipeIsBlocked(t *testing.T) {
	brokerSide, proxySide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, brokerSide.Close())
		assert.NoError(t, proxySide.Close())
	})
	require.NoError(t, proxySide.SetDeadline(time.Now().Add(6*time.Second)))
	writer := &observedProxyWriter{Conn: brokerSide, writing: make(chan struct{})}
	b := newManualTestBroker()
	b.nodeID, b.clusterDir = "self", t.TempDir()
	clustertrusttest.Join(t, b.clusterDir, "cluster", "self", "peer")
	mesh := clustertrust.Open(b.clusterDir)
	b.relayDir = relay.NewDirectory()
	scanner := &scannerProcess{store: b.store, relayDir: b.relayDir, stateMu: &b.discoveryMu, mesh: mesh}
	node := noderec.DirectoryNode{
		HostUUID: "peer", Name: "initial", DiscoveryRecordID: mesh.PeerRecordID("peer"),
		IdentityVerification: noderec.IdentityPinned, Trusted: true,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceOllama: {Port: 11434}},
	}
	initialEvent, err := json.Marshal(noderec.NodeEvent{Node: node})
	require.NoError(t, err)
	scanner.handleNotify(noderec.NotifyNodeDiscovered, initialEvent)
	proxy := &proxyProcess{peer: NewPeer(NewCodec(writer)), relayDir: b.relayDir}
	params, err := json.Marshal(noderec.SubscribeParams{Services: []noderec.ServiceKey{noderec.ServiceOllama}})
	require.NoError(t, err)
	subscribed := make(chan struct{})
	go func() {
		proxy.handleSubscribe("ollama", params)
		close(subscribed)
	}()
	select {
	case <-writer.writing:
	case <-time.After(time.Second):
		require.FailNow(t, "the initial snapshot was not sent")
	}
	finished := func(done <-chan struct{}, operation string) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(time.Second):
			assert.Fail(t, operation+" waited for the blocked proxy")
		}
	}
	node.Name = "intermediate"
	updatedEvent, err := json.Marshal(noderec.NodeEvent{Node: node})
	require.NoError(t, err)
	scanned := make(chan struct{})
	go func() {
		for range 20 {
			scanner.handleNotify(noderec.NotifyNodeUpdated, updatedEvent)
		}
		close(scanned)
	}()
	finished(scanned, "scanner update")
	owned := make(chan struct{})
	go func() {
		b.nodeInfoNotificationHandler(0)(noderec.NotifyInfoUpdated, json.RawMessage(`{"info":{"hostUuid":"self"}}`))
		close(owned)
	}()
	clustertrusttest.RemovePeerPin(t, b.clusterDir, "peer")
	withdrawn := make(chan struct{})
	go func() {
		b.reconcileDiscoveryTrust()
		close(withdrawn)
	}()
	finished(owned, "local hardware update")
	finished(withdrawn, "trust withdrawal")
	finished(subscribed, "subscription setup")

	codec := NewCodec(proxySide)
	initial, err := codec.Read()
	require.NoError(t, err)
	require.Equal(t, "ollama:discovery:nodes", initial.Method)
	var first noderec.GetNodesResult
	require.NoError(t, json.Unmarshal(initial.Params, &first))
	require.Len(t, first.Nodes, 1)
	assert.Equal(t, "initial", first.Nodes[0].Name)
	last, err := codec.Read()
	require.NoError(t, err)
	assert.Equal(t, "ollama:discovery:nodes", last.Method)
	var final noderec.GetNodesResult
	require.NoError(t, json.Unmarshal(last.Params, &final))
	assert.NotNil(t, final.Nodes, "an empty authoritative snapshot is encoded as []")
	assert.Empty(t, final.Nodes)
	finished(scanned, "scanner after pipe recovery")
	finished(owned, "local hardware after pipe recovery")
	finished(withdrawn, "trust withdrawal after pipe recovery")
	waitForProxySnapshotSender(t, proxy)
}

func waitForProxySnapshotSender(t *testing.T, proxy *proxyProcess) {
	t.Helper()
	// An idle sender also proves that no extra old snapshot remains blocked on
	// the pipe after the expected complete snapshots have been consumed.
	require.Eventually(t, func() bool {
		proxy.subMu.Lock()
		defer proxy.subMu.Unlock()
		return !proxy.sendingNodes
	}, time.Second, time.Millisecond)
}

// One facade changing its filter must not discard another facade's pending
// update, or let its previous subscription overwrite the replacement's set.
func TestProxyDiscovery_KeepsEachEnginesLatestSubscriptionSnapshot(t *testing.T) {
	brokerSide, proxySide := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, brokerSide.Close())
		assert.NoError(t, proxySide.Close())
	})
	require.NoError(t, proxySide.SetDeadline(time.Now().Add(3*time.Second)))
	writer := &observedProxyWriter{Conn: brokerSide, writing: make(chan struct{})}
	directory := relay.NewDirectory()
	node := noderec.DirectoryNode{
		HostUUID: "peer", DiscoveryRecordID: "peer:peer", Name: "initial",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceOllama: {Port: 11434}, noderec.ServiceLMStudio: {Port: 1234},
		},
	}
	directory.Apply(noderec.NotifyNodeDiscovered, node)
	proxy := &proxyProcess{peer: NewPeer(NewCodec(writer)), relayDir: directory}
	params := func(service noderec.ServiceKey) json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(noderec.SubscribeParams{Services: []noderec.ServiceKey{service}})
		require.NoError(t, err)
		return raw
	}
	ollamaParams, lmstudioParams, nodeInfoParams := params(noderec.ServiceOllama), params(noderec.ServiceLMStudio), params(noderec.ServiceNodeInfo)
	subscribed := make(chan struct{})
	go func() {
		proxy.handleSubscribe("ollama", ollamaParams)
		close(subscribed)
	}()
	select {
	case <-writer.writing:
	case <-time.After(time.Second):
		require.FailNow(t, "the initial snapshot was not sent")
	}
	select {
	case <-subscribed:
	case <-time.After(time.Second):
		require.FailNow(t, "subscription setup waited for the blocked writer")
	}
	node.Name = "latest"
	var initialOllamaID, initialLMStudioID int
	var currentOllamaID, currentLMStudioID int
	updated := make(chan struct{})
	go func() {
		proxy.handleSubscribe("lmstudio", lmstudioParams)
		// Capture the IDs after both facades have subscribed. Only Ollama's
		// registration should change when it requests a different filter.
		proxy.subMu.Lock()
		initialOllamaID, initialLMStudioID = proxy.subIDs["ollama"], proxy.subIDs["lmstudio"]
		proxy.subMu.Unlock()
		proxy.handleSubscribe("ollama", nodeInfoParams)
		proxy.subMu.Lock()
		currentOllamaID, currentLMStudioID = proxy.subIDs["ollama"], proxy.subIDs["lmstudio"]
		proxy.subMu.Unlock()
		directory.Apply(noderec.NotifyNodeUpdated, node)
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(time.Second):
		require.FailNow(t, "updating subscriptions waited for the blocked writer")
	}
	// The channel close publishes the before/after IDs as well as completing
	// both subscription changes while the first snapshot write is still held.
	assert.NotEqual(t, initialOllamaID, currentOllamaID, "Ollama must replace its subscription")
	assert.Equal(t, initialLMStudioID, currentLMStudioID, "Ollama's re-subscribe must leave LM Studio's subscription intact")

	codec := NewCodec(proxySide)
	initial, err := codec.Read()
	require.NoError(t, err)
	assert.Equal(t, "ollama:discovery:nodes", initial.Method)
	var first noderec.GetNodesResult
	require.NoError(t, json.Unmarshal(initial.Params, &first))
	require.Len(t, first.Nodes, 1)
	assert.Equal(t, "initial", first.Nodes[0].Name)
	snapshots := make(map[string][]noderec.DirectoryNode)
	for range 2 {
		message, err := codec.Read()
		require.NoError(t, err)
		require.NotContains(t, snapshots, message.Method)
		var result noderec.GetNodesResult
		require.NoError(t, json.Unmarshal(message.Params, &result))
		snapshots[message.Method] = result.Nodes
	}
	assert.Equal(t, map[string][]noderec.DirectoryNode{
		"ollama:discovery:nodes":   {},
		"lmstudio:discovery:nodes": {node},
	}, snapshots)
	waitForProxySnapshotSender(t, proxy)
}
