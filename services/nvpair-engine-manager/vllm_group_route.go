// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	vllmGroupRouteBudget   = 60 * time.Second
	vllmGroupRouteInterval = time.Second
	// One broker reply can wait behind an in-flight advertisement reconcile,
	// perform its own bounded status/health/backend reconcile, then query model
	// owners. Keep that legitimate path below this correlated-reply bound while
	// retaining the separate one-minute overall gate.
	vllmGroupRouteReply = 30 * time.Second
)

// The broker owns advertisement and the unified proxy owns routing. A group
// is published ready only after the owning broker reports that the local proxy
// lists this coordinator among the model's inference owners.
type vllmGroupRouteGate struct {
	mu      sync.Mutex
	waiters map[string]chan bool
}

func newVLLMGroupRouteGate() *vllmGroupRouteGate {
	return &vllmGroupRouteGate{waiters: map[string]chan bool{}}
}

func (m *Manager) awaitVLLMGroupRoute(ctx context.Context, run vllmGroupRun) error {
	ctx, cancel := context.WithTimeout(ctx, vllmGroupRouteBudget)
	defer cancel()
	ticker := time.NewTicker(vllmGroupRouteInterval)
	defer ticker.Stop()
	for {
		if routable, err := m.vllmGroupRouteCheck(ctx, run.Plan.Model); err == nil && routable {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("the local proxy did not route the ready serving group within its bound")
		case <-ticker.C:
		}
	}
}

func (m *Manager) vllmGroupRouteCheck(ctx context.Context, model string) (bool, error) {
	gate := m.groupRoute
	id := newOpID()
	reply := make(chan bool, 1)
	gate.mu.Lock()
	gate.waiters[id] = reply
	gate.mu.Unlock()
	defer func() {
		gate.mu.Lock()
		delete(gate.waiters, id)
		gate.mu.Unlock()
	}()
	if err := m.codec.Notify("engine:vllm-group-route-check", map[string]string{"requestId": id, "model": model}); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, vllmGroupRouteReply)
	defer cancel()
	select {
	case routable := <-reply:
		return routable, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (g *vllmGroupRouteGate) receive(raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Routable  *bool  `json:"routable"`
	}
	if strictDiagnosticJSON(raw, &p) != nil || p.Routable == nil {
		return
	}
	g.mu.Lock()
	reply := g.waiters[p.RequestID]
	g.mu.Unlock()
	if reply != nil {
		select {
		case reply <- *p.Routable:
		default:
		}
	}
}
