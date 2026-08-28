// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

func TestInferenceShutdownStopsProxiesBeforeEngines(t *testing.T) {
	order := make(chan string, 3)
	ollama := newOrderedLifecycleHandle("ollama-proxy", order)
	lmstudio := newOrderedLifecycleHandle("lmstudio-proxy", order)
	proxySup := newSupervisor("proxy", noRestartPolicy(), func() (supervisedHandle, error) { return ollama, nil })
	lmstudioSup := newSupervisor("lmstudio-proxy", noRestartPolicy(), func() (supervisedHandle, error) { return lmstudio, nil })
	if err := proxySup.Start(); err != nil {
		t.Fatal(err)
	}
	if err := lmstudioSup.Start(); err != nil {
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

	b := &Broker{proxySup: proxySup, lmstudioProxySup: lmstudioSup}
	b.setEngineMgr(engine)
	b.shutdownInferenceStack()

	got := []string{<-order, <-order, <-order}
	if got[0] != "lmstudio-proxy" || got[1] != "ollama-proxy" || got[2] != "engine-manager" {
		t.Fatalf("shutdown order = %v, want [lmstudio-proxy ollama-proxy engine-manager]", got)
	}
}

func TestLMStudioSupervisorTerminalFailureOpensGate(t *testing.T) {
	spawned := make(chan *fakeHandle, 1)
	sup := newSupervisor("lmstudio-proxy", noRestartPolicy(), func() (supervisedHandle, error) {
		h := newFakeHandle()
		spawned <- h
		return h, nil
	})
	b := &Broker{lmstudioPortReady: make(chan struct{})}
	b.managedLMStudioFacade.Store(true)
	b.configureLMStudioProxySupervisorCallbacks(sup)
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	defer sup.Stop()

	mustSpawn(t, spawned).crash()
	select {
	case <-b.lmstudioPortReady:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal LM Studio proxy failure left ownership pending")
	}
	if b.managedLMStudioFacade.Load() {
		t.Fatal("terminal LM Studio proxy failure left managed ownership enabled")
	}
}

// TestLMStudioProxyRestartInvalidatesPendingGeneration: a requested LM Studio
// proxy restart must invalidate the current logical generation and handle before
// the restart is observable, so a ready notification already queued by the
// outgoing process cannot open the port-ownership gate — and the replacement
// generation's ready still does.
func TestLMStudioProxyRestartInvalidatesPendingGeneration(t *testing.T) {
	proxyClient, proxyServer := net.Pipe()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyServer.Close()
	})
	proxy := &proxyProcess{peer: NewPeer(NewCodec(proxyClient))}
	go proxy.peer.Serve(nil, nil)

	b := &Broker{clusterDir: "cluster", lmstudioPortReady: make(chan struct{})}
	b.lmstudioBackendPort.Store(managedLMStudioBackendStart)
	b.lmstudioProxyGeneration.Store(1)
	b.lmstudioProxyPublishedGeneration.Store(1)
	b.setLMStudioProxy(proxy)
	b.lmstudioProxySup = newSupervisor("lmstudio-proxy", noRestartPolicy(), nil)

	go func() {
		codec := NewCodec(proxyServer)
		msg, err := codec.Read()
		if err == nil {
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()

	b.lmstudioReadyMu.Lock()
	b.restartLMStudioProxyOrFinish(1)
	b.lmstudioReadyMu.Unlock()
	select {
	case <-b.lmstudioProxySup.restartCh:
	case <-time.After(2 * time.Second):
		t.Fatal("restart request did not reach the LM Studio proxy supervisor")
	}
	b.forwardLMStudioProxyNotificationForGeneration(1, "ready", json.RawMessage(`{"port":1236}`))
	select {
	case <-b.lmstudioPortReady:
		t.Fatal("old-generation ready opened gate after restart")
	case <-time.After(100 * time.Millisecond):
	}
	if b.lmstudioProxyGeneration.Load() == 1 || b.getLMStudioProxy() != nil {
		t.Fatal("restart did not invalidate LM Studio generation and handle")
	}

	replacementClient, replacementServer := net.Pipe()
	t.Cleanup(func() {
		_ = replacementClient.Close()
		_ = replacementServer.Close()
	})
	replacement := &proxyProcess{peer: NewPeer(NewCodec(replacementClient))}
	go replacement.peer.Serve(nil, nil)
	replacementGeneration := b.lmstudioProxyGeneration.Add(1)
	b.setLMStudioProxy(replacement)
	b.lmstudioProxyPublishedGeneration.Store(replacementGeneration)
	go func() {
		codec := NewCodec(replacementServer)
		msg, err := codec.Read()
		if err == nil {
			_ = codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
	}()
	b.forwardLMStudioProxyNotificationForGeneration(replacementGeneration, "ready", json.RawMessage(`{"port":1236}`))
	select {
	case <-b.lmstudioPortReady:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement generation did not open gate after the restart")
	}
}
