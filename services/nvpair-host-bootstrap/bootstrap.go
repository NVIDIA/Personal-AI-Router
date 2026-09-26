// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"nvpair-shared/hostbootstrap"
)

var (
	ErrForeignCollision     = errors.New("a fixed bootstrap resource has a foreign owner")
	ErrNotPrivileged        = errors.New("native administrator or root authority is required")
	ErrPlanChanged          = errors.New("native state changed after review")
	ErrReceiptImmutable     = errors.New("bootstrap receipt is immutable")
	ErrRerunSignedBootstrap = errors.New("repair requires rerunning the signed bootstrap")
	ErrRuntimeOwnerActive   = errors.New("the fixed Desktop broker tree is still running")
	ErrStateIdentity        = errors.New("bootstrap state identity changed")
	ErrStateMissing         = errors.New("bootstrap state is missing")
	ErrUnsafeState          = errors.New("bootstrap state ownership or file safety is invalid")
	ErrUnsupportedIdentity  = errors.New("bootstrap artifact identity is not the fixed native identity")
	ErrUnsupportedTarget    = errors.New("bootstrap target is unsupported")
	ErrVerification         = errors.New("bootstrap verification did not reach exact desired state")
)

type platformAdapter interface {
	Target(context.Context) (hostbootstrap.Target, string, error)
	Privileged() (bool, error)
	DesktopRunning(context.Context, hostbootstrap.Request) (bool, error)
	Inspect(context.Context, hostbootstrap.Request) (hostbootstrap.Observations, error)
	ValidatePending(context.Context, pendingEffect, hostbootstrap.Request, hostbootstrap.Observations) error
	RecoverPending(context.Context, pendingEffect, hostbootstrap.Request) (bool, error)
	ValidateRepair(context.Context, hostbootstrap.Plan) error
	ValidateRemoval(context.Context, hostbootstrap.ResourceKind, hostbootstrap.Request) (bool, error)
	ValidateUninstallPending(context.Context, uninstallEffect, hostbootstrap.Request, hostbootstrap.Observations) error
	CapturePendingMarker(hostbootstrap.ResourceKind) (resourceMarker, bool, error)
	ApplyResource(context.Context, hostbootstrap.ResourceKind, hostbootstrap.Request) error
	RemoveResource(context.Context, hostbootstrap.ResourceKind, hostbootstrap.Request) error
}

type stateRepository interface {
	SaveOperation(hostbootstrap.Request, hostbootstrap.Plan) error
	SaveRepairOperation(hostbootstrap.Request, hostbootstrap.Plan) error
	LoadOperation() (hostbootstrap.Request, hostbootstrap.Plan, error)
	SaveReceipt(hostbootstrap.Receipt) error
	LoadReceipt() (hostbootstrap.Receipt, error)
	SavePending(pendingEffect) error
	UpdatePending(pendingEffect, pendingEffect) error
	AdoptPending(pendingEffect, pendingEffect) error
	LoadPending() (pendingEffect, error)
	ClearPending() error
	SaveUninstallPending(uninstallEffect) error
	UpdateUninstallPending(uninstallEffect, uninstallEffect) error
	LoadUninstallPending() (uninstallEffect, error)
	ClearUninstallPending() error
	SaveUninstalled(hostbootstrap.Receipt, hostbootstrap.Observations) error
	RemoveOperation() error
	RunMutation(func() error) error
}

type effectPoint string

const (
	effectBefore effectPoint = "before"
	effectAfter  effectPoint = "after"
)

type pendingEffect struct {
	SchemaVersion    int                        `json:"schemaVersion"`
	OperationID      string                     `json:"operationId"`
	PlanSHA256       string                     `json:"planSha256"`
	Resource         hostbootstrap.ResourceKind `json:"resource"`
	ActionIndex      int                        `json:"actionIndex"`
	ExpectedPre      hostbootstrap.Observations `json:"expectedPre"`
	ExpectedPost     hostbootstrap.Observations `json:"expectedPost"`
	EffectID         string                     `json:"effectId"`
	EffectComplete   bool                       `json:"effectComplete"`
	Revision         uint64                     `json:"revision"`
	PriorMarker      resourceMarker             `json:"priorMarker"`
	PriorMarkerFound bool                       `json:"priorMarkerFound"`
}

type uninstallEffect struct {
	SchemaVersion  int                        `json:"schemaVersion"`
	OperationID    string                     `json:"operationId"`
	Resource       hostbootstrap.ResourceKind `json:"resource"`
	ActionIndex    int                        `json:"actionIndex"`
	ExpectedPre    hostbootstrap.Observations `json:"expectedPre"`
	ExpectedPost   hostbootstrap.Observations `json:"expectedPost"`
	EffectID       string                     `json:"effectId"`
	EffectComplete bool                       `json:"effectComplete"`
	Revision       uint64                     `json:"revision"`
	PriorMarker    resourceMarker             `json:"priorMarker"`
}

type Bootstrap struct {
	platform   platformAdapter
	state      stateRepository
	effectHook func(effectPoint, hostbootstrap.ResourceKind) error
}

func NewBootstrap(platform platformAdapter, state stateRepository) *Bootstrap {
	return &Bootstrap{platform: platform, state: state}
}

func (bootstrap *Bootstrap) inspect(
	ctx context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.Observations, error) {
	if err := request.Validate(); err != nil {
		return hostbootstrap.Observations{}, err
	}
	if err := validateNativeBinding(request.Binding); err != nil {
		return hostbootstrap.Observations{}, err
	}
	actual, distro, err := bootstrap.platform.Target(ctx)
	if err != nil {
		return hostbootstrap.Observations{}, err
	}
	if actual != request.Binding.Target {
		return hostbootstrap.Observations{}, ErrUnsupportedTarget
	}
	if actual.Platform == hostbootstrap.PlatformLinux && distro != "debian" && distro != "ubuntu" {
		return hostbootstrap.Observations{}, ErrUnsupportedTarget
	}
	observed, err := bootstrap.platform.Inspect(ctx, request)
	if err != nil {
		return hostbootstrap.Observations{}, err
	}
	if err := observed.Validate(request.Binding.Target); err != nil {
		return hostbootstrap.Observations{}, err
	}
	return observed, nil
}

func (bootstrap *Bootstrap) Inspect(
	ctx context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.Status, error) {
	observed, err := bootstrap.inspect(ctx, request)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	status := hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         hostbootstrap.PhaseInspect,
		Binding:       request.Binding,
		Observed:      observed,
	}
	if err := status.Validate(); err != nil {
		return hostbootstrap.Status{}, err
	}
	return status, nil
}

// Review is intentionally side-effect free. The reviewed plan becomes trusted
// state only after Apply has freshly inspected and reproduced it exactly.
func (bootstrap *Bootstrap) Review(
	ctx context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.Plan, error) {
	observed, err := bootstrap.inspect(ctx, request)
	if err != nil {
		return hostbootstrap.Plan{}, err
	}
	return hostbootstrap.Reconcile(request, observed)
}

func (bootstrap *Bootstrap) Apply(
	ctx context.Context,
	reviewed hostbootstrap.Plan,
) (hostbootstrap.Status, error) {
	var status hostbootstrap.Status
	err := bootstrap.state.RunMutation(func() error {
		var err error
		status, err = bootstrap.applyUnlocked(ctx, reviewed)
		return err
	})
	return status, err
}

func (bootstrap *Bootstrap) applyUnlocked(
	ctx context.Context,
	reviewed hostbootstrap.Plan,
) (hostbootstrap.Status, error) {
	if err := reviewed.Validate(); err != nil {
		return hostbootstrap.Status{}, err
	}
	if reviewed.Decision == hostbootstrap.DecisionRefuseForeign {
		return hostbootstrap.Status{}, ErrForeignCollision
	}
	privileged, err := bootstrap.platform.Privileged()
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	if !privileged {
		return hostbootstrap.Status{}, privilegeError(reviewed.Binding.Target.Platform)
	}
	request := hostbootstrap.Request{
		SchemaVersion: reviewed.SchemaVersion,
		OperationID:   reviewed.OperationID,
		Binding:       reviewed.Binding,
	}
	planSHA256, err := digestPlan(reviewed)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	startIndex := 0
	pending, pendingErr := bootstrap.state.LoadPending()
	if pendingErr == nil {
		if pending.SchemaVersion != hostbootstrap.SchemaVersion ||
			pending.OperationID != reviewed.OperationID ||
			pending.PlanSHA256 != planSHA256 ||
			pending.ActionIndex < 0 ||
			pending.ActionIndex >= len(reviewed.Actions) ||
			pending.Resource != reviewed.Actions[pending.ActionIndex] {
			return hostbootstrap.Status{}, ErrPlanChanged
		}
		storedRequest, storedPlan, err := bootstrap.state.LoadOperation()
		if err != nil ||
			storedRequest != request ||
			!reflect.DeepEqual(storedPlan, reviewed) {
			return hostbootstrap.Status{}, ErrPlanChanged
		}
		expectedPre := reviewed.Observed
		for index := 0; index < pending.ActionIndex; index++ {
			expectedPre = observationsAfterAction(
				expectedPre,
				request,
				reviewed.Actions[index],
			)
		}
		expectedPost := observationsAfterAction(
			expectedPre,
			request,
			pending.Resource,
		)
		if !reflect.DeepEqual(pending.ExpectedPre, expectedPre) ||
			!reflect.DeepEqual(pending.ExpectedPost, expectedPost) {
			return hostbootstrap.Status{}, ErrPlanChanged
		}
	} else if !errors.Is(pendingErr, ErrStateMissing) {
		return hostbootstrap.Status{}, pendingErr
	}
	fresh, err := bootstrap.inspect(ctx, request)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	if pendingErr == nil {
		masked := maskPendingResource(
			fresh,
			pending.ExpectedPre,
			pending.Resource,
		)
		if pending.Resource == hostbootstrap.ResourceHelper &&
			helperHeadlessTransitionEffect(pending.EffectID) {
			masked.RuntimeOwners = append(
				[]hostbootstrap.RuntimeOwnerObservation(nil),
				pending.ExpectedPre.RuntimeOwners...,
			)
		}
		if !reflect.DeepEqual(masked, pending.ExpectedPre) {
			return hostbootstrap.Status{}, ErrPlanChanged
		}
		if err := bootstrap.platform.ValidatePending(
			ctx,
			pending,
			request,
			fresh,
		); err != nil {
			return hostbootstrap.Status{}, ErrPlanChanged
		}
		if _, err := bootstrap.platform.RecoverPending(ctx, pending, request); err != nil {
			return hostbootstrap.Status{}, err
		}
		fresh, err = bootstrap.inspect(ctx, request)
		if err != nil {
			return hostbootstrap.Status{}, err
		}
	}
	switch {
	case pendingErr == nil:
		switch {
		case reflect.DeepEqual(fresh, pending.ExpectedPost):
			startIndex = pending.ActionIndex + 1
			if err := bootstrap.state.ClearPending(); err != nil {
				return hostbootstrap.Status{}, err
			}
		case reflect.DeepEqual(fresh, pending.ExpectedPre):
			startIndex = pending.ActionIndex
		default:
			return hostbootstrap.Status{}, ErrPlanChanged
		}
	}
	recomputed, err := hostbootstrap.Reconcile(request, fresh)
	if err != nil {
		return hostbootstrap.Status{}, err
	}
	if pendingErr != nil {
		if !reflect.DeepEqual(recomputed, reviewed) {
			return hostbootstrap.Status{}, ErrPlanChanged
		}
	} else if recomputed.Decision == hostbootstrap.DecisionRefuseForeign ||
		!reflect.DeepEqual(recomputed.Actions, reviewed.Actions[startIndex:]) {
		return hostbootstrap.Status{}, ErrPlanChanged
	}
	if pendingErr != nil &&
		reviewed.Decision == hostbootstrap.DecisionRepairOwned {
		if err := bootstrap.platform.ValidateRepair(ctx, reviewed); err != nil {
			return hostbootstrap.Status{}, err
		}
	}
	if err := bootstrap.state.SaveOperation(request, reviewed); err != nil {
		return hostbootstrap.Status{}, err
	}
	if reviewed.Decision == hostbootstrap.DecisionNoOp {
		return hostbootstrap.Status{
			SchemaVersion: hostbootstrap.SchemaVersion,
			OperationID:   reviewed.OperationID,
			Phase:         hostbootstrap.PhaseVerify,
			Decision:      reviewed.Decision,
			Binding:       reviewed.Binding,
			Observed:      fresh,
		}, nil
	}
	for index := startIndex; index < len(reviewed.Actions); index++ {
		action := reviewed.Actions[index]
		before, err := bootstrap.inspect(ctx, request)
		if err != nil {
			return hostbootstrap.Status{}, err
		}
		pending := pendingEffect{
			SchemaVersion: hostbootstrap.SchemaVersion,
			OperationID:   reviewed.OperationID,
			PlanSHA256:    planSHA256,
			Resource:      action,
			ActionIndex:   index,
			ExpectedPre:   before,
			ExpectedPost:  observationsAfterAction(before, request, action),
		}
		priorMarker, priorMarkerFound, err :=
			bootstrap.platform.CapturePendingMarker(action)
		if err != nil {
			return hostbootstrap.Status{}, err
		}
		pending.PriorMarker = priorMarker
		pending.PriorMarkerFound = priorMarkerFound
		if err := bootstrap.state.SavePending(pending); err != nil {
			return hostbootstrap.Status{}, err
		}
		if bootstrap.effectHook != nil {
			if err := bootstrap.effectHook(effectBefore, action); err != nil {
				return hostbootstrap.Status{}, err
			}
		}
		if err := bootstrap.platform.ApplyResource(ctx, action, request); err != nil {
			return hostbootstrap.Status{}, fmt.Errorf("apply %s: %w", action, err)
		}
		if bootstrap.effectHook != nil {
			if err := bootstrap.effectHook(effectAfter, action); err != nil {
				return hostbootstrap.Status{}, err
			}
		}
		after, err := bootstrap.inspect(ctx, request)
		if err != nil {
			return hostbootstrap.Status{}, err
		}
		if !reflect.DeepEqual(after, pending.ExpectedPost) {
			return hostbootstrap.Status{}, ErrVerification
		}
		if err := bootstrap.state.ClearPending(); err != nil {
			return hostbootstrap.Status{}, err
		}
	}
	status := hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   reviewed.OperationID,
		Phase:         hostbootstrap.PhaseApply,
		Decision:      reviewed.Decision,
		Binding:       reviewed.Binding,
		Observed:      reviewed.Observed,
	}
	if err := status.Validate(); err != nil {
		return hostbootstrap.Status{}, err
	}
	return status, nil
}

func helperHeadlessTransitionEffect(effectID string) bool {
	return effectID == "helper:stop-headless-wrapper" ||
		effectID == "helper:stop-prior" ||
		effectID == "helper:install-executable" ||
		effectID == "helper:save-principal" ||
		effectID == "helper:ensure-product-parent" ||
		strings.HasPrefix(effectID, "helper:definition:") ||
		strings.HasPrefix(effectID, "apply:helper:command:") ||
		effectID == "helper:start-headless-wrapper"
}

func (bootstrap *Bootstrap) Repair(
	ctx context.Context,
	reviewed hostbootstrap.Plan,
) (hostbootstrap.Status, error) {
	var status hostbootstrap.Status
	err := bootstrap.state.RunMutation(func() error {
		var err error
		status, err = bootstrap.repairUnlocked(ctx, reviewed)
		return err
	})
	return status, err
}

func (bootstrap *Bootstrap) repairUnlocked(
	ctx context.Context,
	reviewed hostbootstrap.Plan,
) (hostbootstrap.Status, error) {
	if err := reviewed.Validate(); err != nil {
		return hostbootstrap.Status{}, err
	}
	if reviewed.Decision != hostbootstrap.DecisionRepairOwned &&
		reviewed.Decision != hostbootstrap.DecisionNoOp {
		return hostbootstrap.Status{}, ErrRerunSignedBootstrap
	}
	if reviewed.Decision == hostbootstrap.DecisionRepairOwned {
		if err := bootstrap.platform.ValidateRepair(ctx, reviewed); err != nil {
			return hostbootstrap.Status{}, err
		}
	}
	request := hostbootstrap.Request{
		SchemaVersion: reviewed.SchemaVersion,
		OperationID:   reviewed.OperationID,
		Binding:       reviewed.Binding,
	}
	if err := bootstrap.state.SaveRepairOperation(request, reviewed); err != nil {
		return hostbootstrap.Status{}, err
	}
	return bootstrap.applyUnlocked(ctx, reviewed)
}

func digestPlan(plan hostbootstrap.Plan) (string, error) {
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func observationsAfterAction(
	before hostbootstrap.Observations,
	request hostbootstrap.Request,
	action hostbootstrap.ResourceKind,
) hostbootstrap.Observations {
	after := before
	after.RuntimeOwners = make(
		[]hostbootstrap.RuntimeOwnerObservation,
		len(before.RuntimeOwners),
	)
	copy(after.RuntimeOwners, before.RuntimeOwners)
	endpoint := request.Binding.Endpoint
	switch action {
	case hostbootstrap.ResourceSSHService:
		after.SSHService = hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		}
	case hostbootstrap.ResourceFirewall:
		after.Firewall = hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		}
	case hostbootstrap.ResourceAuthorizedKey:
		after.AuthorizedKey = hostbootstrap.AuthorizedKeyObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity: &hostbootstrap.AuthorizedKeyIdentity{
				AccountName:       request.Binding.Account.Name,
				Path:              request.Binding.Account.AuthorizedKeysPath,
				FingerprintSHA256: request.Binding.ControllerKey.FingerprintSHA256,
			},
		}
	case hostbootstrap.ResourceHelper:
		helper := request.Binding.Helper
		after.Helper = hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &helper,
		}
	case hostbootstrap.ResourceProduct:
		product := request.Binding.Product
		after.Product = hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &product,
		}
	case hostbootstrap.ResourceActiveRole:
		after.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{{
			Ownership: hostbootstrap.OwnershipOwned,
			Owner:     request.Binding.RuntimeOwner,
		}}
	}
	return after
}

func maskPendingResource(
	current hostbootstrap.Observations,
	expected hostbootstrap.Observations,
	resource hostbootstrap.ResourceKind,
) hostbootstrap.Observations {
	switch resource {
	case hostbootstrap.ResourceSSHService:
		current.SSHService = expected.SSHService
	case hostbootstrap.ResourceFirewall:
		current.Firewall = expected.Firewall
	case hostbootstrap.ResourceAuthorizedKey:
		current.AuthorizedKey = expected.AuthorizedKey
	case hostbootstrap.ResourceHelper:
		current.Helper = expected.Helper
	case hostbootstrap.ResourceProduct:
		current.Product = expected.Product
	case hostbootstrap.ResourceActiveRole:
		current.RuntimeOwners = make(
			[]hostbootstrap.RuntimeOwnerObservation,
			len(expected.RuntimeOwners),
		)
		copy(current.RuntimeOwners, expected.RuntimeOwners)
	}
	return current
}

func (bootstrap *Bootstrap) Verify(
	ctx context.Context,
	reviewed hostbootstrap.Plan,
) (hostbootstrap.Receipt, error) {
	var receipt hostbootstrap.Receipt
	err := bootstrap.state.RunMutation(func() error {
		var err error
		receipt, err = bootstrap.verifyUnlocked(ctx, reviewed)
		return err
	})
	return receipt, err
}

func (bootstrap *Bootstrap) verifyUnlocked(
	ctx context.Context,
	reviewed hostbootstrap.Plan,
) (hostbootstrap.Receipt, error) {
	if err := reviewed.Validate(); err != nil {
		return hostbootstrap.Receipt{}, err
	}
	privileged, err := bootstrap.platform.Privileged()
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	if !privileged {
		return hostbootstrap.Receipt{}, privilegeError(reviewed.Binding.Target.Platform)
	}
	storedRequest, storedPlan, err := bootstrap.state.LoadOperation()
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	if !reflect.DeepEqual(storedPlan, reviewed) {
		return hostbootstrap.Receipt{}, ErrStateIdentity
	}
	fresh, err := bootstrap.inspect(ctx, storedRequest)
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	receipt, err := hostbootstrap.NewReceipt(reviewed, fresh)
	if err != nil {
		return hostbootstrap.Receipt{}, ErrVerification
	}
	existing, existingErr := bootstrap.state.LoadReceipt()
	if existingErr == nil {
		if existing.OperationID != reviewed.OperationID ||
			existing.Binding != reviewed.Binding {
			return hostbootstrap.Receipt{}, ErrReceiptImmutable
		}
		return existing, nil
	}
	if !errors.Is(existingErr, ErrStateMissing) {
		return hostbootstrap.Receipt{}, existingErr
	}
	if err := bootstrap.state.SaveReceipt(receipt); err != nil {
		return hostbootstrap.Receipt{}, err
	}
	return receipt, nil
}

func (bootstrap *Bootstrap) ReadReceipt() (hostbootstrap.Receipt, error) {
	return bootstrap.state.LoadReceipt()
}

func (bootstrap *Bootstrap) Uninstall(
	ctx context.Context,
	request hostbootstrap.Request,
) error {
	return bootstrap.state.RunMutation(func() error {
		return bootstrap.uninstallUnlocked(ctx, request)
	})
}

func (bootstrap *Bootstrap) uninstallUnlocked(
	ctx context.Context,
	request hostbootstrap.Request,
) error {
	if err := request.Validate(); err != nil {
		return err
	}
	privileged, err := bootstrap.platform.Privileged()
	if err != nil {
		return err
	}
	if !privileged {
		return privilegeError(request.Binding.Target.Platform)
	}
	if _, err := bootstrap.state.LoadPending(); err == nil {
		return ErrStateIdentity
	} else if !errors.Is(err, ErrStateMissing) {
		return err
	}
	uninstallPending, uninstallErr := bootstrap.state.LoadUninstallPending()
	if uninstallErr != nil && !errors.Is(uninstallErr, ErrStateMissing) {
		return uninstallErr
	}
	storedRequest, _, err := bootstrap.state.LoadOperation()
	if err != nil {
		return err
	}
	if storedRequest != request {
		return ErrStateIdentity
	}
	receipt, err := bootstrap.state.LoadReceipt()
	if err != nil ||
		receipt.OperationID != request.OperationID ||
		receipt.Binding != request.Binding {
		return ErrStateIdentity
	}
	observed, err := bootstrap.inspect(ctx, request)
	if err != nil {
		return err
	}
	if uninstallErr == nil {
		if err := bootstrap.platform.ValidateUninstallPending(
			ctx,
			uninstallPending,
			request,
			observed,
		); err != nil {
			return err
		}
	}
	if errors.Is(uninstallErr, ErrStateMissing) {
		for _, kind := range uninstallOrder {
			if _, err := bootstrap.platform.ValidateRemoval(
				ctx,
				kind,
				request,
			); err != nil {
				return fmt.Errorf("validate removal %s: %w", kind, err)
			}
		}
	}
	for index, kind := range uninstallOrder {
		observed, err = bootstrap.inspect(ctx, request)
		if err != nil {
			return err
		}
		if uninstallErr == nil {
			if uninstallPending.ActionIndex != index ||
				uninstallPending.Resource != kind {
				if index < uninstallPending.ActionIndex {
					continue
				}
				return ErrStateIdentity
			}
		} else {
			owned, validateErr := bootstrap.platform.ValidateRemoval(
				ctx,
				kind,
				request,
			)
			if validateErr != nil {
				return fmt.Errorf(
					"validate removal %s: %w",
					kind,
					validateErr,
				)
			}
			if !owned {
				continue
			}
			marker, found, markerErr :=
				bootstrap.platform.CapturePendingMarker(kind)
			if markerErr != nil || !found {
				return ErrStateIdentity
			}
			uninstallPending = uninstallEffect{
				SchemaVersion: hostbootstrap.SchemaVersion,
				OperationID:   request.OperationID,
				Resource:      kind,
				ActionIndex:   index,
				ExpectedPre:   observed,
				ExpectedPost: observationsAfterRemoval(
					observed,
					kind,
				),
				Revision:    1,
				PriorMarker: marker,
			}
			if err := bootstrap.state.SaveUninstallPending(
				uninstallPending,
			); err != nil {
				return err
			}
			uninstallErr = nil
		}
		if err := bootstrap.platform.RemoveResource(ctx, kind, request); err != nil {
			return fmt.Errorf("remove %s: %w", kind, err)
		}
		observed, err = bootstrap.platform.Inspect(ctx, request)
		if err != nil {
			return err
		}
		if !resourceRemoved(observed, kind) {
			return fmt.Errorf("%w: %s remains present", ErrVerification, kind)
		}
		if err := bootstrap.state.ClearUninstallPending(); err != nil {
			return err
		}
		uninstallErr = ErrStateMissing
		uninstallPending = uninstallEffect{}
	}
	finalObserved, err := bootstrap.platform.Inspect(ctx, request)
	if err != nil {
		return err
	}
	if err := bootstrap.state.SaveUninstalled(
		receipt,
		finalObserved,
	); err != nil {
		return err
	}
	return bootstrap.state.RemoveOperation()
}

func observationsAfterRemoval(
	observed hostbootstrap.Observations,
	kind hostbootstrap.ResourceKind,
) hostbootstrap.Observations {
	after := observed
	after.RuntimeOwners = append(
		[]hostbootstrap.RuntimeOwnerObservation(nil),
		observed.RuntimeOwners...,
	)
	switch kind {
	case hostbootstrap.ResourceSSHService:
		after.SSHService = hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		}
	case hostbootstrap.ResourceFirewall:
		if observed.Firewall.Ownership !=
			hostbootstrap.OwnershipNotApplicable {
			after.Firewall = hostbootstrap.EndpointObservation{
				Ownership: hostbootstrap.OwnershipAbsent,
			}
		}
	case hostbootstrap.ResourceAuthorizedKey:
		after.AuthorizedKey = hostbootstrap.AuthorizedKeyObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		}
	case hostbootstrap.ResourceHelper:
		after.Helper = hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		}
	case hostbootstrap.ResourceProduct:
		after.Product = hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipAbsent,
		}
	case hostbootstrap.ResourceActiveRole:
		after.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{}
	}
	return after
}

func resourceObservationExact(
	observed hostbootstrap.Observations,
	binding hostbootstrap.Binding,
	kind hostbootstrap.ResourceKind,
) bool {
	switch kind {
	case hostbootstrap.ResourceSSHService:
		return observed.SSHService.Ownership == hostbootstrap.OwnershipOwned &&
			observed.SSHService.Endpoint != nil &&
			*observed.SSHService.Endpoint == binding.Endpoint
	case hostbootstrap.ResourceFirewall:
		if binding.Target.Platform == hostbootstrap.PlatformDarwin {
			return observed.Firewall.Ownership ==
				hostbootstrap.OwnershipNotApplicable &&
				observed.Firewall.Endpoint == nil
		}
		return observed.Firewall.Ownership == hostbootstrap.OwnershipOwned &&
			observed.Firewall.Endpoint != nil &&
			*observed.Firewall.Endpoint == binding.Endpoint
	case hostbootstrap.ResourceAuthorizedKey:
		return observed.AuthorizedKey.Ownership == hostbootstrap.OwnershipOwned &&
			observed.AuthorizedKey.Identity != nil &&
			observed.AuthorizedKey.Identity.AccountName == binding.Account.Name &&
			observed.AuthorizedKey.Identity.Path ==
				binding.Account.AuthorizedKeysPath &&
			observed.AuthorizedKey.Identity.FingerprintSHA256 ==
				binding.ControllerKey.FingerprintSHA256
	case hostbootstrap.ResourceHelper:
		return observed.Helper.Ownership == hostbootstrap.OwnershipOwned &&
			observed.Helper.Identity != nil &&
			*observed.Helper.Identity == binding.Helper
	case hostbootstrap.ResourceProduct:
		return observed.Product.Ownership == hostbootstrap.OwnershipOwned &&
			observed.Product.Identity != nil &&
			*observed.Product.Identity == binding.Product
	case hostbootstrap.ResourceActiveRole:
		return len(observed.RuntimeOwners) == 1 &&
			observed.RuntimeOwners[0].Ownership == hostbootstrap.OwnershipOwned &&
			observed.RuntimeOwners[0].Owner == binding.RuntimeOwner
	default:
		return false
	}
}

var uninstallOrder = []hostbootstrap.ResourceKind{
	hostbootstrap.ResourceActiveRole,
	hostbootstrap.ResourceProduct,
	hostbootstrap.ResourceHelper,
	hostbootstrap.ResourceAuthorizedKey,
	hostbootstrap.ResourceFirewall,
	hostbootstrap.ResourceSSHService,
}

func resourceOwned(observed hostbootstrap.Observations, kind hostbootstrap.ResourceKind) bool {
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

func resourceRemoved(
	observed hostbootstrap.Observations,
	kind hostbootstrap.ResourceKind,
) bool {
	switch kind {
	case hostbootstrap.ResourceSSHService:
		return observed.SSHService.Ownership == hostbootstrap.OwnershipAbsent
	case hostbootstrap.ResourceFirewall:
		return observed.Firewall.Ownership == hostbootstrap.OwnershipAbsent ||
			observed.Firewall.Ownership == hostbootstrap.OwnershipNotApplicable
	case hostbootstrap.ResourceAuthorizedKey:
		return observed.AuthorizedKey.Ownership == hostbootstrap.OwnershipAbsent
	case hostbootstrap.ResourceHelper:
		return observed.Helper.Ownership == hostbootstrap.OwnershipAbsent
	case hostbootstrap.ResourceProduct:
		return observed.Product.Ownership == hostbootstrap.OwnershipAbsent
	case hostbootstrap.ResourceActiveRole:
		return len(observed.RuntimeOwners) == 0
	default:
		return false
	}
}

func privilegeError(platform hostbootstrap.Platform) error {
	switch platform {
	case hostbootstrap.PlatformWindows:
		return fmt.Errorf("%w: rerun the signed package from an Administrator deployment context", ErrNotPrivileged)
	case hostbootstrap.PlatformDarwin:
		return fmt.Errorf("%w: rerun the signed package through its native macOS authorization flow", ErrNotPrivileged)
	case hostbootstrap.PlatformLinux:
		return fmt.Errorf("%w: rerun the signed package from an already-root deployment context", ErrNotPrivileged)
	default:
		return ErrNotPrivileged
	}
}
