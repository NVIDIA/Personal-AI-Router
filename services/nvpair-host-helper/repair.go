// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"nvpair-shared/hostbootstrap"
)

type ownedResourceMarker struct {
	IdentitySHA256 string
}

type ownedResourceSet map[hostbootstrap.ResourceKind]ownedResourceMarker

func markerOwned(markers ownedResourceSet, kind hostbootstrap.ResourceKind) bool {
	_, found := markers[kind]
	return found
}

var helperManagedResources = []hostbootstrap.ResourceKind{
	hostbootstrap.ResourceSSHService,
	hostbootstrap.ResourceFirewall,
	hostbootstrap.ResourceAuthorizedKey,
	hostbootstrap.ResourceHelper,
	hostbootstrap.ResourceProduct,
	hostbootstrap.ResourceActiveRole,
}

func deriveHelperRepairPlan(
	request hostbootstrap.Request,
	receipt hostbootstrap.Receipt,
	markers ownedResourceSet,
	observed hostbootstrap.Observations,
) (hostbootstrap.Plan, error) {
	if err := validateHelperRepairAuthorization(
		request,
		receipt,
		markers,
	); err != nil {
		return hostbootstrap.Plan{}, err
	}
	for _, kind := range helperManagedResources {
		if !receiptResourceOwned(receipt.Verified, kind) ||
			receiptResourceOwned(observed, kind) {
			continue
		}
		if kind == hostbootstrap.ResourceActiveRole &&
			markerOwned(markers, kind) &&
			len(observed.RuntimeOwners) == 0 &&
			len(receipt.Verified.RuntimeOwners) == 1 &&
			receipt.Verified.RuntimeOwners[0].Owner ==
				hostbootstrap.RoleHeadless &&
			request.Binding.RuntimeOwner == hostbootstrap.RoleHeadless {
			continue
		}
		return hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	plan, err := hostbootstrap.Reconcile(request, observed)
	if err != nil {
		return hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	if plan.Decision != hostbootstrap.DecisionNoOp &&
		plan.Decision != hostbootstrap.DecisionRepairOwned {
		return hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	for _, action := range plan.Actions {
		if !markerOwned(markers, action) {
			return hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
		}
	}
	return plan, nil
}

func validateHelperRepairAuthorization(
	request hostbootstrap.Request,
	receipt hostbootstrap.Receipt,
	markers ownedResourceSet,
) error {
	if err := request.Validate(); err != nil {
		return hostbootstrap.ErrHelperProtocol
	}
	if err := receipt.Validate(); err != nil ||
		receipt.OperationID != request.OperationID ||
		receipt.Binding != request.Binding {
		return hostbootstrap.ErrHelperProtocol
	}
	for _, kind := range helperManagedResources {
		wasOwned := receiptResourceOwned(receipt.Verified, kind)
		if markerOwned(markers, kind) != wasOwned {
			return hostbootstrap.ErrHelperProtocol
		}
	}
	if marker, found := markers[hostbootstrap.ResourceHelper]; found &&
		marker.IdentitySHA256 != request.Binding.Helper.SHA256 {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

func historicalMarkerAuthorized(
	request hostbootstrap.Request,
	receipt hostbootstrap.Receipt,
	kind hostbootstrap.ResourceKind,
	identitySHA256 string,
) bool {
	if receipt.Validate() != nil ||
		receipt.OperationID == request.OperationID ||
		receipt.Binding.Target != request.Binding.Target ||
		receipt.Binding.Account != request.Binding.Account ||
		identitySHA256 != helperResourceIdentitySHA256(
			kind,
			receipt.Binding,
		) {
		return false
	}
	return receiptResourceOwned(receipt.Verified, kind)
}

func helperResourceIdentitySHA256(
	kind hostbootstrap.ResourceKind,
	binding hostbootstrap.Binding,
) string {
	switch kind {
	case hostbootstrap.ResourceSSHService, hostbootstrap.ResourceFirewall:
		return helperJSONIdentitySHA256(binding.Endpoint)
	case hostbootstrap.ResourceAuthorizedKey:
		return binding.ControllerKey.FingerprintSHA256
	case hostbootstrap.ResourceHelper:
		return binding.Helper.SHA256
	case hostbootstrap.ResourceProduct:
		return binding.Product.SHA256
	case hostbootstrap.ResourceActiveRole:
		return helperRoleIdentitySHA256(binding.RuntimeOwner)
	default:
		return ""
	}
}

func helperJSONIdentitySHA256(value hostbootstrap.SSHEndpoint) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func helperRoleIdentitySHA256(value hostbootstrap.Role) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func receiptResourceOwned(
	observed hostbootstrap.Observations,
	kind hostbootstrap.ResourceKind,
) bool {
	switch kind {
	case hostbootstrap.ResourceSSHService:
		return observed.SSHService.Ownership == hostbootstrap.OwnershipOwned
	case hostbootstrap.ResourceFirewall:
		return observed.Firewall.Ownership == hostbootstrap.OwnershipOwned
	case hostbootstrap.ResourceAuthorizedKey:
		return observed.AuthorizedKey.Ownership == hostbootstrap.OwnershipOwned
	case hostbootstrap.ResourceHelper:
		return observed.Helper.Ownership == hostbootstrap.OwnershipOwned
	case hostbootstrap.ResourceProduct:
		return observed.Product.Ownership == hostbootstrap.OwnershipOwned
	case hostbootstrap.ResourceActiveRole:
		return len(observed.RuntimeOwners) == 1 &&
			observed.RuntimeOwners[0].Ownership == hostbootstrap.OwnershipOwned
	default:
		return false
	}
}
