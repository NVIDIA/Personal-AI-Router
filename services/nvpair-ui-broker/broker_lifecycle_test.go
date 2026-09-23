// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nvpair-shared/engines"
)

// TestClusterManagerConfigDirTracksBrokerClusterDir pins the invariant that keeps
// the cluster dir single-sourced: the broker hands its workers <base>/cluster and
// must hand nvpair-cluster-manager the same <base>. The manager is the only writer
// of that tree and the workers' only source of membership, so if the two resolved
// different bases the node would pair into a directory nothing reads — a healthy
// roster with no cluster traffic, and no restart left to mask it.
func TestClusterManagerConfigDirTracksBrokerClusterDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Personal AI Router")
	b := &Broker{clusterDir: filepath.Join(base, "cluster")}
	if got := b.clusterManagerConfigDir(); got != base {
		t.Fatalf("clusterManagerConfigDir = %q, want %q", got, base)
	}

	// With no cluster dir there is nothing to pass, and the manager falls back to
	// its own default — the one case where the two can still diverge, which the
	// broker reports at startup.
	if got := (&Broker{}).clusterManagerConfigDir(); got != "" {
		t.Fatalf("clusterManagerConfigDir with no cluster dir = %q, want empty", got)
	}
}

func TestEngineAvailabilityWaitsForBothProxyOutcomes(t *testing.T) {
	engineClient, engineServer := net.Pipe()
	defer engineClient.Close()
	defer engineServer.Close()
	engine := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}

	b := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	b.setEngineMgr(engine)

	restore := make(chan string, 1)
	go func() {
		msg, err := NewCodec(engineServer).Read()
		if err == nil {
			restore <- msg.Method
		}
	}()
	advertised := make(chan string, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- b.runEngineAvailabilityAfterPortGates(
			ctx,
			func(context.Context) { advertised <- "ollama" },
			func(context.Context) { advertised <- "lmstudio" },
		)
	}()

	select {
	case got := <-restore:
		t.Fatalf("restore %q ran before either proxy outcome", got)
	case got := <-advertised:
		t.Fatalf("%s advertising ran before either proxy outcome", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(b.ollamaPortReady)
	select {
	case got := <-restore:
		t.Fatalf("restore %q ran before LM Studio proxy outcome", got)
	case got := <-advertised:
		t.Fatalf("%s advertising ran before LM Studio proxy outcome", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(b.lmstudioPortReady)

	select {
	case got := <-restore:
		if got != restoreEnabledEnginesMethod {
			t.Fatalf("restore method = %q, want %q", got, restoreEnabledEnginesMethod)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enabled-engine restore did not run after both proxy outcomes")
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case got := <-advertised:
			seen[got] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("advertising did not start for both engines: %v", seen)
		}
	}
	if !<-done {
		t.Fatal("availability orchestration reported cancellation")
	}
}

type orderedLifecycleHandle struct {
	name  string
	order chan<- string
	done  chan struct{}
	once  sync.Once
}

func newOrderedLifecycleHandle(name string, order chan<- string) *orderedLifecycleHandle {
	return &orderedLifecycleHandle{name: name, order: order, done: make(chan struct{})}
}

func (h *orderedLifecycleHandle) Done() <-chan struct{} { return h.done }
func (h *orderedLifecycleHandle) Stop() {
	h.once.Do(func() {
		h.order <- h.name
		close(h.done)
	})
}

// Ingress stops before engines drain, so no new inference can arrive while
// engine-manager is stopping engines. One proxy process holds every engine's
// facade now, so there is one proxy stop rather than two — but it must still
// precede engine-manager.
func TestInferenceShutdownStopsTheProxyBeforeEngines(t *testing.T) {
	order := make(chan string, 2)
	proxy := newOrderedLifecycleHandle(engines.ProxyComponent, order)
	proxySup := newSupervisor(engines.ProxyComponent, noRestartPolicy(),
		func() (supervisedHandle, error) { return proxy, nil })
	if err := proxySup.Start(); err != nil {
		t.Fatal(err)
	}

	engineClient, engineServer := net.Pipe()
	defer engineClient.Close()
	defer engineServer.Close()
	engine := &rpcWorker{peer: NewPeer(NewCodec(engineClient))}
	go engine.peer.Serve(nil, nil)
	go func() {
		codec := NewCodec(engineServer)
		msg, err := codec.Read()
		if err != nil {
			return
		}
		order <- "engine-manager"
		_ = codec.Respond(msg.ID, nil)
	}()

	b := &Broker{proxySup: proxySup}
	b.setEngineMgr(engine)
	b.shutdownInferenceStack()

	got := []string{<-order, <-order}
	if got[0] != engines.ProxyComponent || got[1] != "engine-manager" {
		t.Fatalf("shutdown order = %v, want [%s engine-manager]", got, engines.ProxyComponent)
	}
}

// A terminally failed proxy releases every engine's ownership gate. Shared fate
// is the accepted cost of one process, so a caller waiting on LM Studio's gate
// must not be left waiting because the failure was reported for the process.
func TestTerminalProxyFailureOpensEveryEnginesGate(t *testing.T) {
	spawned := make(chan *fakeHandle, 1)
	sup := newSupervisor(engines.ProxyComponent, noRestartPolicy(), func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	})
	b := &Broker{
		ollamaPortReady:   make(chan struct{}),
		lmstudioPortReady: make(chan struct{}),
	}
	b.ollamaState().managedFacade.Store(true)
	b.lmstudioState().managedFacade.Store(true)
	b.configureProxySupervisorCallbacks(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()

	mustSpawn(t, spawned).crash()
	for name, gate := range map[string]<-chan struct{}{
		ollamaProxyProfile.Name:   b.ollamaPortReady,
		lmstudioProxyProfile.Name: b.lmstudioPortReady,
	} {
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
			t.Fatalf("terminal proxy failure left %s ownership pending", name)
		}
	}
	if b.ollamaState().managedFacade.Load() || b.lmstudioState().managedFacade.Load() {
		t.Fatal("terminal proxy failure left managed ownership enabled")
	}
}

// An unbindable LM Studio facade is given up on by itself: its ownership gate
// is released so callers stop waiting, without restarting the process and
// without clearing the shared proxy handle.
//
// This used to restart the process, which was fine when a process hosted one
// engine. It is not fine now: the same process hosts Ollama's facade, so a
// restart to fix LM Studio's port would drop Ollama's listener and the
// inference in flight on it. The handle assertion is the load-bearing one —
// clearing it would strand every other facade's traffic.
func TestUnbindableLMStudioFacadeFinishesWithoutRestartingTheProcess(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	b := &Broker{clusterDir: "cluster", lmstudioPortReady: make(chan struct{})}
	b.lmstudioState().backendPort.Store(managedLMStudioBackendStart)
	b.lmstudioProxyGeneration.Store(1)
	b.lmstudioProxyPublishedGeneration.Store(1)
	b.setLMStudioProxy(proxy)
	// Both engines resolve to this one process, which is what the assertions
	// below rely on: Ollama's handle has to be observable to show it survived.
	b.setProxy(proxy)
	b.proxySup = newSupervisor(engines.ProxyComponent, noRestartPolicy(), nil)

	// Answer every set-port with a port other than the one asked for, which is
	// how the child reports it could not take it.
	go func() {
		codec := NewCodec(proxyServer)
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			_ = codec.Respond(msg.ID, proxyReadyParams{Port: 1})
		}
	}()

	b.lmstudioReadyMu.Lock()
	port, ok := b.rebindLMStudioFacadeOrFinish(proxy, 1, managedLMStudioFacadePort)
	b.lmstudioReadyMu.Unlock()

	if ok || port != 0 {
		t.Fatalf("rebind reported success (port %d) though every set-port was refused", port)
	}
	// The gate has to open, or every engine:status for LM Studio waits out the
	// call timeout and answers "retry" for the life of the process.
	select {
	case <-b.lmstudioPortReady:
	case <-time.After(2 * time.Second):
		t.Fatal("an unbindable facade left its ownership gate closed")
	}
	if b.lmstudioState().managedFacade.Load() {
		t.Error("managed LM Studio mode survived an unbindable facade")
	}
	// The process is untouched: the same handle is still published for BOTH
	// engines, so Ollama's facade in that same process keeps serving. Handle
	// identity is the whole property — a process restart would replace it,
	// which is what would take Ollama's listener down to fix LM Studio's port.
	//
	// The Ollama handle is published in the setup precisely so this can fail:
	// asserting on a slot that was never populated would pass no matter what.
	if b.getLMStudioProxy() != proxy {
		t.Error("a facade port failure replaced the shared proxy handle")
	}
	if b.getProxy() != proxy {
		t.Error("a facade port failure disturbed the other engine's handle on the same process")
	}
}
