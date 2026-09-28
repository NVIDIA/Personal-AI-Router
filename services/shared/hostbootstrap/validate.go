// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"reflect"
	"regexp"
)

var operationIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// SupportedTargets returns the six accepted target combinations in stable order.
func SupportedTargets() []Target {
	return []Target{
		{Platform: PlatformWindows, Architecture: ArchitectureAMD64},
		{Platform: PlatformWindows, Architecture: ArchitectureARM64},
		{Platform: PlatformDarwin, Architecture: ArchitectureAMD64},
		{Platform: PlatformDarwin, Architecture: ArchitectureARM64},
		{Platform: PlatformLinux, Architecture: ArchitectureAMD64},
		{Platform: PlatformLinux, Architecture: ArchitectureARM64},
	}
}

// Validate checks the complete request contract without side effects.
func (request Request) Validate() error {
	if request.SchemaVersion != SchemaVersion || !operationIDPattern.MatchString(request.OperationID) {
		return ErrInvalid
	}
	return request.Binding.validate()
}

func (binding Binding) validate() error {
	if err := binding.Target.validate(); err != nil {
		return err
	}
	switch binding.Lane {
	case LaneQuickConnect, LaneZeroTouch:
	default:
		return ErrInvalid
	}
	switch binding.Role {
	case RoleAuto, RoleDesktop, RoleHeadless:
	default:
		return ErrInvalid
	}
	if binding.RuntimeOwner != RoleDesktop && binding.RuntimeOwner != RoleHeadless {
		return ErrInvalid
	}
	if binding.Role != RoleAuto && binding.Role != binding.RuntimeOwner {
		return ErrInvalid
	}
	if err := binding.Account.validate(binding.Target.Platform); err != nil {
		return err
	}
	if err := binding.ControllerKey.validate(); err != nil {
		return err
	}
	if err := binding.Endpoint.validate(); err != nil {
		return err
	}
	if err := binding.Product.validate(binding.Target.Platform); err != nil {
		return err
	}
	if err := binding.Helper.validate(binding.Target.Platform); err != nil {
		return err
	}
	if binding.Product.ID == binding.Helper.ID || binding.Product.Path == binding.Helper.Path {
		return ErrInvalid
	}
	return nil
}

func (target Target) validate() error {
	switch target.Platform {
	case PlatformWindows, PlatformDarwin, PlatformLinux:
	default:
		return ErrInvalid
	}
	switch target.Architecture {
	case ArchitectureAMD64, ArchitectureARM64:
	default:
		return ErrInvalid
	}
	return nil
}

// Validate checks a complete ownership snapshot for the declared target.
func (observed Observations) Validate(target Target) error {
	if err := target.validate(); err != nil {
		return err
	}
	if observed.RuntimeOwners == nil {
		return ErrInvalid
	}
	if err := observed.SSHService.validateManaged(); err != nil {
		return err
	}
	if err := observed.Firewall.validateFirewall(target.Platform); err != nil {
		return err
	}
	if err := observed.AuthorizedKey.validate(target.Platform); err != nil {
		return err
	}
	if err := observed.Product.validate(target.Platform); err != nil {
		return err
	}
	if err := observed.Helper.validate(target.Platform); err != nil {
		return err
	}
	if len(observed.RuntimeOwners) > 1 {
		return ErrInvalid
	}
	if len(observed.RuntimeOwners) == 1 {
		owner := observed.RuntimeOwners[0]
		if owner.Ownership != OwnershipOwned && owner.Ownership != OwnershipForeign {
			return ErrInvalid
		}
		if owner.Owner != RoleDesktop && owner.Owner != RoleHeadless {
			return ErrInvalid
		}
	}
	if observed.Product.Identity != nil && observed.Helper.Identity != nil {
		if observed.Product.Identity.ID == observed.Helper.Identity.ID || observed.Product.Identity.Path == observed.Helper.Identity.Path {
			return ErrInvalid
		}
	}
	return nil
}

func (observed EndpointObservation) validate() error {
	switch observed.Ownership {
	case OwnershipAbsent:
		if observed.Endpoint != nil {
			return ErrInvalid
		}
		return nil
	case OwnershipOwned, OwnershipForeign:
		if observed.Endpoint == nil {
			return ErrInvalid
		}
		return observed.Endpoint.validate()
	case OwnershipNotApplicable, OwnershipUnavailable:
		if observed.Endpoint != nil {
			return ErrInvalid
		}
		return nil
	default:
		return ErrInvalid
	}
}

func (observed EndpointObservation) validateManaged() error {
	if observed.Ownership == OwnershipNotApplicable || observed.Ownership == OwnershipUnavailable {
		return ErrInvalid
	}
	return observed.validate()
}

func (observed EndpointObservation) validateFirewall(platform Platform) error {
	if err := observed.validate(); err != nil {
		return err
	}
	switch platform {
	case PlatformWindows:
		if observed.Ownership == OwnershipNotApplicable {
			return ErrInvalid
		}
	case PlatformDarwin:
		if observed.Ownership != OwnershipNotApplicable {
			return ErrInvalid
		}
	case PlatformLinux:
	default:
		return ErrInvalid
	}
	return nil
}

func (observed AuthorizedKeyObservation) validate(platform Platform) error {
	switch observed.Ownership {
	case OwnershipAbsent:
		if observed.Identity != nil {
			return ErrInvalid
		}
		return nil
	case OwnershipOwned, OwnershipForeign:
		if observed.Identity == nil {
			return ErrInvalid
		}
		return observed.Identity.validate(platform)
	default:
		return ErrInvalid
	}
}

func (identity AuthorizedKeyIdentity) validate(platform Platform) error {
	if len(identity.AccountName) > maxAccountNameBytes {
		return ErrTooLarge
	}
	if !accountNamePattern.MatchString(identity.AccountName) || identity.AccountName == "." || identity.AccountName == ".." {
		return ErrInvalid
	}
	if err := validateCanonicalPath(platform, identity.Path); err != nil {
		return err
	}
	if !sha256Pattern.MatchString(identity.FingerprintSHA256) {
		return ErrInvalid
	}
	return nil
}

func (observed ArtifactObservation) validate(platform Platform) error {
	switch observed.Ownership {
	case OwnershipAbsent:
		if observed.Identity != nil {
			return ErrInvalid
		}
		return nil
	case OwnershipOwned, OwnershipForeign:
		if observed.Identity == nil {
			return ErrInvalid
		}
		return observed.Identity.validate(platform)
	default:
		return ErrInvalid
	}
}

// Validate checks that a plan is the deterministic result of its embedded data.
func (plan Plan) Validate() error {
	if plan.SchemaVersion != SchemaVersion || !operationIDPattern.MatchString(plan.OperationID) {
		return ErrInvalid
	}
	if err := plan.Binding.validate(); err != nil {
		return err
	}
	if err := plan.Observed.Validate(plan.Binding.Target); err != nil {
		return err
	}
	phase, decision, actions := derivePlan(plan.Binding, plan.Observed)
	if plan.Phase != phase || plan.Decision != decision || !reflect.DeepEqual(plan.Actions, actions) {
		return ErrInvalid
	}
	return nil
}

// Validate checks that a receipt contains the exact desired verified state.
func (receipt Receipt) Validate() error {
	if receipt.SchemaVersion != SchemaVersion || !operationIDPattern.MatchString(receipt.OperationID) || receipt.Phase != PhaseComplete {
		return ErrInvalid
	}
	if receipt.Decision != DecisionApply && receipt.Decision != DecisionRepairOwned && receipt.Decision != DecisionNoOp {
		return ErrInvalid
	}
	if err := receipt.Binding.validate(); err != nil {
		return err
	}
	if err := receipt.Verified.Validate(receipt.Binding.Target); err != nil {
		return err
	}
	if !observationsExact(receipt.Binding, receipt.Verified) {
		return ErrInvalid
	}
	return nil
}

// Validate checks one internally consistent operation status.
func (status Status) Validate() error {
	if status.SchemaVersion != SchemaVersion || !operationIDPattern.MatchString(status.OperationID) {
		return ErrInvalid
	}
	if err := status.Binding.validate(); err != nil {
		return err
	}
	if err := status.Observed.Validate(status.Binding.Target); err != nil {
		return err
	}
	derivedPhase, derivedDecision, _ := derivePlan(status.Binding, status.Observed)
	switch status.Phase {
	case PhaseInspect:
		if status.Decision != "" {
			return ErrInvalid
		}
	case PhaseReview:
		if derivedPhase != PhaseReview || status.Decision != derivedDecision {
			return ErrInvalid
		}
	case PhaseApply:
		if derivedPhase != PhaseReview ||
			(status.Decision != DecisionApply && status.Decision != DecisionRepairOwned) ||
			status.Decision != derivedDecision {
			return ErrInvalid
		}
	case PhaseVerify, PhaseComplete:
		if status.Decision != DecisionApply && status.Decision != DecisionRepairOwned && status.Decision != DecisionNoOp {
			return ErrInvalid
		}
		if !observationsExact(status.Binding, status.Observed) {
			return ErrInvalid
		}
	case PhaseBlocked:
		if status.Decision != DecisionRefuseForeign || !hasForeign(status.Observed) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
