// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"slices"
	"sort"
)

const fabricLeaseOwnerServingGroup = "vllm-serving-group"

// A consumer lease binds an active fabric to the one serving-group generation
// whose ranks depend on it. Rollback stays refused until that generation
// releases it after every rank's cleanup is confirmed. The lease is part of the
// retained operation record, so it survives an Engine Manager restart.
type fabricConsumerLease struct {
	Owner      string `json:"owner"`
	RunID      string `json:"runId"`
	Generation uint64 `json:"generation"`
	PlanDigest string `json:"planDigest"`
}

var errNoFabric = errors.New("no retained fabric operation involves the selected members")

func validFabricConsumerLease(lease fabricConsumerLease) bool {
	return lease.Owner == fabricLeaseOwnerServingGroup && onboardingID.MatchString(lease.RunID) && lease.Generation > 0 && onboardingSHA.MatchString(lease.PlanDigest)
}

// directFabricFor returns the freshly requalified endpoints of the one active
// direct fabric between exactly these two members.
func (s *fabricService) directFabricFor(ctx context.Context, nodeIDs []string) (string, string, []fabricCandidateIP, error) {
	if len(nodeIDs) != 2 || nodeIDs[0] == nodeIDs[1] {
		return "", "", nil, errors.New("a direct fabric joins exactly two distinct members")
	}
	return s.exactFabricFor(ctx, nodeIDs, fabricRecipe, "roll it back or recover it, or choose pipeline parallelism explicitly")
}

// ringFabricFor returns the freshly requalified endpoints of the one active
// routed ring joining exactly these three members. The retained unrouted ring
// advertises no address every member reaches, so it fails closed.
func (s *fabricService) ringFabricFor(ctx context.Context, nodeIDs []string) (string, string, []fabricCandidateIP, error) {
	if len(nodeIDs) != 3 || nodeIDs[0] == nodeIDs[1] || nodeIDs[0] == nodeIDs[2] || nodeIDs[1] == nodeIDs[2] {
		return "", "", nil, errors.New("a ring fabric joins exactly three distinct members")
	}
	return s.exactFabricFor(ctx, nodeIDs, fabricRingRecipe, "roll it back or recover it before reviewing a group of these Sparks")
}

// exactFabricFor returns the freshly requalified endpoints of the one active
// recipeID fabric joining exactly these members. errNoFabric means no retained
// fabric operation or reservation involves any of them; any other error means
// one that does is stale or ambiguous, and callers fail closed.
func (s *fabricService) exactFabricFor(ctx context.Context, nodeIDs []string, recipeID, remedy string) (string, string, []fabricCandidateIP, error) {
	want := slices.Clone(nodeIDs)
	sort.Strings(want)
	s.mu.Lock()
	if s.recoveryFailed {
		s.mu.Unlock()
		return "", "", nil, errors.New("retained fabric history is incomplete; no fabric lane can be proven")
	}
	var involved []*fabricRunRecord
	for _, run := range s.runs {
		if run.Public.CleanupConfirmed || run.Public.State == "not-started" {
			continue
		}
		for _, target := range run.Public.Targets {
			if slices.Contains(want, target.NodeID) {
				involved = append(involved, run)
				break
			}
		}
	}
	reserved := s.reservation != nil && slices.Contains(want, s.reservation.Target.NodeID)
	exact, unrouted := false, false
	if len(involved) == 1 {
		run := involved[0]
		members := make([]string, 0, len(run.Public.Targets))
		for _, target := range run.Public.Targets {
			members = append(members, target.NodeID)
		}
		sort.Strings(members)
		active := run.Public.State == "active" && slices.Equal(members, want) &&
			(!reserved || s.reservation.OperationID == run.Public.OperationID)
		exact = active && run.Public.RecipeID == recipeID
		unrouted = active && recipeID == fabricRingRecipe && run.Public.RecipeID == fabricRingRetainedRecipe
	}
	s.mu.Unlock()
	if len(involved) == 0 && !reserved {
		return "", "", nil, errNoFabric
	}
	if unrouted {
		return "", "", nil, errors.New("the active ring predates routed advertised addresses and cannot carry serving traffic; roll it back and apply the routed ring")
	}
	if !exact {
		return "", "", nil, errors.New("a stale or ambiguous fabric operation involves the selected Sparks; " + remedy)
	}
	return s.requalifyFabric(ctx, nodeIDs)
}

// replaces names a prior generation the caller has proven terminal with every
// rank's cleanup confirmed; only its lease may be superseded.
func (s *fabricService) acquireConsumerLease(ctx context.Context, operationID, qualification string, lease fabricConsumerLease, replaces *fabricConsumerLease) error {
	if !validFabricConsumerLease(lease) {
		return errors.New("invalid fabric consumer lease")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		s.mu.Lock()
		if s.shuttingDown || s.recoveryFailed {
			s.mu.Unlock()
			return errors.New("fabric ownership is unavailable for a new consumer lease")
		}
		r := s.runs[operationID]
		if r == nil {
			s.mu.Unlock()
			return errors.New("the reviewed fabric is no longer active and qualified")
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
					return errors.New("the reviewed fabric did not settle before the lease deadline")
				}
			}
		}
		if r.Public.State != "active" || r.Public.QualificationDigest != qualification || s.qualified[operationID] != qualification {
			s.mu.Unlock()
			return errors.New("the reviewed fabric is no longer active and qualified")
		}
		if held := r.ConsumerLease; held != nil && *held != lease && (replaces == nil || *held != *replaces) {
			s.mu.Unlock()
			return errors.New("another serving group holds the reviewed fabric")
		}
		previous := r.ConsumerLease
		next := lease
		r.ConsumerLease = &next
		if err := s.save(r); err != nil {
			r.ConsumerLease = previous
			s.mu.Unlock()
			return errors.New("the fabric consumer lease could not be retained")
		}
		s.mu.Unlock()
		return nil
	}
}

func (s *fabricService) releaseConsumerLease(operationID string, lease fabricConsumerLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[operationID]
	if r == nil || r.ConsumerLease == nil || *r.ConsumerLease != lease {
		return nil
	}
	r.ConsumerLease = nil
	if err := s.save(r); err != nil {
		held := lease
		r.ConsumerLease = &held
		return errors.New("the fabric consumer lease release could not be retained")
	}
	return nil
}

func (s *fabricService) consumerLeaseHeld(operationID string, lease fabricConsumerLease) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[operationID]
	return r != nil && r.ConsumerLease != nil && *r.ConsumerLease == lease
}
