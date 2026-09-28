// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

// Reconcile returns the single valid reviewed plan for a request and snapshot.
func Reconcile(request Request, observed Observations) (Plan, error) {
	if err := request.Validate(); err != nil {
		return Plan{}, err
	}
	if err := observed.Validate(request.Binding.Target); err != nil {
		return Plan{}, err
	}
	phase, decision, actions := derivePlan(request.Binding, observed)
	plan := Plan{
		SchemaVersion: SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         phase,
		Decision:      decision,
		Binding:       request.Binding,
		Observed:      observed,
		Actions:       actions,
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func derivePlan(binding Binding, observed Observations) (Phase, Decision, []ResourceKind) {
	if hasForeign(observed) {
		return PhaseReview, DecisionRefuseForeign, []ResourceKind{}
	}

	actions := make([]ResourceKind, 0, 6)
	if !endpointObservationExact(observed.SSHService, binding.Endpoint) {
		actions = append(actions, ResourceSSHService)
	}
	if !firewallObservationExact(binding, observed.Firewall) {
		actions = append(actions, ResourceFirewall)
	}
	if !authorizedKeyObservationExact(observed.AuthorizedKey, binding) {
		actions = append(actions, ResourceAuthorizedKey)
	}
	if !artifactObservationExact(observed.Helper, binding.Helper) {
		actions = append(actions, ResourceHelper)
	}
	if !artifactObservationExact(observed.Product, binding.Product) {
		actions = append(actions, ResourceProduct)
	}
	if !runtimeOwnerObservationExact(observed.RuntimeOwners, binding.RuntimeOwner) {
		actions = append(actions, ResourceActiveRole)
	}
	if len(actions) == 0 {
		return PhaseReview, DecisionNoOp, actions
	}
	if hasOwned(observed) {
		return PhaseReview, DecisionRepairOwned, actions
	}
	return PhaseReview, DecisionApply, actions
}

func hasForeign(observed Observations) bool {
	if observed.SSHService.Ownership == OwnershipForeign ||
		observed.Firewall.Ownership == OwnershipForeign ||
		observed.Firewall.Ownership == OwnershipUnavailable ||
		observed.AuthorizedKey.Ownership == OwnershipForeign ||
		observed.Helper.Ownership == OwnershipForeign ||
		observed.Product.Ownership == OwnershipForeign {
		return true
	}
	return len(observed.RuntimeOwners) == 1 && observed.RuntimeOwners[0].Ownership == OwnershipForeign
}

func hasOwned(observed Observations) bool {
	if observed.SSHService.Ownership == OwnershipOwned ||
		observed.Firewall.Ownership == OwnershipOwned ||
		observed.AuthorizedKey.Ownership == OwnershipOwned ||
		observed.Helper.Ownership == OwnershipOwned ||
		observed.Product.Ownership == OwnershipOwned {
		return true
	}
	return len(observed.RuntimeOwners) == 1 && observed.RuntimeOwners[0].Ownership == OwnershipOwned
}

func observationsExact(binding Binding, observed Observations) bool {
	return endpointObservationExact(observed.SSHService, binding.Endpoint) &&
		firewallObservationExact(binding, observed.Firewall) &&
		authorizedKeyObservationExact(observed.AuthorizedKey, binding) &&
		artifactObservationExact(observed.Helper, binding.Helper) &&
		artifactObservationExact(observed.Product, binding.Product) &&
		runtimeOwnerObservationExact(observed.RuntimeOwners, binding.RuntimeOwner)
}

func endpointObservationExact(observed EndpointObservation, desired SSHEndpoint) bool {
	return observed.Ownership == OwnershipOwned && observed.Endpoint != nil && *observed.Endpoint == desired
}

func firewallObservationExact(binding Binding, observed EndpointObservation) bool {
	switch binding.Target.Platform {
	case PlatformWindows:
		return endpointObservationExact(observed, binding.Endpoint)
	case PlatformDarwin:
		return observed.Ownership == OwnershipNotApplicable && observed.Endpoint == nil
	case PlatformLinux:
		return (observed.Ownership == OwnershipNotApplicable && observed.Endpoint == nil) ||
			endpointObservationExact(observed, binding.Endpoint)
	default:
		return false
	}
}

func authorizedKeyObservationExact(observed AuthorizedKeyObservation, binding Binding) bool {
	return observed.Ownership == OwnershipOwned &&
		observed.Identity != nil &&
		observed.Identity.AccountName == binding.Account.Name &&
		observed.Identity.Path == binding.Account.AuthorizedKeysPath &&
		observed.Identity.FingerprintSHA256 == binding.ControllerKey.FingerprintSHA256
}

func artifactObservationExact(observed ArtifactObservation, desired ArtifactIdentity) bool {
	return observed.Ownership == OwnershipOwned && observed.Identity != nil && *observed.Identity == desired
}

func runtimeOwnerObservationExact(observed []RuntimeOwnerObservation, desired Role) bool {
	return len(observed) == 1 && observed[0].Ownership == OwnershipOwned && observed[0].Owner == desired
}
