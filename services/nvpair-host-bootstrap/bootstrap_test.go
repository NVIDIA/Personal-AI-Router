// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"nvpair-shared/hostbootstrap"
)

func testPublicKey(seed byte) hostbootstrap.PublicKeyIdentity {
	algorithm := []byte(hostbootstrap.PublicKeyAlgorithmED25519)
	blob := make([]byte, 4+len(algorithm)+4+32)
	binary.BigEndian.PutUint32(blob[:4], uint32(len(algorithm)))
	copy(blob[4:], algorithm)
	offset := 4 + len(algorithm)
	binary.BigEndian.PutUint32(blob[offset:offset+4], 32)
	for index := offset + 4; index < len(blob); index++ {
		blob[index] = seed + byte(index)
	}
	sum := sha256.Sum256(blob)
	return hostbootstrap.PublicKeyIdentity{
		Algorithm:         hostbootstrap.PublicKeyAlgorithmED25519,
		Material:          base64.StdEncoding.EncodeToString(blob),
		FingerprintSHA256: hex.EncodeToString(sum[:]),
	}
}

func testRequest(target hostbootstrap.Target) hostbootstrap.Request {
	account := hostbootstrap.AccountIdentity{Name: "pairuser"}
	product := hostbootstrap.ArtifactIdentity{
		ID:      "nvpair",
		Version: "1.2.3",
		SHA256:  strings.Repeat("12", 32),
	}
	helper := hostbootstrap.ArtifactIdentity{
		ID:      "nvpair-host-helper",
		Version: "1.2.3",
		SHA256:  strings.Repeat("34", 32),
	}
	switch target.Platform {
	case hostbootstrap.PlatformWindows:
		account.HomePath = `C:\Users\pairuser`
		account.AuthorizedKeysPath = `C:\Users\pairuser\.ssh\authorized_keys`
		product.Path = `C:\Program Files\NVIDIA Corporation\PAIR\product`
		helper.Path = `C:\Program Files\NVIDIA Corporation\PAIR\nvpair-host-helper.exe`
	case hostbootstrap.PlatformDarwin:
		account.HomePath = "/Users/pairuser"
		account.AuthorizedKeysPath = "/Users/pairuser/.ssh/authorized_keys"
		product.Path = "/Applications/NVPAIR.app"
		helper.Path = "/Library/PrivilegedHelperTools/nvpair-host-helper"
	case hostbootstrap.PlatformLinux:
		account.HomePath = "/home/pairuser"
		account.AuthorizedKeysPath = "/home/pairuser/.ssh/authorized_keys"
		product.Path = "/opt/nvpair/product"
		helper.Path = "/usr/libexec/nvpair-host-helper"
	}
	return hostbootstrap.Request{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   strings.Repeat("ab", 16),
		Binding: hostbootstrap.Binding{
			Target:        target,
			Lane:          hostbootstrap.LaneQuickConnect,
			Role:          hostbootstrap.RoleAuto,
			RuntimeOwner:  hostbootstrap.RoleHeadless,
			Account:       account,
			ControllerKey: testPublicKey(1),
			Endpoint:      hostbootstrap.SSHEndpoint{Address: "192.0.2.10", Port: 22},
			Product:       product,
			Helper:        helper,
		},
	}
}

func absentTestObservations() hostbootstrap.Observations {
	return hostbootstrap.Observations{
		SSHService:    hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Firewall:      hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent},
		AuthorizedKey: hostbootstrap.AuthorizedKeyObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Helper:        hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Product:       hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent},
		RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{},
	}
}

func exactTestObservations(request hostbootstrap.Request) hostbootstrap.Observations {
	endpoint := request.Binding.Endpoint
	product := request.Binding.Product
	helper := request.Binding.Helper
	observed := hostbootstrap.Observations{
		SSHService: hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		},
		Firewall: hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Endpoint:  &endpoint,
		},
		AuthorizedKey: hostbootstrap.AuthorizedKeyObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity: &hostbootstrap.AuthorizedKeyIdentity{
				AccountName:       request.Binding.Account.Name,
				Path:              request.Binding.Account.AuthorizedKeysPath,
				FingerprintSHA256: request.Binding.ControllerKey.FingerprintSHA256,
			},
		},
		Helper: hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &helper,
		},
		Product: hostbootstrap.ArtifactObservation{
			Ownership: hostbootstrap.OwnershipOwned,
			Identity:  &product,
		},
		RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{{
			Ownership: hostbootstrap.OwnershipOwned,
			Owner:     request.Binding.RuntimeOwner,
		}},
	}
	if request.Binding.Target.Platform == hostbootstrap.PlatformDarwin {
		observed.Firewall = hostbootstrap.EndpointObservation{
			Ownership: hostbootstrap.OwnershipNotApplicable,
		}
	}
	return observed
}

type fakePlatform struct {
	target           hostbootstrap.Target
	distro           string
	privileged       bool
	observed         hostbootstrap.Observations
	inspectQueue     []hostbootstrap.Observations
	events           []string
	desktopRunning   bool
	repairErr        error
	removalRequest   hostbootstrap.Request
	removalCrashKind hostbootstrap.ResourceKind
	removalCrashed   bool
}

func (platform *fakePlatform) Target(context.Context) (hostbootstrap.Target, string, error) {
	return platform.target, platform.distro, nil
}

func (platform *fakePlatform) Privileged() (bool, error) {
	return platform.privileged, nil
}

func (platform *fakePlatform) DesktopRunning(context.Context, hostbootstrap.Request) (bool, error) {
	platform.events = append(platform.events, "inspect-desktop")
	return platform.desktopRunning, nil
}

func (platform *fakePlatform) ValidatePending(
	_ context.Context,
	pending pendingEffect,
	_ hostbootstrap.Request,
	current hostbootstrap.Observations,
) error {
	if pending.EffectID != "" ||
		(!sameResourceObservation(current, pending.ExpectedPre, pending.Resource) &&
			!sameResourceObservation(current, pending.ExpectedPost, pending.Resource)) {
		return ErrPlanChanged
	}
	return nil
}

func (platform *fakePlatform) RecoverPending(
	_ context.Context,
	pending pendingEffect,
	_ hostbootstrap.Request,
) (bool, error) {
	platform.events = append(platform.events, "recover:"+string(pending.Resource))
	return reflect.DeepEqual(platform.observed, pending.ExpectedPost), nil
}

func (platform *fakePlatform) ValidateRepair(context.Context, hostbootstrap.Plan) error {
	return platform.repairErr
}

func (platform *fakePlatform) ValidateRemoval(
	_ context.Context,
	kind hostbootstrap.ResourceKind,
	request hostbootstrap.Request,
) (bool, error) {
	platform.removalRequest = request
	if !resourceOwned(platform.observed, kind) {
		return false, nil
	}
	if !resourceObservationExact(platform.observed, request.Binding, kind) {
		return false, ErrForeignCollision
	}
	return true, nil
}

func (platform *fakePlatform) ValidateUninstallPending(
	_ context.Context,
	pending uninstallEffect,
	_ hostbootstrap.Request,
	current hostbootstrap.Observations,
) error {
	for _, kind := range uninstallOrder {
		expected := pending.ExpectedPre
		if kind == pending.Resource {
			if sameResourceObservation(current, expected, kind) ||
				sameResourceObservation(
					current,
					pending.ExpectedPost,
					kind,
				) {
				continue
			}
			return ErrPlanChanged
		}
		if !sameResourceObservation(current, expected, kind) {
			return ErrPlanChanged
		}
	}
	return nil
}

func (platform *fakePlatform) CapturePendingMarker(
	kind hostbootstrap.ResourceKind,
) (resourceMarker, bool, error) {
	request := platform.removalRequest
	if request.OperationID == "" {
		return resourceMarker{}, false, nil
	}
	marker := resourceMarker{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		Kind:           kind,
		OperationID:    request.OperationID,
		IdentitySHA256: resourceIdentitySHA256(kind, request.Binding),
	}
	if kind == hostbootstrap.ResourceAuthorizedKey {
		marker.AuthorizedKeyLine = desiredAuthorizedKeyLine(request)
	}
	if kind == hostbootstrap.ResourceProduct {
		marker.Files = []payloadManifestFile{{
			Path:   "nvpair",
			Type:   payloadTypeFile,
			Size:   1,
			Mode:   0755,
			SHA256: payloadDigest([]byte("x")),
		}}
	}
	return marker, true, nil
}

func (platform *fakePlatform) Inspect(_ context.Context, _ hostbootstrap.Request) (hostbootstrap.Observations, error) {
	platform.events = append(platform.events, "inspect")
	if len(platform.inspectQueue) > 0 {
		platform.observed = platform.inspectQueue[0]
		platform.inspectQueue = platform.inspectQueue[1:]
	}
	return platform.observed, nil
}

func (platform *fakePlatform) ApplyResource(ctx context.Context, kind hostbootstrap.ResourceKind, request hostbootstrap.Request) error {
	platform.events = append(platform.events, "apply:"+string(kind))
	exact := exactTestObservations(request)
	switch kind {
	case hostbootstrap.ResourceSSHService:
		platform.observed.SSHService = exact.SSHService
	case hostbootstrap.ResourceFirewall:
		platform.observed.Firewall = exact.Firewall
	case hostbootstrap.ResourceAuthorizedKey:
		platform.observed.AuthorizedKey = exact.AuthorizedKey
	case hostbootstrap.ResourceHelper:
		platform.observed.Helper = exact.Helper
	case hostbootstrap.ResourceProduct:
		platform.observed.Product = exact.Product
	case hostbootstrap.ResourceActiveRole:
		if request.Binding.RuntimeOwner == hostbootstrap.RoleHeadless {
			running, err := platform.DesktopRunning(ctx, request)
			if err != nil {
				return err
			}
			if running {
				return ErrRuntimeOwnerActive
			}
		}
		if len(platform.observed.RuntimeOwners) == 1 &&
			platform.observed.RuntimeOwners[0].Ownership == hostbootstrap.OwnershipOwned &&
			platform.observed.RuntimeOwners[0].Owner != request.Binding.RuntimeOwner {
			previous := platform.observed.RuntimeOwners[0].Owner
			if err := platform.StopRole(ctx, previous, request); err != nil {
				return err
			}
			if _, err := platform.Inspect(ctx, request); err != nil {
				return err
			}
		}
		return platform.StartRole(ctx, request.Binding.RuntimeOwner, request)
	default:
		return errors.New("unexpected resource")
	}
	return nil
}

func (platform *fakePlatform) StopRole(_ context.Context, role hostbootstrap.Role, _ hostbootstrap.Request) error {
	platform.events = append(platform.events, "stop-role:"+string(role))
	platform.observed.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{}
	return nil
}

func (platform *fakePlatform) StartRole(_ context.Context, role hostbootstrap.Role, _ hostbootstrap.Request) error {
	platform.events = append(platform.events, "start-role:"+string(role))
	platform.observed.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{{
		Ownership: hostbootstrap.OwnershipOwned,
		Owner:     role,
	}}
	return nil
}

func (platform *fakePlatform) RemoveResource(_ context.Context, kind hostbootstrap.ResourceKind, _ hostbootstrap.Request) error {
	platform.events = append(platform.events, "remove:"+string(kind))
	switch kind {
	case hostbootstrap.ResourceSSHService:
		platform.observed.SSHService = hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent}
		platform.removalRequest = hostbootstrap.Request{}
	case hostbootstrap.ResourceFirewall:
		platform.observed.Firewall = hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent}
	case hostbootstrap.ResourceAuthorizedKey:
		platform.observed.AuthorizedKey = hostbootstrap.AuthorizedKeyObservation{Ownership: hostbootstrap.OwnershipAbsent}
	case hostbootstrap.ResourceHelper:
		platform.observed.Helper = hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent}
	case hostbootstrap.ResourceProduct:
		platform.observed.Product = hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent}
	case hostbootstrap.ResourceActiveRole:
		platform.observed.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{}
	default:
		return errors.New("unexpected resource")
	}
	if kind == platform.removalCrashKind && !platform.removalCrashed {
		platform.removalCrashed = true
		return errors.New("injected removal crash")
	}
	return nil
}

type memoryState struct {
	request          *hostbootstrap.Request
	plan             *hostbootstrap.Plan
	receipt          *hostbootstrap.Receipt
	receipts         map[string]hostbootstrap.Receipt
	pending          *pendingEffect
	uninstallPending *uninstallEffect
	tombstone        *hostbootstrap.Observations
	mutationMu       sync.Mutex
}

func (state *memoryState) SaveOperation(request hostbootstrap.Request, plan hostbootstrap.Plan) error {
	if state.request != nil && (*state.request != request || !reflect.DeepEqual(*state.plan, plan)) {
		return ErrStateIdentity
	}
	requestCopy := request
	planCopy := plan
	state.request = &requestCopy
	state.plan = &planCopy
	return nil
}

func (state *memoryState) SaveRepairOperation(
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) error {
	if state.request == nil ||
		state.plan == nil ||
		state.receipt == nil ||
		state.pending != nil ||
		*state.request != request ||
		plan.OperationID != request.OperationID ||
		plan.Binding != request.Binding ||
		(plan.Decision != hostbootstrap.DecisionRepairOwned &&
			plan.Decision != hostbootstrap.DecisionNoOp) {
		return ErrStateIdentity
	}
	copy := plan
	state.plan = &copy
	return nil
}

func (state *memoryState) LoadOperation() (hostbootstrap.Request, hostbootstrap.Plan, error) {
	if state.request == nil || state.plan == nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, ErrStateMissing
	}
	return *state.request, *state.plan, nil
}

func (state *memoryState) SaveReceipt(receipt hostbootstrap.Receipt) error {
	if state.request == nil ||
		state.request.OperationID != receipt.OperationID {
		return ErrReceiptImmutable
	}
	if state.receipts == nil {
		state.receipts = make(map[string]hostbootstrap.Receipt)
	}
	if existing, found := state.receipts[receipt.OperationID]; found {
		if !reflect.DeepEqual(existing, receipt) {
			return ErrReceiptImmutable
		}
		copy := existing
		state.receipt = &copy
		return nil
	}
	if state.receipt != nil {
		state.receipts[state.receipt.OperationID] = *state.receipt
	}
	copy := receipt
	state.receipt = &copy
	state.receipts[receipt.OperationID] = copy
	state.tombstone = nil
	return nil
}

func (state *memoryState) LoadReceipt() (hostbootstrap.Receipt, error) {
	if state.request == nil {
		return hostbootstrap.Receipt{}, ErrStateMissing
	}
	if state.receipt != nil &&
		state.receipt.OperationID == state.request.OperationID {
		return *state.receipt, nil
	}
	receipt, found := state.receipts[state.request.OperationID]
	if !found {
		return hostbootstrap.Receipt{}, ErrStateMissing
	}
	return receipt, nil
}

func (state *memoryState) SavePending(pending pendingEffect) error {
	copy := pending
	state.pending = &copy
	return nil
}

func (state *memoryState) UpdatePending(
	expected pendingEffect,
	pending pendingEffect,
) error {
	if state.pending == nil ||
		!reflect.DeepEqual(*state.pending, expected) ||
		!pendingTransitionValid(expected, pending) {
		return ErrStateIdentity
	}
	copy := pending
	state.pending = &copy
	return nil
}

func (state *memoryState) AdoptPending(
	expected pendingEffect,
	pending pendingEffect,
) error {
	if state.pending == nil ||
		!reflect.DeepEqual(*state.pending, expected) ||
		pending.Revision != expected.Revision+1 ||
		pending.EffectID != "role:marker-advance" ||
		!pending.EffectComplete ||
		!pendingIdentityEqual(expected, pending) {
		return ErrStateIdentity
	}
	copy := pending
	state.pending = &copy
	return nil
}

func (state *memoryState) LoadPending() (pendingEffect, error) {
	if state.pending == nil {
		return pendingEffect{}, ErrStateMissing
	}
	return *state.pending, nil
}

func (state *memoryState) ClearPending() error {
	state.pending = nil
	return nil
}

func (state *memoryState) SaveUninstallPending(
	pending uninstallEffect,
) error {
	if state.uninstallPending != nil &&
		!reflect.DeepEqual(*state.uninstallPending, pending) {
		return ErrStateIdentity
	}
	copy := pending
	state.uninstallPending = &copy
	return nil
}

func (state *memoryState) UpdateUninstallPending(
	expected uninstallEffect,
	next uninstallEffect,
) error {
	if state.uninstallPending == nil ||
		!reflect.DeepEqual(*state.uninstallPending, expected) ||
		!uninstallPendingTransitionValid(expected, next) {
		return ErrStateIdentity
	}
	copy := next
	state.uninstallPending = &copy
	return nil
}

func (state *memoryState) LoadUninstallPending() (
	uninstallEffect,
	error,
) {
	if state.uninstallPending == nil {
		return uninstallEffect{}, ErrStateMissing
	}
	return *state.uninstallPending, nil
}

func (state *memoryState) ClearUninstallPending() error {
	state.uninstallPending = nil
	return nil
}

func (state *memoryState) RemoveOperation() error {
	state.request = nil
	state.plan = nil
	state.pending = nil
	state.uninstallPending = nil
	return nil
}

func (state *memoryState) SaveUninstalled(
	receipt hostbootstrap.Receipt,
	observed hostbootstrap.Observations,
) error {
	if state.receipt == nil ||
		!reflect.DeepEqual(*state.receipt, receipt) {
		return ErrStateIdentity
	}
	copy := observed
	state.tombstone = &copy
	return nil
}

func (state *memoryState) RunMutation(operation func() error) error {
	state.mutationMu.Lock()
	defer state.mutationMu.Unlock()
	return operation()
}

func TestCleanApplySecondRunNoOpAndRebootSafeVerify(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	platform := &fakePlatform{
		target:     request.Binding.Target,
		privileged: true,
		observed:   absentTestObservations(),
	}
	state := &memoryState{}
	bootstrap := NewBootstrap(platform, state)

	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if plan.Decision != hostbootstrap.DecisionApply {
		t.Fatalf("decision = %q, want apply", plan.Decision)
	}
	if state.request != nil || state.plan != nil || state.receipt != nil {
		t.Fatal("Review() persisted state")
	}
	if _, err := bootstrap.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	receipt, err := bootstrap.Verify(context.Background(), plan)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if receipt.Phase != hostbootstrap.PhaseComplete {
		t.Fatalf("receipt phase = %q", receipt.Phase)
	}

	secondRequest := request
	secondRequest.OperationID = strings.Repeat("cd", 16)
	secondBootstrap := NewBootstrap(platform, &memoryState{})
	second, err := secondBootstrap.Review(context.Background(), secondRequest)
	if err != nil {
		t.Fatalf("second Review() error = %v", err)
	}
	if second.Decision != hostbootstrap.DecisionNoOp || len(second.Actions) != 0 {
		t.Fatalf("second plan = %#v, want exact no-op", second)
	}
	before := append([]string(nil), platform.events...)
	if _, err := secondBootstrap.Apply(context.Background(), second); err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	for _, event := range platform.events[len(before):] {
		if strings.HasPrefix(event, "apply:") || strings.HasPrefix(event, "start-role:") || strings.HasPrefix(event, "stop-role:") {
			t.Fatalf("no-op performed effect %q", event)
		}
	}

	restarted := NewBootstrap(platform, state)
	readback, err := restarted.ReadReceipt()
	if err != nil {
		t.Fatalf("ReadReceipt() error = %v", err)
	}
	if !reflect.DeepEqual(receipt, readback) {
		t.Fatalf("receipt changed across readback")
	}
}

func TestOwnedPartialRepairAndKeyRotation(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureARM64,
	})
	observed := exactTestObservations(request)
	observed.Helper.Identity = &hostbootstrap.ArtifactIdentity{
		ID:      request.Binding.Helper.ID,
		Version: "1.2.2",
		SHA256:  strings.Repeat("56", 32),
		Path:    request.Binding.Helper.Path,
	}
	oldKey := testPublicKey(9)
	observed.AuthorizedKey.Identity = &hostbootstrap.AuthorizedKeyIdentity{
		AccountName:       request.Binding.Account.Name,
		Path:              request.Binding.Account.AuthorizedKeysPath,
		FingerprintSHA256: oldKey.FingerprintSHA256,
	}
	platform := &fakePlatform{
		target:     request.Binding.Target,
		distro:     "ubuntu",
		privileged: true,
		observed:   observed,
	}
	bootstrap := NewBootstrap(platform, &memoryState{})

	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	wantActions := []hostbootstrap.ResourceKind{
		hostbootstrap.ResourceAuthorizedKey,
		hostbootstrap.ResourceHelper,
	}
	if plan.Decision != hostbootstrap.DecisionRepairOwned || !reflect.DeepEqual(plan.Actions, wantActions) {
		t.Fatalf("repair plan = %#v, want actions %#v", plan, wantActions)
	}
	if _, err := bootstrap.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !reflect.DeepEqual(platform.observed, exactTestObservations(request)) {
		t.Fatalf("repaired observations = %#v", platform.observed)
	}
}

func TestRepairPreflightsEveryPlannedResourceBeforeFirstEffect(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	observed := exactTestObservations(request)
	observed.SSHService.Endpoint = &hostbootstrap.SSHEndpoint{
		Address: request.Binding.Endpoint.Address,
		Port:    2222,
	}
	helper := request.Binding.Helper
	helper.SHA256 = strings.Repeat("56", 32)
	observed.Helper.Identity = &helper
	platform := &fakePlatform{
		target:     request.Binding.Target,
		distro:     "debian",
		privileged: true,
		observed:   observed,
		repairErr:  ErrForeignCollision,
	}
	bootstrap := NewBootstrap(platform, &memoryState{})
	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Apply(
		context.Background(),
		plan,
	); !errors.Is(err, ErrForeignCollision) {
		t.Fatalf("Apply() error = %v", err)
	}
	for _, event := range platform.events {
		if strings.HasPrefix(event, "apply:") {
			t.Fatalf("preflight refusal followed effect %q", event)
		}
	}
}

func TestRoleSwitchStopsAndVerifiesPriorOwnerBeforeStart(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformDarwin,
		Architecture: hostbootstrap.ArchitectureARM64,
	})
	request.Binding.Role = hostbootstrap.RoleDesktop
	request.Binding.RuntimeOwner = hostbootstrap.RoleDesktop
	observed := exactTestObservations(request)
	observed.RuntimeOwners[0].Owner = hostbootstrap.RoleHeadless
	platform := &fakePlatform{
		target:     request.Binding.Target,
		privileged: true,
		observed:   observed,
	}
	bootstrap := NewBootstrap(platform, &memoryState{})
	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if _, err := bootstrap.Apply(context.Background(), plan); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	wantTail := []string{
		"stop-role:headless",
		"inspect",
		"start-role:desktop",
		"inspect",
	}
	if len(platform.events) < len(wantTail) {
		t.Fatalf("events = %#v", platform.events)
	}
	gotTail := platform.events[len(platform.events)-len(wantTail):]
	if !reflect.DeepEqual(gotTail, wantTail) {
		t.Fatalf("role events = %#v, want %#v", gotTail, wantTail)
	}
}

func TestHeadlessSelectionRefusesRunningDesktopBeforeEffects(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	request.Binding.Role = hostbootstrap.RoleHeadless
	request.Binding.RuntimeOwner = hostbootstrap.RoleHeadless
	observed := exactTestObservations(request)
	observed.RuntimeOwners[0].Owner = hostbootstrap.RoleDesktop
	platform := &fakePlatform{
		target:         request.Binding.Target,
		privileged:     true,
		observed:       observed,
		desktopRunning: true,
	}
	bootstrap := NewBootstrap(platform, &memoryState{})
	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Apply(context.Background(), plan); !errors.Is(err, ErrRuntimeOwnerActive) {
		t.Fatalf("Apply() error = %v, want ErrRuntimeOwnerActive", err)
	}
	for _, event := range platform.events {
		if event == "start-role:headless" {
			t.Fatal("headless owner started while Desktop broker tree was running")
		}
	}
}

func TestForeignCollisionAndChangedReviewRefuseBeforeEffects(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureARM64,
	})
	for _, kind := range []hostbootstrap.ResourceKind{
		hostbootstrap.ResourceSSHService,
		hostbootstrap.ResourceFirewall,
		hostbootstrap.ResourceAuthorizedKey,
		hostbootstrap.ResourceHelper,
		hostbootstrap.ResourceProduct,
		hostbootstrap.ResourceActiveRole,
	} {
		t.Run(string(kind), func(t *testing.T) {
			observed := absentTestObservations()
			exact := exactTestObservations(request)
			switch kind {
			case hostbootstrap.ResourceSSHService:
				observed.SSHService = exact.SSHService
				observed.SSHService.Ownership = hostbootstrap.OwnershipForeign
			case hostbootstrap.ResourceFirewall:
				observed.Firewall = exact.Firewall
				observed.Firewall.Ownership = hostbootstrap.OwnershipForeign
			case hostbootstrap.ResourceAuthorizedKey:
				observed.AuthorizedKey = exact.AuthorizedKey
				observed.AuthorizedKey.Ownership = hostbootstrap.OwnershipForeign
			case hostbootstrap.ResourceHelper:
				observed.Helper = exact.Helper
				observed.Helper.Ownership = hostbootstrap.OwnershipForeign
			case hostbootstrap.ResourceProduct:
				observed.Product = exact.Product
				observed.Product.Ownership = hostbootstrap.OwnershipForeign
			case hostbootstrap.ResourceActiveRole:
				observed.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{{
					Ownership: hostbootstrap.OwnershipForeign,
					Owner:     hostbootstrap.RoleDesktop,
				}}
			}
			platform := &fakePlatform{
				target:     request.Binding.Target,
				privileged: true,
				observed:   observed,
			}
			bootstrap := NewBootstrap(platform, &memoryState{})
			plan, err := bootstrap.Review(context.Background(), request)
			if err != nil {
				t.Fatalf("Review() error = %v", err)
			}
			if plan.Decision != hostbootstrap.DecisionRefuseForeign {
				t.Fatalf("decision = %q", plan.Decision)
			}
			before := len(platform.events)
			if _, err := bootstrap.Apply(context.Background(), plan); !errors.Is(err, ErrForeignCollision) {
				t.Fatalf("Apply() error = %v, want ErrForeignCollision", err)
			}
			for _, event := range platform.events[before:] {
				if strings.HasPrefix(event, "apply:") || strings.HasPrefix(event, "remove:") {
					t.Fatalf("foreign state caused effect %q", event)
				}
			}
		})
	}

	clean := absentTestObservations()
	changed := absentTestObservations()
	exact := exactTestObservations(request)
	changed.Firewall = exact.Firewall
	changed.Firewall.Ownership = hostbootstrap.OwnershipForeign
	platform := &fakePlatform{
		target:       request.Binding.Target,
		privileged:   true,
		observed:     clean,
		inspectQueue: []hostbootstrap.Observations{clean, changed},
	}
	bootstrap := NewBootstrap(platform, &memoryState{})
	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	before := len(platform.events)
	if _, err := bootstrap.Apply(context.Background(), plan); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("Apply() error = %v, want ErrPlanChanged", err)
	}
	for _, event := range platform.events[before:] {
		if strings.HasPrefix(event, "apply:") {
			t.Fatalf("changed state caused effect %q", event)
		}
	}
}

func TestMarkerBoundUninstallPreservesForeignResources(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	observed := exactTestObservations(request)
	observed.Firewall.Ownership = hostbootstrap.OwnershipForeign
	observed.Product.Ownership = hostbootstrap.OwnershipForeign
	platform := &fakePlatform{
		target:     request.Binding.Target,
		distro:     "debian",
		privileged: true,
		observed:   observed,
	}
	state := &memoryState{}
	exact := exactTestObservations(request)
	plan, err := hostbootstrap.Reconcile(request, exact)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	receipt, err := hostbootstrap.NewReceipt(plan, exact)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	bootstrap := NewBootstrap(platform, state)
	if err := bootstrap.Uninstall(context.Background(), request); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	wantRemoved := []string{
		"remove:active-role",
		"remove:helper",
		"remove:authorized-key",
		"remove:ssh-service",
	}
	var removed []string
	for _, event := range platform.events {
		if strings.HasPrefix(event, "remove:") {
			removed = append(removed, event)
		}
	}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Fatalf("removed = %#v, want %#v", removed, wantRemoved)
	}
	if platform.observed.Firewall.Ownership != hostbootstrap.OwnershipForeign ||
		platform.observed.Product.Ownership != hostbootstrap.OwnershipForeign {
		t.Fatalf("foreign resources changed: %#v", platform.observed)
	}
}

func TestApplyVerifyUninstallReinstallRetainsLifecycleLineage(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	platform := &fakePlatform{
		target:     request.Binding.Target,
		distro:     "debian",
		privileged: true,
		observed:   absentTestObservations(),
	}
	state := &memoryState{}
	bootstrap := NewBootstrap(platform, state)
	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	firstReceipt, err := bootstrap.Verify(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.Uninstall(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if state.tombstone == nil ||
		state.receipt == nil ||
		!reflect.DeepEqual(*state.receipt, firstReceipt) {
		t.Fatal("uninstall did not retain receipt-bound lineage")
	}
	next := request
	next.OperationID = strings.Repeat("cd", 16)
	nextPlan, err := bootstrap.Review(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Apply(
		context.Background(),
		nextPlan,
	); err != nil {
		t.Fatal(err)
	}
	nextReceipt, err := bootstrap.Verify(
		context.Background(),
		nextPlan,
	)
	if err != nil {
		t.Fatal(err)
	}
	if nextReceipt.OperationID != next.OperationID {
		t.Fatalf("reinstall receipt = %#v", nextReceipt)
	}
	if state.tombstone != nil {
		t.Fatal("completed reinstall retained uninstall tombstone")
	}
	retained, found := state.receipts[firstReceipt.OperationID]
	if !found || !reflect.DeepEqual(retained, firstReceipt) {
		t.Fatal("historical receipt was not retained after reinstall")
	}
}

func TestUninstallRefusesOwnedDriftBeforeAnyRemovalEffect(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	for _, kind := range uninstallOrder {
		t.Run(string(kind), func(t *testing.T) {
			observed := exactTestObservations(request)
			switch kind {
			case hostbootstrap.ResourceSSHService:
				changed := request.Binding.Endpoint
				changed.Port = 2222
				observed.SSHService.Endpoint = &changed
			case hostbootstrap.ResourceFirewall:
				changed := request.Binding.Endpoint
				changed.Port = 2222
				observed.Firewall.Endpoint = &changed
			case hostbootstrap.ResourceAuthorizedKey:
				observed.AuthorizedKey.Identity.FingerprintSHA256 = strings.Repeat("56", 32)
			case hostbootstrap.ResourceHelper:
				observed.Helper.Identity.SHA256 = strings.Repeat("56", 32)
			case hostbootstrap.ResourceProduct:
				observed.Product.Identity.SHA256 = strings.Repeat("56", 32)
			case hostbootstrap.ResourceActiveRole:
				observed.RuntimeOwners[0].Owner = hostbootstrap.RoleDesktop
			}
			exact := exactTestObservations(request)
			plan, err := hostbootstrap.Reconcile(request, exact)
			if err != nil {
				t.Fatal(err)
			}
			state := &memoryState{}
			if err := state.SaveOperation(request, plan); err != nil {
				t.Fatal(err)
			}
			receipt, err := hostbootstrap.NewReceipt(plan, exact)
			if err != nil {
				t.Fatal(err)
			}
			if err := state.SaveReceipt(receipt); err != nil {
				t.Fatal(err)
			}
			platform := &fakePlatform{
				target:     request.Binding.Target,
				privileged: true,
				observed:   observed,
			}
			bootstrap := NewBootstrap(platform, state)
			if err := bootstrap.Uninstall(
				context.Background(),
				request,
			); !errors.Is(err, ErrForeignCollision) {
				t.Fatalf("Uninstall() error = %v", err)
			}
			for _, event := range platform.events {
				if strings.HasPrefix(event, "remove:") {
					t.Fatalf("owned drift caused removal effect %q", event)
				}
			}
		})
	}
}

func TestUninstallRefusesEveryPendingCrashBeforeMarker(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	expectedPre := plan.Observed
	for index, action := range plan.Actions {
		t.Run(string(action), func(t *testing.T) {
			state := &memoryState{}
			if err := state.SaveOperation(request, plan); err != nil {
				t.Fatal(err)
			}
			pending := pendingEffect{
				SchemaVersion:  hostbootstrap.SchemaVersion,
				OperationID:    request.OperationID,
				PlanSHA256:     digest,
				Resource:       action,
				ActionIndex:    index,
				ExpectedPre:    expectedPre,
				ExpectedPost:   observationsAfterAction(expectedPre, request, action),
				EffectID:       "effect-before-marker",
				EffectComplete: true,
				Revision:       2,
			}
			if err := state.SavePending(pending); err != nil {
				t.Fatal(err)
			}
			platform := &fakePlatform{
				target:     request.Binding.Target,
				privileged: true,
				observed:   exactTestObservations(request),
			}
			bootstrap := NewBootstrap(platform, state)
			if err := bootstrap.Uninstall(
				context.Background(),
				request,
			); !errors.Is(err, ErrStateIdentity) {
				t.Fatalf("Uninstall() error = %v", err)
			}
			for _, event := range platform.events {
				if strings.HasPrefix(event, "remove:") {
					t.Fatalf("pending journal caused removal %q", event)
				}
			}
		})
		expectedPre = observationsAfterAction(expectedPre, request, action)
	}
}

func TestUninstallResumesEveryResourceBeforeMarkerRemoval(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	for _, crashKind := range uninstallOrder {
		t.Run(string(crashKind), func(t *testing.T) {
			exact := exactTestObservations(request)
			plan, err := hostbootstrap.Reconcile(request, exact)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := hostbootstrap.NewReceipt(plan, exact)
			if err != nil {
				t.Fatal(err)
			}
			state := &memoryState{}
			if err := state.SaveOperation(request, plan); err != nil {
				t.Fatal(err)
			}
			if err := state.SaveReceipt(receipt); err != nil {
				t.Fatal(err)
			}
			platform := &fakePlatform{
				target:           request.Binding.Target,
				privileged:       true,
				observed:         exact,
				removalCrashKind: crashKind,
			}
			bootstrap := NewBootstrap(platform, state)
			if err := bootstrap.Uninstall(
				context.Background(),
				request,
			); err == nil {
				t.Fatal("injected removal crash was ignored")
			}
			pending, err := state.LoadUninstallPending()
			if err != nil ||
				pending.Resource != crashKind ||
				pending.EffectComplete {
				t.Fatalf("pending=%#v error=%v", pending, err)
			}
			if err := bootstrap.Uninstall(
				context.Background(),
				request,
			); err != nil {
				t.Fatalf("uninstall retry error = %v", err)
			}
			if state.uninstallPending != nil ||
				state.tombstone == nil ||
				state.request != nil {
				t.Fatal("retry did not complete tombstone teardown")
			}
		})
	}
}

func TestApplyRequiresNativePrivilegeWithoutElevation(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformDarwin,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	observed := absentTestObservations()
	observed.Firewall = hostbootstrap.EndpointObservation{
		Ownership: hostbootstrap.OwnershipNotApplicable,
	}
	platform := &fakePlatform{
		target:     request.Binding.Target,
		privileged: false,
		observed:   observed,
	}
	bootstrap := NewBootstrap(platform, &memoryState{})
	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Apply(context.Background(), plan); !errors.Is(err, ErrNotPrivileged) {
		t.Fatalf("Apply() error = %v, want ErrNotPrivileged", err)
	}
	for _, event := range platform.events {
		if strings.HasPrefix(event, "apply:") {
			t.Fatalf("unprivileged apply caused effect %q", event)
		}
	}
}

func TestHeadlessHelperTransitionMasksOnlyBoundEffects(t *testing.T) {
	for _, effectID := range []string{
		"helper:stop-headless-wrapper",
		"helper:stop-prior",
		"helper:install-executable",
		"helper:save-principal",
		"helper:ensure-product-parent",
		"helper:definition:0",
		"apply:helper:command:0",
		"helper:start-headless-wrapper",
	} {
		if !helperHeadlessTransitionEffect(effectID) {
			t.Fatalf("transition effect rejected %q", effectID)
		}
	}
	for _, effectID := range []string{
		"",
		"marker:helper",
		"product:install-tree",
		"role:start:command:0",
	} {
		if helperHeadlessTransitionEffect(effectID) {
			t.Fatalf("unrelated effect accepted %q", effectID)
		}
	}
}

func TestDirectMutatingBootstrapVerbsShareCrossProcessLock(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	filesystem := &fakeSecureFS{}
	firstState := newDiskState("/state", filesystem)
	secondState := newDiskState("/state", filesystem)
	if err := firstState.SaveOperation(request, plan); err != nil {
		t.Fatal(err)
	}
	var lock sync.Mutex
	held := false
	acquire := func() (func() error, error) {
		lock.Lock()
		defer lock.Unlock()
		if held {
			return nil, ErrStateIdentity
		}
		held = true
		return func() error {
			lock.Lock()
			held = false
			lock.Unlock()
			return nil
		}, nil
	}
	firstState.acquireLock = acquire
	secondState.acquireLock = acquire
	firstPlatform := &fakePlatform{
		target:     request.Binding.Target,
		privileged: true,
		observed:   absentTestObservations(),
	}
	secondPlatform := &fakePlatform{
		target:     request.Binding.Target,
		privileged: true,
		observed:   absentTestObservations(),
	}
	first := NewBootstrap(firstPlatform, firstState)
	second := NewBootstrap(secondPlatform, secondState)
	started := make(chan struct{})
	release := make(chan struct{})
	first.effectHook = func(
		point effectPoint,
		_ hostbootstrap.ResourceKind,
	) error {
		if point == effectBefore {
			select {
			case <-started:
			default:
				close(started)
				<-release
			}
		}
		return nil
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.Apply(context.Background(), plan)
		firstDone <- err
	}()
	select {
	case <-started:
	case err := <-firstDone:
		t.Fatalf("first apply exited before effect: %v", err)
	}
	if _, err := second.Apply(
		context.Background(),
		plan,
	); !errors.Is(err, ErrStateIdentity) {
		t.Fatalf("second direct apply error = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestApplyRecoversCrashesBeforeAndAfterEveryAction(t *testing.T) {
	for _, crashPoint := range []effectPoint{effectBefore, effectAfter} {
		for _, crashKind := range []hostbootstrap.ResourceKind{
			hostbootstrap.ResourceSSHService,
			hostbootstrap.ResourceFirewall,
			hostbootstrap.ResourceAuthorizedKey,
			hostbootstrap.ResourceHelper,
			hostbootstrap.ResourceProduct,
			hostbootstrap.ResourceActiveRole,
		} {
			t.Run(string(crashPoint)+"/"+string(crashKind), func(t *testing.T) {
				request := testRequest(hostbootstrap.Target{
					Platform:     hostbootstrap.PlatformWindows,
					Architecture: hostbootstrap.ArchitectureAMD64,
				})
				platform := &fakePlatform{
					target:     request.Binding.Target,
					privileged: true,
					observed:   absentTestObservations(),
				}
				state := &memoryState{}
				bootstrap := NewBootstrap(platform, state)
				plan, err := bootstrap.Review(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				crash := errors.New("injected crash")
				bootstrap.effectHook = func(point effectPoint, kind hostbootstrap.ResourceKind) error {
					if point == crashPoint && kind == crashKind {
						return crash
					}
					return nil
				}
				if _, err := bootstrap.Apply(context.Background(), plan); !errors.Is(err, crash) {
					t.Fatalf("Apply() error = %v, want injected crash", err)
				}
				if state.pending == nil || state.pending.Resource != crashKind {
					t.Fatalf("pending = %#v, want %s", state.pending, crashKind)
				}

				restarted := NewBootstrap(platform, state)
				if _, err := restarted.Apply(context.Background(), plan); err != nil {
					t.Fatalf("resumed Apply() error = %v", err)
				}
				if state.pending != nil {
					t.Fatalf("pending survived successful resume: %#v", state.pending)
				}
				if !reflect.DeepEqual(platform.observed, exactTestObservations(request)) {
					t.Fatalf("resumed observations = %#v", platform.observed)
				}
			})
		}
	}
}

func TestPendingRecoveryRefusesUnprovenStateBeforeEffects(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	plan, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	planSHA256, err := digestPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	basePending := pendingEffect{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		PlanSHA256:    planSHA256,
		Resource:      plan.Actions[0],
		ActionIndex:   0,
		ExpectedPre:   plan.Observed,
		ExpectedPost:  observationsAfterAction(plan.Observed, request, plan.Actions[0]),
	}
	tests := []struct {
		name    string
		prepare func(*memoryState, *fakePlatform, *pendingEffect)
	}{
		{
			name: "non-pending owned drift",
			prepare: func(_ *memoryState, platform *fakePlatform, _ *pendingEffect) {
				platform.observed.Firewall = exactTestObservations(request).Firewall
			},
		},
		{
			name: "stored binding drift",
			prepare: func(state *memoryState, _ *fakePlatform, _ *pendingEffect) {
				changed := *state.request
				changed.Binding.Account.Name = "different-user"
				state.request = &changed
			},
		},
		{
			name: "unrecorded internal effect",
			prepare: func(_ *memoryState, _ *fakePlatform, pending *pendingEffect) {
				pending.EffectID = "attacker:effect"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &memoryState{}
			if err := state.SaveOperation(request, plan); err != nil {
				t.Fatal(err)
			}
			pending := basePending
			state.pending = &pending
			platform := &fakePlatform{
				target:     request.Binding.Target,
				privileged: true,
				observed:   absentTestObservations(),
			}
			test.prepare(state, platform, state.pending)
			bootstrap := NewBootstrap(platform, state)
			if _, err := bootstrap.Apply(context.Background(), plan); !errors.Is(err, ErrPlanChanged) {
				t.Fatalf("Apply() error = %v, want ErrPlanChanged", err)
			}
			for _, event := range platform.events {
				if strings.HasPrefix(event, "recover:") ||
					strings.HasPrefix(event, "apply:") ||
					strings.HasPrefix(event, "start-role:") ||
					strings.HasPrefix(event, "stop-role:") {
					t.Fatalf("unproven recovery performed effect %q", event)
				}
			}
		})
	}
}

func TestHelperRepairRefusesInitialInstallRequirement(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformLinux,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	platform := &fakePlatform{
		target:     request.Binding.Target,
		distro:     "debian",
		privileged: true,
		observed:   absentTestObservations(),
	}
	bootstrap := NewBootstrap(platform, &memoryState{})
	plan, err := bootstrap.Review(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Repair(context.Background(), plan); !errors.Is(err, ErrRerunSignedBootstrap) {
		t.Fatalf("clean helper repair error = %v", err)
	}
	if len(platform.events) != 1 || platform.events[0] != "inspect" {
		t.Fatalf("repair performed effects: %#v", platform.events)
	}
}

func TestRepairPersistsFreshDerivedRepairAndNoOpPlans(t *testing.T) {
	request := testRequest(hostbootstrap.Target{
		Platform:     hostbootstrap.PlatformWindows,
		Architecture: hostbootstrap.ArchitectureAMD64,
	})
	original, err := hostbootstrap.Reconcile(request, absentTestObservations())
	if err != nil {
		t.Fatal(err)
	}
	state := &memoryState{}
	if err := state.SaveOperation(request, original); err != nil {
		t.Fatal(err)
	}
	exact := exactTestObservations(request)
	receipt, err := hostbootstrap.NewReceipt(original, exact)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SaveReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	drift := exact
	changedHelper := request.Binding.Helper
	changedHelper.SHA256 = strings.Repeat("56", 32)
	drift.Helper.Identity = &changedHelper
	repair, err := hostbootstrap.Reconcile(request, drift)
	if err != nil {
		t.Fatal(err)
	}
	platform := &fakePlatform{
		target:     request.Binding.Target,
		privileged: true,
		observed:   drift,
	}
	bootstrap := NewBootstrap(platform, state)
	if _, err := bootstrap.Repair(context.Background(), repair); err != nil {
		t.Fatalf("Repair() error = %v", err)
	}
	if !reflect.DeepEqual(*state.plan, repair) {
		t.Fatalf("stored repair plan = %#v", state.plan)
	}
	noOp, err := hostbootstrap.Reconcile(request, exact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Repair(context.Background(), noOp); err != nil {
		t.Fatalf("no-op Repair() error = %v", err)
	}
	if !reflect.DeepEqual(*state.plan, noOp) {
		t.Fatalf("stored no-op plan = %#v", state.plan)
	}
	readback, err := bootstrap.Verify(context.Background(), noOp)
	if err != nil {
		t.Fatalf("no-op Verify() error = %v", err)
	}
	if !reflect.DeepEqual(readback, receipt) {
		t.Fatalf("immutable receipt changed after repair")
	}
}
