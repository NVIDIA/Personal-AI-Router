// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type onboardingSettledInvite struct {
	RequestKey string `json:"requestKey"`
	InviteID   string `json:"inviteId"`
	State      string `json:"state"`
}
type onboardingInvite struct {
	InviteID     string `json:"inviteId"`
	ClusterID    string `json:"clusterId"`
	FromNodeUUID string `json:"fromNodeUuid"`
	ToNodeID     string `json:"toNodeId"`
	Pin          string `json:"pin"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
}

func validOnboardingInvitationPlan(plan onboardingPlan) bool {
	if plan.InviteRequestKey != "" && (!onboardingID.MatchString(plan.InviteRequestKey) || plan.InviteNodeID == "") {
		return false
	}
	if len(plan.InviteHistory) > 8 {
		return false
	}
	seen := map[string]bool{}
	for _, old := range plan.InviteHistory {
		if !onboardingID.MatchString(old.RequestKey) || seen[old.RequestKey] || old.RequestKey == plan.InviteRequestKey || old.InviteID == "" || !onboardingInviteTerminal(old.State) {
			return false
		}
		seen[old.RequestKey] = true
	}
	return true
}

func onboardingInviteTerminal(state string) bool {
	switch state {
	case "canceled", "declined", "expired", "failed", "rejected":
		return true
	}
	return false
}
func onboardingUnknownInvite(err error) bool {
	var rpc *onboardingClusterError
	return errors.As(err, &rpc) && rpc.Code == -32001
}
func (s *onboardingService) invitationPlan(run *onboardingRun, id string) onboardingPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return run.Plans[id]
}
func (s *onboardingService) lookupOnboardingInvite(ctx context.Context, run *onboardingRun, id string) (onboardingInvite, error) {
	plan := s.invitationPlan(run, id)
	params := map[string]any{"requestKey": plan.InviteRequestKey}
	if plan.InviteRequestKey == "" {
		if plan.InviteID == "" {
			return onboardingInvite{}, errors.New("no invitation intent")
		}
		params = map[string]any{"inviteId": plan.InviteID}
	}
	raw, err := s.cluster(ctx, "cluster:invite-status", params)
	if err != nil {
		return onboardingInvite{}, err
	}
	defer clear(raw)
	var invite onboardingInvite
	if json.Unmarshal(raw, &invite) != nil || invite.InviteID == "" || invite.FromNodeUUID != run.ControllerNodeID || invite.ToNodeID != plan.InviteNodeID || (plan.InviteID != "" && plan.InviteID != invite.InviteID) {
		return invite, errors.New("invitation status does not match the durable selected-node intent")
	}
	if invite.State != "pending" && invite.State != "paired" && !onboardingInviteTerminal(invite.State) {
		return invite, errors.New("invitation status is unrecognized")
	}
	return invite, nil
}
func (s *onboardingService) recordOnboardingInvite(run *onboardingRun, id string, invite onboardingInvite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan := run.Plans[id]
	if invite.InviteID == "" || invite.FromNodeUUID != run.ControllerNodeID || invite.ToNodeID != plan.InviteNodeID || (plan.InviteID != "" && plan.InviteID != invite.InviteID) {
		return errors.New("invitation changed selected identities")
	}
	if invite.ClusterID != "" {
		if plan.InviteExpectedClusterID != "" && invite.ClusterID != plan.InviteExpectedClusterID {
			return errors.New("invitation is outside the reviewed cluster")
		}
		if run.Public.TargetClusterID != "" && run.Public.TargetClusterID != invite.ClusterID {
			return errors.New("invitation differs from the operation cluster")
		}
		run.Public.TargetClusterID = invite.ClusterID
	}
	plan.InviteID = invite.InviteID
	run.Plans[id] = plan
	if err := s.saveRun(run); err != nil {
		s.recoveryRequired = true
		return errors.New("invitation checkpoint was not retained; completion must not be sent")
	}
	return nil
}

// A retained key is resolved before enforcing the current cluster. Only its
// CM-owned exact association may explain the seed's own automatic founding.
func (s *onboardingService) reconcileOnboardingInvite(ctx context.Context, run *onboardingRun, id string) error {
	plan := s.invitationPlan(run, id)
	if plan.InviteRequestKey == "" && plan.InviteID == "" {
		return nil
	}
	invite, err := s.lookupOnboardingInvite(ctx, run, id)
	if onboardingUnknownInvite(err) {
		return nil
	} // Retry may submit only the SAME durable key, never a new one.
	if err != nil {
		return errors.New("retained invitation status is unavailable; no new effects are admitted")
	}
	defer func() { invite.Pin = "" }()
	if err = s.recordOnboardingInvite(run, id, invite); err != nil {
		return err
	}
	if onboardingInviteTerminal(invite.State) && plan.InviteExpectedClusterID == "" {
		identity, e := s.localIdentity(ctx)
		if e == nil && identity.NodeUUID == run.ControllerNodeID && identity.ClusterID == "" {
			s.mu.Lock()
			run.Public.TargetClusterID = ""
			err = s.saveRun(run)
			s.mu.Unlock()
			return err
		}
	}
	return nil
}
func (s *onboardingService) obtainOnboardingInvite(ctx context.Context, run *onboardingRun, id, nodeID string) (onboardingInvite, error) {
	plan := s.invitationPlan(run, id)
	retained := plan.InviteRequestKey != ""
	if retained {
		invite, err := s.lookupOnboardingInvite(ctx, run, id)
		if err == nil {
			if invite.State == "pending" || invite.State == "paired" {
				return s.waitOnboardingInvite(ctx, run, id)
			}
			if !onboardingInviteTerminal(invite.State) {
				return invite, errors.New("retained invitation is not terminal")
			}
			// This is an explicit retry, and the previous exact transaction is
			// authoritatively terminal before a new keyed attempt is retained.
			s.mu.Lock()
			current := run.Plans[id]
			if len(current.InviteHistory) >= 8 {
				s.mu.Unlock()
				return invite, errors.New("approved invitation retry limit reached")
			}
			current.InviteHistory = append(current.InviteHistory, onboardingSettledInvite{current.InviteRequestKey, invite.InviteID, invite.State})
			current.InviteRequestKey = ""
			current.InviteID = ""
			run.Plans[id] = current
			err = s.saveRun(run)
			s.mu.Unlock()
			if err != nil {
				return invite, errors.New("settled invitation checkpoint failed")
			}
			plan = current
		} else if !onboardingUnknownInvite(err) {
			return onboardingInvite{}, errors.New("prior invitation is unresolved; retry cannot create another transaction")
		}
	}
	if plan.InviteRequestKey == "" {
		s.mu.Lock()
		expected := run.Public.TargetClusterID
		s.mu.Unlock()
		identity, err := s.localIdentity(ctx)
		if err != nil || identity.NodeUUID != run.ControllerNodeID || identity.ClusterID != expected {
			return onboardingInvite{}, errors.New("controller cluster changed during installation; no invitation was issued")
		}
		s.mu.Lock()
		plan = run.Plans[id]
		plan.InviteRequestKey = newOpID()
		plan.InviteExpectedClusterID = expected
		plan.InviteNodeID = nodeID
		run.Plans[id] = plan
		err = s.saveRun(run)
		s.mu.Unlock()
		if err != nil {
			return onboardingInvite{}, errors.New("invitation intent could not be retained before send")
		}
	}
	if plan.InviteNodeID != nodeID {
		return onboardingInvite{}, errors.New("retained invitation targets a different native node")
	}
	// Original expectedClusterId is part of the idempotency tuple; a replay
	// after own automatic founding MUST still carry its original empty value.
	raw, callErr := s.cluster(ctx, "cluster:invite-node", map[string]any{"address": plan.Candidate.Address, "nodeId": nodeID, "expectedClusterId": plan.InviteExpectedClusterID, "requestKey": plan.InviteRequestKey})
	clear(raw) // The authoritative keyed status path below owns result parsing.
	invite, err := s.waitOnboardingInvite(ctx, run, id)
	if err != nil && callErr != nil {
		return invite, errors.New("invitation reply is uncertain; the durable request key was retained for exact status/retry")
	}
	return invite, err
}
func (s *onboardingService) waitOnboardingInvite(ctx context.Context, run *onboardingRun, id string) (onboardingInvite, error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		invite, err := s.lookupOnboardingInvite(ctx, run, id)
		if err != nil {
			return invite, err
		}
		if err = s.recordOnboardingInvite(run, id, invite); err != nil {
			return invite, err
		}
		if invite.State == "paired" {
			invite.Pin = ""
			return invite, nil
		}
		if onboardingInviteTerminal(invite.State) {
			invite.Pin = ""
			return invite, errors.New("invitation reached a non-paired terminal state; an explicit retry is required")
		}
		if invite.State == "pending" && len(invite.Pin) == 6 {
			return invite, nil
		}
		select {
		case <-ctx.Done():
			return onboardingInvite{}, errors.New("keyed invitation initial exchange did not become ready before deadline")
		case <-ticker.C:
		}
	}
}
func (s *onboardingService) cancelOnboardingInvitation(ctx context.Context, run *onboardingRun, id string) error {
	plan := s.invitationPlan(run, id)
	if !validOnboardingInvitationPlan(plan) {
		return errors.New("retained invitation history cannot confirm cleanup")
	}
	if plan.InviteRequestKey == "" && plan.InviteID == "" {
		return nil
	}
	invite, err := s.lookupOnboardingInvite(ctx, run, id)
	if err != nil {
		return errors.New("invitation cancellation is unconfirmed; exact retained status must be reconciled")
	}
	invite.Pin = ""
	if invite.State == "paired" {
		return errors.New("pairing committed; cancellation cannot erase membership")
	}
	if !onboardingInviteTerminal(invite.State) {
		raw, _ := s.cluster(ctx, "cluster:cancel-invite", map[string]any{"inviteId": invite.InviteID})
		clear(raw)
		invite, err = s.lookupOnboardingInvite(ctx, run, id)
		invite.Pin = ""
		if err != nil || !onboardingInviteTerminal(invite.State) {
			return errors.New("inviter transaction did not confirm terminal cancellation")
		}
	}
	return s.recordOnboardingInvite(run, id, invite)
}
