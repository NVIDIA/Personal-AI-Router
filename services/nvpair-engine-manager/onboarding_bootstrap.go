// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"nvpair-shared/hostbootstrap"
)

type bootstrapTargetReference struct {
	CandidateID   string `json:"candidateId"`
	AccessID      string `json:"accessId"`
	HostKeySHA256 string `json:"hostKeySha256"`
}

type bootstrapRequestEnvelope struct {
	bootstrapTargetReference
	Request json.RawMessage `json:"request"`
}

type bootstrapPlanEnvelope struct {
	bootstrapTargetReference
	Plan json.RawMessage `json:"plan"`
}

type bootstrapOperationEnvelope struct {
	bootstrapTargetReference
	OperationID string `json:"operationId"`
}

func (s *onboardingService) bootstrapInspect(
	ctx context.Context,
	raw json.RawMessage,
) (hostbootstrap.Status, error) {
	envelope, request, err := decodeBootstrapRequest(raw)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	target, access, err := s.bootstrapTarget(
		envelope.bootstrapTargetReference,
	)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	if err := s.validateBootstrapBinding(
		request.Binding,
		target,
		access,
	); err != nil {
		return hostbootstrap.Status{}, err
	}
	response, err := s.callBootstrapHelper(
		ctx,
		target,
		access,
		request.Binding.Target.Platform,
		hostbootstrap.HelperRequest{
			SchemaVersion: hostbootstrap.SchemaVersion,
			OperationID:   request.OperationID,
			Action:        hostbootstrap.HelperActionInspect,
		},
	)
	if err != nil || response.Status == nil {
		if err == nil {
			err = errors.New("target helper returned no bootstrap status")
		}
		return hostbootstrap.Status{}, err
	}
	if response.Status.OperationID != request.OperationID ||
		response.Status.Binding != request.Binding ||
		response.Status.Phase != hostbootstrap.PhaseInspect {
		return hostbootstrap.Status{},
			errors.New("target helper status differs from the reviewed request")
	}
	s.rememberBootstrapTarget(
		envelope.CandidateID,
		envelope.AccessID,
		envelope.HostKeySHA256,
		request.Binding.Target,
	)
	return *response.Status, nil
}

func (s *onboardingService) bootstrapReview(
	ctx context.Context,
	raw json.RawMessage,
) (hostbootstrap.Plan, error) {
	envelope, request, err := decodeBootstrapRequest(raw)
	if err != nil {
		return hostbootstrap.Plan{}, err
	}
	status, err := s.bootstrapInspect(ctx, raw)
	if err != nil {
		return hostbootstrap.Plan{}, err
	}
	if status.OperationID != request.OperationID ||
		status.Binding != request.Binding ||
		envelope.CandidateID == "" {
		return hostbootstrap.Plan{},
			errors.New("target inspection changed during bootstrap review")
	}
	return hostbootstrap.Reconcile(request, status.Observed)
}

func (s *onboardingService) bootstrapApply(
	ctx context.Context,
	raw json.RawMessage,
) (hostbootstrap.Status, error) {
	envelope, plan, err := decodeBootstrapPlan(raw)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	target, access, err := s.bootstrapTarget(
		envelope.bootstrapTargetReference,
	)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	if err := s.validateBootstrapBinding(
		plan.Binding,
		target,
		access,
	); err != nil {
		return hostbootstrap.Status{}, err
	}
	response, err := s.callBootstrapHelper(
		ctx,
		target,
		access,
		plan.Binding.Target.Platform,
		hostbootstrap.HelperRequest{
			SchemaVersion: hostbootstrap.SchemaVersion,
			OperationID:   plan.OperationID,
			Action:        hostbootstrap.HelperActionApply,
		},
	)
	if err != nil || response.Status == nil {
		if err == nil {
			err = errors.New("target helper returned no apply status")
		}
		return hostbootstrap.Status{}, err
	}
	if response.Status.OperationID != plan.OperationID ||
		response.Status.Binding != plan.Binding ||
		response.Status.Decision != plan.Decision {
		return hostbootstrap.Status{},
			errors.New("target helper apply status differs from the reviewed plan")
	}
	s.rememberBootstrapTarget(
		envelope.CandidateID,
		envelope.AccessID,
		envelope.HostKeySHA256,
		plan.Binding.Target,
	)
	return *response.Status, nil
}

func (s *onboardingService) bootstrapStatus(
	ctx context.Context,
	raw json.RawMessage,
) (hostbootstrap.Status, error) {
	return s.bootstrapOperationStatus(
		ctx,
		raw,
		hostbootstrap.HelperActionInspect,
	)
}

func (s *onboardingService) bootstrapRecover(
	ctx context.Context,
	raw json.RawMessage,
) (hostbootstrap.Status, error) {
	return s.bootstrapOperationStatus(
		ctx,
		raw,
		hostbootstrap.HelperActionApply,
	)
}

func (s *onboardingService) bootstrapOperationStatus(
	ctx context.Context,
	raw json.RawMessage,
	action hostbootstrap.HelperAction,
) (hostbootstrap.Status, error) {
	var envelope bootstrapOperationEnvelope
	if err := onboardingDecode(raw, &envelope); err != nil ||
		!onboardingID.MatchString(envelope.OperationID) {
		return hostbootstrap.Status{},
			errors.New("invalid bootstrap operation request")
	}
	target, access, err := s.bootstrapTarget(
		envelope.bootstrapTargetReference,
	)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	platform := hostbootstrap.Platform(target.BootstrapPlatform)
	if platform != hostbootstrap.PlatformWindows &&
		platform != hostbootstrap.PlatformDarwin &&
		platform != hostbootstrap.PlatformLinux {
		return hostbootstrap.Status{},
			errors.New("bootstrap target platform is unavailable; inspect it again")
	}
	response, err := s.callBootstrapHelper(
		ctx,
		target,
		access,
		platform,
		hostbootstrap.HelperRequest{
			SchemaVersion: hostbootstrap.SchemaVersion,
			OperationID:   envelope.OperationID,
			Action:        action,
		},
	)
	if err != nil || response.Status == nil {
		if err == nil {
			err = errors.New("target helper returned no operation status")
		}
		return hostbootstrap.Status{}, err
	}
	if response.Status.OperationID != envelope.OperationID ||
		response.Status.Binding.Target.Platform != platform ||
		(target.BootstrapArchitecture != "" &&
			string(response.Status.Binding.Target.Architecture) !=
				target.BootstrapArchitecture) ||
		s.validateBootstrapBinding(
			response.Status.Binding,
			target,
			access,
		) != nil {
		return hostbootstrap.Status{},
			errors.New("target helper operation status is not access-bound")
	}
	s.rememberBootstrapTarget(
		envelope.CandidateID,
		envelope.AccessID,
		envelope.HostKeySHA256,
		response.Status.Binding.Target,
	)
	return *response.Status, nil
}

func (s *onboardingService) bootstrapVerify(
	ctx context.Context,
	raw json.RawMessage,
) (hostbootstrap.Receipt, error) {
	envelope, plan, err := decodeBootstrapPlan(raw)
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	target, access, err := s.bootstrapTarget(
		envelope.bootstrapTargetReference,
	)
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	if err := s.validateBootstrapBinding(
		plan.Binding,
		target,
		access,
	); err != nil {
		return hostbootstrap.Receipt{}, err
	}
	response, err := s.callBootstrapHelper(
		ctx,
		target,
		access,
		plan.Binding.Target.Platform,
		hostbootstrap.HelperRequest{
			SchemaVersion: hostbootstrap.SchemaVersion,
			OperationID:   plan.OperationID,
			Action:        hostbootstrap.HelperActionVerify,
		},
	)
	if err != nil || response.Receipt == nil {
		if err == nil {
			err = errors.New("target helper returned no bootstrap receipt")
		}
		return hostbootstrap.Receipt{}, err
	}
	if response.Receipt.OperationID != plan.OperationID ||
		response.Receipt.Binding != plan.Binding ||
		response.Receipt.Decision != plan.Decision {
		return hostbootstrap.Receipt{},
			errors.New("target helper receipt differs from the reviewed plan")
	}
	s.rememberBootstrapTarget(
		envelope.CandidateID,
		envelope.AccessID,
		envelope.HostKeySHA256,
		plan.Binding.Target,
	)
	return *response.Receipt, nil
}

func decodeBootstrapRequest(
	raw json.RawMessage,
) (
	bootstrapRequestEnvelope,
	hostbootstrap.Request,
	error,
) {
	var envelope bootstrapRequestEnvelope
	if err := onboardingDecode(raw, &envelope); err != nil {
		return envelope, hostbootstrap.Request{}, err
	}
	request, err := hostbootstrap.DecodeRequest(envelope.Request)
	if err != nil {
		return envelope, hostbootstrap.Request{},
			errors.New("invalid canonical bootstrap request")
	}
	return envelope, request, nil
}

func decodeBootstrapPlan(
	raw json.RawMessage,
) (
	bootstrapPlanEnvelope,
	hostbootstrap.Plan,
	error,
) {
	var envelope bootstrapPlanEnvelope
	if err := onboardingDecode(raw, &envelope); err != nil {
		return envelope, hostbootstrap.Plan{}, err
	}
	plan, err := hostbootstrap.DecodePlan(envelope.Plan)
	if err != nil {
		return envelope, hostbootstrap.Plan{},
			errors.New("invalid canonical bootstrap plan")
	}
	return envelope, plan, nil
}

func (s *onboardingService) bootstrapTarget(
	reference bootstrapTargetReference,
) (
	onboardingCandidate,
	onboardingAccess,
	error,
) {
	if !onboardingID.MatchString(reference.CandidateID) ||
		!onboardingID.MatchString(reference.AccessID) ||
		reference.HostKeySHA256 == "" {
		return onboardingCandidate{}, onboardingAccess{},
			errors.New("exact bootstrap candidate, access and host key are required")
	}
	s.expireAccess(time.Now())
	s.mu.Lock()
	target := s.targets[reference.CandidateID]
	if target == nil {
		s.mu.Unlock()
		return onboardingCandidate{}, onboardingAccess{},
			errors.New("bootstrap candidate is unavailable")
	}
	copy := *target
	s.mu.Unlock()
	if !copy.candidate.AccessAvailable ||
		copy.candidate.AccessID != reference.AccessID ||
		copy.candidate.HostKeySHA256 != reference.HostKeySHA256 ||
		copy.changedKey ||
		(!copy.expiresAt.IsZero() && time.Now().After(copy.expiresAt)) {
		return onboardingCandidate{}, onboardingAccess{},
			errors.New("bootstrap access or reviewed host identity changed")
	}
	copy.candidate.HostKeyTrusted = true
	return copy.candidate,
		copy.access.forPurpose("enrolled-peer-upgrade"),
		nil
}

func (s *onboardingService) validateBootstrapBinding(
	binding hostbootstrap.Binding,
	target onboardingCandidate,
	access onboardingAccess,
) error {
	request := hostbootstrap.Request{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   strings.Repeat("a", 32),
		Binding:       binding,
	}
	if err := request.Validate(); err != nil {
		return errors.New("invalid canonical bootstrap binding")
	}
	if binding.Account.Name != access.user ||
		binding.Endpoint.Address != target.Address ||
		binding.Endpoint.Port != target.Port {
		return errors.New("bootstrap request does not match reviewed target access")
	}
	if s.loadBootstrapCatalog == nil {
		return errors.New("bootstrap catalog loader is unavailable")
	}
	catalog, err := s.loadBootstrapCatalog()
	if err != nil {
		return err
	}
	return catalog.validateBinding(binding)
}

func (s *onboardingService) callBootstrapHelper(
	ctx context.Context,
	target onboardingCandidate,
	access onboardingAccess,
	platform hostbootstrap.Platform,
	request hostbootstrap.HelperRequest,
) (hostbootstrap.HelperResponse, error) {
	raw, err := hostbootstrap.EncodeHelperRequest(request)
	if err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	client, err := s.dial(ctx, target, access)
	if err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	defer client.close()
	output, err := client.runBootstrapHelper(
		ctx,
		platform,
		bytes.NewReader(raw),
	)
	if err != nil {
		return hostbootstrap.HelperResponse{}, err
	}
	response, err := hostbootstrap.DecodeHelperResponse(output)
	if err != nil {
		return hostbootstrap.HelperResponse{},
			errors.New("target helper returned an invalid bounded response")
	}
	if response.OperationID != request.OperationID ||
		response.Action != request.Action {
		return hostbootstrap.HelperResponse{},
			errors.New("target helper response correlation failed")
	}
	if !response.Accepted {
		return hostbootstrap.HelperResponse{}, errors.New(response.Reason)
	}
	return response, nil
}

func (s *onboardingService) rememberBootstrapTarget(
	candidateID string,
	accessID string,
	hostKeySHA256 string,
	target hostbootstrap.Target,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.targets[candidateID]
	if current == nil ||
		current.candidate.AccessID != accessID ||
		current.candidate.HostKeySHA256 != hostKeySHA256 {
		return
	}
	current.candidate.BootstrapPlatform = string(target.Platform)
	current.candidate.BootstrapArchitecture =
		string(target.Architecture)
	current.candidate.BootstrapState = "ssh-ready"
}
