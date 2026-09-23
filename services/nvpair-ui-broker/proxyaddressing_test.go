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
func TestRelayAddressesTheMethodItSendsDownward(t *testing.T) {
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
				if err != nil {
					return
				}
				seen <- msg.Method
				_ = codec.Respond(msg.ID, map[string][]string{"nodes": {}})
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
				want := profile.addressed("nodes/list")
				if got != want {
					t.Fatalf("relayed method = %q, want %q", got, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("relay sent nothing downward")
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
func TestBlockingAManagedFacadeKeepsTheAliasForTheRetry(t *testing.T) {
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

	if got := b.currentOllamaHostAlias(); got.Port != alias.Port {
		t.Errorf("blocking released the alias a retry still needs: %+v", got)
	}
}

// An engine that failed inside a process that is staying gets terminal
// treatment, which is what finally returns the alias.
//
// Without this the broker would hold a reservation for a facade that is never
// coming back, and engine-manager could never be given that port.
func TestSurvivingProcessReleasesTheAliasForAFailedFacade(t *testing.T) {
	b := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	alias := ollamaHostAlias{Address: "127.0.0.1:11433", Port: 11433}
	b.setOllamaHostAlias(alias)
	if got := b.currentOllamaHostAlias(); got.Port != alias.Port {
		t.Fatalf("alias not established for the test: %+v", got)
	}

	b.blockAndFinishEngineProxy(ollamaProxyProfile)
	if got := b.currentOllamaHostAlias(); got.Port != 0 {
		t.Errorf("terminal treatment did not release the alias: %+v", got)
	}
}

// A facade that reported ready is kept when its enable call went unanswered,
// and disowned when the child answered with a rejection.
//
// Both halves matter and neither runs on the ordinary error return, so without
// this test a later edit can either strand a live listener the broker reports
// as unavailable, or publish a dead port the broker will never retry.
func TestFacadeCameUpAnywayOnlyTrustsAnUnansweredEnable(t *testing.T) {
	engine := ollamaProxyProfile.Name
	ready := &proxyProcess{facadeState: readyFacade(engine, 11434)}
	notReady := &proxyProcess{}

	for _, tc := range []struct {
		name string
		pp   *proxyProcess
		err  error
		want bool
	}{
		{
			// A timeout or closed pipe: the child does the bind, two notifies
			// and the serve inside the call, so it may genuinely be serving.
			name: "unanswered enable, facade reported ready",
			pp:   ready, err: context.DeadlineExceeded, want: true,
		},
		{
			name: "unanswered enable, facade never reported ready",
			pp:   notReady, err: context.DeadlineExceeded, want: false,
		},
		{
			// The child answered, so it decided against the facade and
			// withdrew it: an earlier ready from the same attempt is stale.
			name: "answered rejection, stale ready ignored",
			pp:   ready, err: fmt.Errorf("%w: rejected", errRPCAnswered), want: false,
		},
		{
			name: "answered bind failure, stale ready ignored",
			pp:   ready,
			err:  fmt.Errorf("%w: %w: taken", errRPCAnswered, errFacadeBindFailed),
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := facadeCameUpAnyway(tc.pp, engine, tc.err); got != tc.want {
				t.Fatalf("facadeCameUpAnyway = %v, want %v", got, tc.want)
			}
		})
	}
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
func TestReadinessIsTrackedPerEngine(t *testing.T) {
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
		if !ready {
			t.Errorf("%s facade is not ready", engine)
		}
		if port != wantPort {
			t.Errorf("%s facade port = %d, want %d", engine, port, wantPort)
		}
		if p.ReadyParams(engine) == nil {
			t.Errorf("%s facade has no replayable ready payload", engine)
		}
	}

	// An engine that never announced itself is not ready, rather than
	// inheriting a sibling's port.
	if ready, port := p.Status("vllm"); ready || port != 0 {
		t.Errorf("an unannounced engine reported ready=%v port=%d", ready, port)
	}
}

// Each facade subscribes for its own engine's discovery service, so the broker
// has to track a subscription per engine. A single id per process let the second
// facade's subscribe replace the first's, which unsubscribed a live facade and
// left that engine with a routing set that never updated again.
func TestSubscriptionsAreTrackedPerEngine(t *testing.T) {
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
		if err != nil {
			t.Fatalf("marshal subscribe for %s: %v", engine, err)
		}
		p.handleSubscribe(engine, params)
	}

	for _, e := range engines.All() {
		subscribe(e.Name, e.DiscoveryService)
	}

	p.subMu.Lock()
	defer p.subMu.Unlock()
	if len(p.subIDs) != len(engines.All()) {
		t.Fatalf("tracked %d subscriptions for %d engines: %v",
			len(p.subIDs), len(engines.All()), p.subIDs)
	}
	seen := map[int]string{}
	for engine, id := range p.subIDs {
		if id == 0 {
			t.Errorf("engine %q has subscription id 0", engine)
		}
		if other, dup := seen[id]; dup {
			t.Errorf("engines %q and %q share subscription id %d", engine, other, id)
		}
		seen[id] = engine
	}
}

// A facade re-subscribing must drop only its own prior registration. Dropping
// every one would silence the other engines, and dropping none would double-feed
// this one.
func TestResubscribeReplacesOnlyThatEnginesSubscription(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
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

	all := engines.All()
	if len(all) < 2 {
		t.Skip("needs at least two engines to tell the subscriptions apart")
	}
	first, second := all[0], all[1]

	params := func(e engines.Engine) json.RawMessage {
		raw, err := json.Marshal(noderec.SubscribeParams{
			Services: []noderec.ServiceKey{e.DiscoveryService},
		})
		if err != nil {
			t.Fatalf("marshal subscribe: %v", err)
		}
		return raw
	}

	p.handleSubscribe(first.Name, params(first))
	p.handleSubscribe(second.Name, params(second))

	p.subMu.Lock()
	firstID, secondID := p.subIDs[first.Name], p.subIDs[second.Name]
	p.subMu.Unlock()

	p.handleSubscribe(first.Name, params(first))

	p.subMu.Lock()
	defer p.subMu.Unlock()
	if p.subIDs[first.Name] == firstID {
		t.Errorf("re-subscribe kept %s's original id %d, so it is now double-fed",
			first.Name, firstID)
	}
	if p.subIDs[second.Name] != secondID {
		t.Errorf("re-subscribing %s changed %s's id from %d to %d",
			first.Name, second.Name, secondID, p.subIDs[second.Name])
	}
}

// The read pump reports ready and wires subscriptions by matching bare method
// names, so it has to see through the address. An unaddressed notification is
// process-scoped and passes through untouched.
func TestFacadeMethodForStripsOnlyItsOwnEngine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile engineProxyProfile
		method  string
		want    string
		ok      bool
	}{
		{
			name:    "own address is stripped",
			profile: ollamaProxyProfile,
			method:  ollamaProxyProfile.addressed("ready"),
			want:    "ready",
			ok:      true,
		},
		{
			// forwardProxyProcessNotification routes by engine before this
			// reader sees anything, so reaching it with another engine's
			// address means addressing broke. Forwarding it would file one
			// engine's event under another.
			name:    "another engine's address is dropped",
			profile: ollamaProxyProfile,
			method:  lmstudioProxyProfile.addressed("ready"),
			ok:      false,
		},
		{
			name:    "process-scoped method passes through",
			profile: ollamaProxyProfile,
			method:  "workload:started",
			want:    "workload:started",
			ok:      true,
		},
		{
			// The errors relay matches bare names, so an addressed error has to
			// come out as errors:report and not as ollama:errors:report.
			name:    "addressed error becomes a bare errors method",
			profile: ollamaProxyProfile,
			method:  ollamaProxyProfile.addressed(methodErrorsReport),
			want:    methodErrorsReport,
			ok:      true,
		},
		{
			name:    "unaddressed error is untouched",
			profile: lmstudioProxyProfile,
			method:  methodErrorsReport,
			want:    methodErrorsReport,
			ok:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := facadeMethodFor(tc.profile, tc.method)
			if ok != tc.ok {
				t.Fatalf("facadeMethodFor(%s, %q) ok = %v, want %v",
					tc.profile.Name, tc.method, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("facadeMethodFor(%s, %q) = %q, want %q",
					tc.profile.Name, tc.method, got, tc.want)
			}
		})
	}
}
