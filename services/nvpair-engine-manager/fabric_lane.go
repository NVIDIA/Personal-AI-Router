// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sort"
)

var (
	errNoFabricLane         = errors.New("no fabric links these members")
	errFabricTransferActive = errors.New("a model copy is using this fabric; let it finish or cancel it before rollback")
)

// A fabric has at most three members, and each member admits at most eight
// retained receive stages. Keeping the same ceiling also keeps journals below
// the shared 64 KiB strict-decode limit.
const maxFabricTransferHolds = 3 * vllmRetainedReceiveStageLimit

// fabricLane is one reciprocal link of an active, freshly qualified fabric.
type fabricLane struct {
	OperationID   string
	Qualification string
	SourceAddress string
	TargetAddress string
}

// laneBetween returns the requalified link a model copy from source to target
// should use. errNoFabricLane means no retained fabric operation links the
// pair; a linking operation that is not active or cannot be re-proven fails
// closed instead of silently moving the copy to the management network.
func (s *fabricService) laneBetween(ctx context.Context, source, target string) (fabricLane, error) {
	if source == target || !cableIdentifier(source, 128) || !cableIdentifier(target, 128) {
		return fabricLane{}, errors.New("a model copy joins two distinct members")
	}
	s.mu.Lock()
	if s.recoveryFailed {
		s.mu.Unlock()
		return fabricLane{}, errors.New("retained fabric history is incomplete; the fabric route cannot be proven")
	}
	var linking []*fabricRunRecord
	for _, run := range s.runs {
		if run.Public.CleanupConfirmed || run.Public.State == "not-started" {
			continue
		}
		members := map[string]bool{}
		for _, t := range run.Public.Targets {
			members[t.NodeID] = true
		}
		if members[source] && members[target] {
			linking = append(linking, run)
		}
	}
	s.mu.Unlock()
	if len(linking) == 0 {
		return fabricLane{}, errNoFabricLane
	}
	if len(linking) != 1 || linking[0].Public.State != "active" {
		return fabricLane{}, errors.New("the fabric linking these Sparks is not active; recover or roll it back before copying")
	}
	run := linking[0]
	if err := s.requalifyActive(ctx, run); err != nil {
		return fabricLane{}, errors.New("the fabric linking these Sparks could not be freshly re-proven; recover or roll it back before copying")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := run.Public.QualificationDigest
	if run.Public.State != "active" || digest == "" || s.qualified[run.Public.OperationID] != digest {
		return fabricLane{}, errors.New("the fabric linking these Sparks changed during requalification")
	}
	lanes := fabricLaneEndpoints(run.Public.CandidateIPs)
	var near []fabricCandidateIP
	for _, endpoint := range lanes {
		if endpoint.NodeID == source && endpoint.PeerNodeID == target {
			near = append(near, endpoint)
		}
	}
	sort.Slice(near, func(i, j int) bool { return near[i].InterfaceIndex < near[j].InterfaceIndex })
	for _, a := range near {
		for _, b := range lanes {
			if b.NodeID == target && b.PeerNodeID == source && b.Address == a.PeerAddress && b.PeerAddress == a.Address {
				return fabricLane{OperationID: run.Public.OperationID, Qualification: digest, SourceAddress: a.Address, TargetAddress: b.Address}, nil
			}
		}
	}
	return fabricLane{}, errors.New("the fabric linking these Sparks has no reciprocal lane between them")
}

// A transfer hold keeps a fabric from being rolled back until the exact target
// confirms that its copy has ended. An uncertain stream keeps its hold.
type fabricTransferHold struct {
	OperationID string `json:"operationId"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Model       string `json:"model"`
}

func validFabricTransferHold(holdID string, owner fabricTransferHold, members map[string]bool) bool {
	_, _, modelErr := parseExactVLLMHFModel(owner.Model)
	return vllmPullOperationToken.MatchString(holdID) && vllmPullOperationToken.MatchString(owner.OperationID) &&
		owner.Source != owner.Target && members[owner.Source] && members[owner.Target] && modelErr == nil
}

func (s *fabricService) acquireTransferHold(ctx context.Context, fabricOperationID, qualification, holdID string, owner fabricTransferHold) error {
	if !validFabricTransferHold(holdID, owner, map[string]bool{owner.Source: true, owner.Target: true}) {
		return errors.New("invalid fabric transfer hold")
	}
	for {
		s.mu.Lock()
		if s.shuttingDown || s.recoveryFailed {
			s.mu.Unlock()
			return errors.New("fabric ownership is unavailable for a new model copy")
		}
		r := s.runs[fabricOperationID]
		if r == nil {
			s.mu.Unlock()
			return errors.New("the fabric linking these Sparks is no longer active and qualified")
		}
		if r.done != nil {
			select {
			case <-r.done:
			default:
				done := r.done
				s.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return errors.New("the fabric linking these Sparks did not settle before the copy deadline")
				}
			}
		}
		if r.Public.State != "active" || r.Public.QualificationDigest != qualification || s.qualified[fabricOperationID] != qualification {
			s.mu.Unlock()
			return errors.New("the fabric linking these Sparks is no longer active and qualified")
		}
		members := map[string]bool{}
		for _, target := range r.Public.Targets {
			members[target.NodeID] = true
		}
		if !validFabricTransferHold(holdID, owner, members) || len(r.TransferHolds) >= maxFabricTransferHolds {
			s.mu.Unlock()
			return errors.New("invalid or exhausted fabric transfer hold")
		}
		if r.TransferHolds == nil {
			r.TransferHolds = map[string]fabricTransferHold{}
		}
		if _, exists := r.TransferHolds[holdID]; exists {
			s.mu.Unlock()
			return errors.New("this fabric transfer attempt already exists")
		}
		for _, held := range r.TransferHolds {
			if held == owner {
				s.mu.Unlock()
				return errors.New("this model copy already holds the fabric")
			}
		}
		r.TransferHolds[holdID] = owner
		if err := s.save(r); err != nil {
			delete(r.TransferHolds, holdID)
			s.mu.Unlock()
			return errors.New("fabric transfer hold could not be retained")
		}
		s.mu.Unlock()
		return nil
	}
}

func (s *fabricService) releaseTransferHold(fabricOperationID, holdID string, owner fabricTransferHold) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[fabricOperationID]
	if r == nil || r.TransferHolds[holdID] != owner {
		return nil
	}
	delete(r.TransferHolds, holdID)
	if err := s.save(r); err != nil {
		r.TransferHolds[holdID] = owner
		return errors.New("fabric transfer hold release could not be retained")
	}
	return nil
}

// An explicit cancel may arrive on a different RPC after the copy stream was
// lost. Bind its eventual release to the one attempt held when cancellation
// starts, so a delayed duplicate response cannot clear a successor retry.
func (s *fabricService) transferHoldRelease(owner fabricTransferHold) (func() error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matchedRun *fabricRunRecord
	var matchedID string
	for _, r := range s.runs {
		for holdID, held := range r.TransferHolds {
			if held != owner {
				continue
			}
			if matchedRun != nil {
				s.recoveryFailed = true
				return nil, errors.New("fabric transfer hold identity is ambiguous")
			}
			matchedRun, matchedID = r, holdID
		}
	}
	if matchedRun == nil {
		return func() error { return nil }, nil
	}
	fabricOperationID := matchedRun.Public.OperationID
	return func() error { return s.releaseTransferHold(fabricOperationID, matchedID, owner) }, nil
}
