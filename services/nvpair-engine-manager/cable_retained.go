// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sort"

	"nvpair-shared/cableprobe"
)

func (s *cableProductService) retainedOwner() (string, string, error) {
	if s.m == nil || s.m.mesh == nil || s.m.cableLocal == nil {
		return "", "", errors.New("current cable controller identity is unavailable")
	}
	s.m.mesh.Refresh()
	node, principal := s.m.cableLocal.nodeID, s.m.mesh.NodeUUID()
	if !cableIdentifier(node, 128) || !cableIdentifier(principal, 256) {
		return "", "", errors.New("current cable controller identity is unavailable")
	}
	return node, principal, nil
}

func (s *cableProductService) retainedRunHeldLocked(r *cableProductRun) bool {
	return r.Public.State == "preparing" || r.Public.State == "running" || r.Public.State == "cancelling" ||
		(!r.Public.CleanupConfirmed && !s.cleanupReleasedLocked(r))
}

// Discovery uses only validated retained records, never a new selection, NIC
// observation, SSH connection or approval. Summaries omit plans and access data.
func (s *cableProductService) retainedRuns(ctx context.Context) (cableprobe.RetainedRuns, error) {
	if err := ctx.Err(); err != nil {
		return cableprobe.RetainedRuns{}, err
	}
	node, principal, err := s.retainedOwner()
	if err != nil {
		return cableprobe.RetainedRuns{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) > 128 {
		return cableprobe.RetainedRuns{}, errors.New("retained cable history exceeds its supported bound")
	}
	result := cableprobe.RetainedRuns{OwnerNodeID: node, Held: s.heldLocked() || s.shuttingDown, Limited: s.recoveryFailed || s.shuttingDown, Runs: []cableprobe.RetainedRun{}}
	for _, r := range s.runs {
		if !s.retainedRunHeldLocked(r) {
			continue
		}
		if r.OwnerPrincipal != principal || r.Public.OwnerNodeID != node {
			result.Limited = true
			continue
		}
		row := cableprobe.RetainedRun{RunID: r.Public.RunID, ReviewID: r.Public.ReviewID, OwnerNodeID: node, Revision: r.Public.Revision, State: r.Public.State, CleanupConfirmed: r.Public.CleanupConfirmed, StartedAt: r.Public.StartedAt, NodeIDs: []string{}, Ports: []cableprobe.PortRef{}}
		for _, target := range r.Public.Targets {
			row.NodeIDs = append(row.NodeIDs, target.NodeID)
			for _, port := range target.Ports {
				row.Ports = append(row.Ports, cableprobe.PortRef{NodeID: target.NodeID, SwitchID: port.SwitchID, PortName: port.PortName})
			}
		}
		result.Runs = append(result.Runs, row)
		result.Held = true
	}
	sort.Slice(result.Runs, func(i, j int) bool {
		if result.Runs[i].StartedAt != result.Runs[j].StartedAt {
			return result.Runs[i].StartedAt > result.Runs[j].StartedAt
		}
		return result.Runs[i].RunID < result.Runs[j].RunID
	})
	if result.Held && len(result.Runs) == 0 {
		result.Limited = true
	}
	if result.Limited {
		result.Reason = "Retained cable state still blocks another check. Some records belong to another controller, cannot be reconciled, or are still settling."
	}
	currentNode, currentPrincipal, err := s.retainedOwner()
	if err != nil || currentNode != node || currentPrincipal != principal {
		return cableprobe.RetainedRuns{}, errors.New("cable controller identity changed during retained-run discovery")
	}
	if err := ctx.Err(); err != nil {
		return cableprobe.RetainedRuns{}, err
	}
	return result, nil
}

func (m *Manager) runCableRetained(ctx context.Context, msg *Message) {
	var request struct{}
	if onboardingDecode(msg.Params, &request) != nil {
		m.codec.RespondError(msg.ID, -32602, "retained cable discovery accepts no selection or action parameters")
		return
	}
	result, err := m.cables.retainedRuns(ctx)
	m.respondOrErr(msg, result, err)
}
