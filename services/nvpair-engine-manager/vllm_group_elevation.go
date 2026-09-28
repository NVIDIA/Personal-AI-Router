// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync"
)

// Credentials belong only to one live group action. They are never part of its
// plan, journal, status or process arguments. One Reconcile action may first
// close an exact retained predecessor on the same selected node before fencing
// the shown operation; both steps consume only that action's fresh node choice.
// A later Reconcile takes new input.
type vllmGroupElevation struct {
	mu      sync.Mutex
	entries map[string]diagnosticPackageElevation
}

type vllmGroupElevationContext struct{}

func nonInteractiveVLLMGroupElevation(plan vllmGroupPlan) []diagnosticPackageElevation {
	entries := make([]diagnosticPackageElevation, 0, len(plan.Members))
	for _, member := range plan.Members {
		entries = append(entries, diagnosticPackageElevation{NodeID: member.NodeID, NonInteractive: true})
	}
	return entries
}

func newVLLMGroupElevation(plan vllmGroupPlan, entries []diagnosticPackageElevation, requireAll bool) (*vllmGroupElevation, error) {
	targets := make([]diagnosticInspectionTarget, 0, len(plan.Members))
	for _, member := range plan.Members {
		targets = append(targets, diagnosticInspectionTarget{NodeID: member.NodeID})
	}
	passwords, consent, err := packageElevation(entries, targets)
	defer clear(passwords)
	if err != nil {
		return nil, err
	}
	if requireAll {
		for _, member := range plan.Members {
			if !consent[member.NodeID] {
				return nil, errors.New("choose administrator access for every reviewed participant in PAIR")
			}
		}
	}
	auth := &vllmGroupElevation{entries: make(map[string]diagnosticPackageElevation, len(entries))}
	for _, entry := range entries {
		if !consent[entry.NodeID] {
			return nil, errors.New("administrator access selection is incomplete")
		}
		auth.entries[entry.NodeID] = entry
	}
	return auth, nil
}

func (a *vllmGroupElevation) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.entries)
}

func (a *vllmGroupElevation) takeForNode(node string) *diagnosticPackageElevation {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[node]
	if !ok {
		return nil
	}
	delete(a.entries, node)
	return &entry
}

func (g *vllmServingGroup) elevationContext(ctx context.Context) context.Context {
	g.mu.Lock()
	auth := g.elevation
	g.mu.Unlock()
	return context.WithValue(ctx, vllmGroupElevationContext{}, auth)
}

func groupRequestElevation(ctx context.Context, node string) *diagnosticPackageElevation {
	auth, _ := ctx.Value(vllmGroupElevationContext{}).(*vllmGroupElevation)
	return auth.takeForNode(node)
}
