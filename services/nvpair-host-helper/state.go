// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"strings"

	"nvpair-shared/hostbootstrap"
)

var windowsSIDPattern = regexp.MustCompile(`^S-1-[0-9]+(?:-[0-9]+)+$`)

const maxHelperStateBytes = 16 << 20

type diskHelperState struct {
	root      string
	mutations mutationCoordinator
	read      func(string) ([]byte, error)
}

type startupPendingRecord struct {
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
	PriorMarker      startupPriorMarker         `json:"priorMarker"`
	PriorMarkerFound bool                       `json:"priorMarkerFound"`
}

type startupPriorMarker struct {
	SchemaVersion     int                        `json:"schemaVersion"`
	Kind              hostbootstrap.ResourceKind `json:"kind"`
	OperationID       string                     `json:"operationId"`
	IdentitySHA256    string                     `json:"identitySha256"`
	AuthorizedKeyLine string                     `json:"authorizedKeyLine"`
	FirewallFamilies  []string                   `json:"firewallFamilies"`
	Files             []json.RawMessage          `json:"files"`
}

type startupLineageMode int

const (
	startupLineageInitial startupLineageMode = iota
	startupLineageTransition
)

func classifyStartupLineage(
	pending startupPendingRecord,
) (startupLineageMode, error) {
	if pending.PriorMarkerFound {
		if pending.PriorMarker.Kind != hostbootstrap.ResourceHelper {
			return startupLineageInitial, hostbootstrap.ErrHelperProtocol
		}
		return startupLineageTransition, nil
	}
	if !reflect.DeepEqual(pending.PriorMarker, startupPriorMarker{}) {
		return startupLineageInitial, hostbootstrap.ErrHelperProtocol
	}
	return startupLineageInitial, nil
}

func newDiskHelperState(root string) *diskHelperState {
	return &diskHelperState{
		root: root,
		mutations: mutationCoordinator{
			acquire: func() (func() error, error) {
				return func() error { return nil }, nil
			},
		},
		read: helperReadSecure,
	}
}

func (state *diskHelperState) LoadPrincipal() (reviewedPrincipal, error) {
	raw, err := state.read(state.path("principal.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return reviewedPrincipal{}, hostbootstrap.ErrHelperProtocol
	}
	if err != nil {
		return reviewedPrincipal{}, err
	}
	fields, err := strictObject(raw, map[string]bool{
		"uid": true,
		"gid": true,
		"sid": true,
	})
	if err != nil || len(fields) != 3 {
		return reviewedPrincipal{}, hostbootstrap.ErrHelperProtocol
	}
	var principal reviewedPrincipal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&principal); err != nil {
		return reviewedPrincipal{}, hostbootstrap.ErrHelperProtocol
	}
	if principal.SID == "" {
		if principal.UID == 0 {
			return reviewedPrincipal{}, hostbootstrap.ErrHelperProtocol
		}
	} else if principal.UID != 0 ||
		principal.GID != 0 ||
		!windowsSIDPattern.MatchString(principal.SID) {
		return reviewedPrincipal{}, hostbootstrap.ErrHelperProtocol
	}
	return principal, nil
}

func (state *diskHelperState) LoadOperation() (
	hostbootstrap.Request,
	hostbootstrap.Plan,
	error,
) {
	raw, err := state.read(state.path("operation.json"))
	if err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, err
	}
	fields, err := strictObject(raw, map[string]bool{
		"request": true,
		"plan":    true,
	})
	if err != nil || len(fields) != 2 {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	var envelope struct {
		Request json.RawMessage `json:"request"`
		Plan    json.RawMessage `json:"plan"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	request, err := hostbootstrap.DecodeRequest(envelope.Request)
	if err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	plan, err := hostbootstrap.DecodePlan(envelope.Plan)
	if err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	if request.OperationID != plan.OperationID || request.Binding != plan.Binding {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, hostbootstrap.ErrHelperProtocol
	}
	return request, plan, nil
}

func (state *diskHelperState) LoadRepairAuthorization(
	request hostbootstrap.Request,
) (hostbootstrap.Receipt, ownedResourceSet, error) {
	operationID := request.OperationID
	if !helperHex32.MatchString(operationID) {
		return hostbootstrap.Receipt{}, nil, hostbootstrap.ErrHelperProtocol
	}
	raw, err := state.read(
		state.path("receipt-" + operationID + ".json"),
	)
	if err != nil {
		return hostbootstrap.Receipt{}, nil, err
	}
	receipt, err := hostbootstrap.DecodeReceipt(raw)
	if err != nil ||
		receipt.OperationID != operationID {
		return hostbootstrap.Receipt{}, nil, hostbootstrap.ErrHelperProtocol
	}
	markers := make(ownedResourceSet)
	for _, kind := range helperManagedResources {
		raw, err := state.read(
			state.path("marker-" + string(kind) + ".json"),
		)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return hostbootstrap.Receipt{}, nil, err
		}
		fields, err := strictObject(raw, map[string]bool{
			"schemaVersion":     true,
			"kind":              true,
			"operationId":       true,
			"identitySha256":    true,
			"authorizedKeyLine": true,
			"firewallFamilies":  true,
			"files":             true,
		})
		if err != nil || len(fields) != 7 {
			return hostbootstrap.Receipt{}, nil, hostbootstrap.ErrHelperProtocol
		}
		var marker struct {
			SchemaVersion     int                        `json:"schemaVersion"`
			Kind              hostbootstrap.ResourceKind `json:"kind"`
			OperationID       string                     `json:"operationId"`
			IdentitySHA256    string                     `json:"identitySha256"`
			AuthorizedKeyLine string                     `json:"authorizedKeyLine"`
			FirewallFamilies  []string                   `json:"firewallFamilies"`
			Files             []json.RawMessage          `json:"files"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&marker); err != nil ||
			marker.SchemaVersion != hostbootstrap.SchemaVersion ||
			marker.Kind != kind ||
			!helperHex32.MatchString(marker.OperationID) ||
			!helperHex64.MatchString(marker.IdentitySHA256) ||
			(kind == hostbootstrap.ResourceAuthorizedKey) !=
				(marker.AuthorizedKeyLine != "") ||
			(kind != hostbootstrap.ResourceFirewall &&
				len(marker.FirewallFamilies) != 0) ||
			(kind == hostbootstrap.ResourceFirewall &&
				request.Binding.Target.Platform == hostbootstrap.PlatformLinux &&
				len(marker.FirewallFamilies) == 0) {
			return hostbootstrap.Receipt{}, nil, hostbootstrap.ErrHelperProtocol
		}
		if marker.OperationID != operationID {
			priorRaw, err := state.read(
				state.path("receipt-" + marker.OperationID + ".json"),
			)
			if err != nil {
				return hostbootstrap.Receipt{}, nil, err
			}
			priorReceipt, err := hostbootstrap.DecodeReceipt(priorRaw)
			if err != nil ||
				!historicalMarkerAuthorized(
					request,
					priorReceipt,
					kind,
					marker.IdentitySHA256,
				) {
				return hostbootstrap.Receipt{}, nil, hostbootstrap.ErrHelperProtocol
			}
		}
		markers[kind] = ownedResourceMarker{
			IdentitySHA256: marker.IdentitySHA256,
		}
	}
	return receipt, markers, nil
}

func (state *diskHelperState) HasPending() (bool, error) {
	_, err := state.read(state.path("pending.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (state *diskHelperState) AuthorizeStartup(
	principal reviewedPrincipal,
) (hostbootstrap.Request, bool, error) {
	request, plan, err := state.LoadOperation()
	if err != nil {
		return hostbootstrap.Request{}, false, err
	}
	_, markers, steadyErr := state.LoadRepairAuthorization(request)
	if steadyErr == nil {
		tombstoneRaw, tombstoneErr := state.read(
			state.path("uninstalled.json"),
		)
		if tombstoneErr == nil {
			if err := state.validateUninstallTombstone(
				tombstoneRaw,
			); err != nil {
				return hostbootstrap.Request{}, false, err
			}
			var tombstone struct {
				ReceiptOperationID string `json:"receiptOperationId"`
			}
			if json.Unmarshal(tombstoneRaw, &tombstone) != nil ||
				tombstone.ReceiptOperationID == request.OperationID {
				return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
			}
		} else if !errors.Is(tombstoneErr, fs.ErrNotExist) {
			return hostbootstrap.Request{}, false, tombstoneErr
		}
		marker, found := markers[hostbootstrap.ResourceHelper]
		if !found ||
			marker.IdentitySHA256 != request.Binding.Helper.SHA256 {
			return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
		}
		if !reviewedPrincipalMatchesAccount(
			principal,
			request.Binding.Account.Name,
		) {
			return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
		}
		return request, true, nil
	}
	if !errors.Is(steadyErr, fs.ErrNotExist) {
		return hostbootstrap.Request{}, false, steadyErr
	}
	helperPlanned := false
	for _, action := range plan.Actions {
		if action == hostbootstrap.ResourceHelper {
			helperPlanned = true
		}
	}
	if !helperPlanned ||
		!reviewedPrincipalMatchesAccount(principal, request.Binding.Account.Name) {
		return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
	}
	raw, err := state.read(state.path("pending.json"))
	if err != nil {
		return hostbootstrap.Request{}, false, err
	}
	fields, err := strictObject(raw, map[string]bool{
		"schemaVersion":    true,
		"operationId":      true,
		"planSha256":       true,
		"resource":         true,
		"actionIndex":      true,
		"expectedPre":      true,
		"expectedPost":     true,
		"effectId":         true,
		"effectComplete":   true,
		"revision":         true,
		"priorMarker":      true,
		"priorMarkerFound": true,
	})
	if err != nil || len(fields) != 12 {
		return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
	}
	var pending startupPendingRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil ||
		pending.SchemaVersion != hostbootstrap.SchemaVersion ||
		pending.OperationID != request.OperationID ||
		!helperHex64.MatchString(pending.PlanSHA256) ||
		pending.Resource != hostbootstrap.ResourceHelper ||
		pending.ActionIndex < 0 ||
		pending.ActionIndex >= len(plan.Actions) ||
		plan.Actions[pending.ActionIndex] != pending.Resource ||
		!initialHelperStartReached(
			request.Binding.Target.Platform,
			pending.EffectID,
		) ||
		pending.EffectComplete ||
		pending.Revision == 0 ||
		pending.ExpectedPre.Validate(request.Binding.Target) != nil ||
		pending.ExpectedPost.Validate(request.Binding.Target) != nil {
		return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
	}
	planRaw, err := json.Marshal(plan)
	if err != nil {
		return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
	}
	sum := sha256.Sum256(planRaw)
	if hex.EncodeToString(sum[:]) != pending.PlanSHA256 {
		return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
	}
	expectedPre := plan.Observed
	for index := 0; index < pending.ActionIndex; index++ {
		expectedPre = helperObservationsAfterAction(
			expectedPre,
			request,
			plan.Actions[index],
		)
	}
	expectedPost := helperObservationsAfterAction(
		expectedPre,
		request,
		pending.Resource,
	)
	if !reflect.DeepEqual(pending.ExpectedPre, expectedPre) ||
		!reflect.DeepEqual(pending.ExpectedPost, expectedPost) ||
		(pending.PriorMarkerFound &&
			pending.PriorMarker.Kind != hostbootstrap.ResourceHelper) {
		return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
	}
	mode, err := classifyStartupLineage(pending)
	if err != nil {
		return hostbootstrap.Request{}, false, err
	}
	var lineageErr error
	if mode == startupLineageTransition {
		lineageErr = state.authorizeTransitionStartup(
			request,
			principal,
			pending,
		)
	} else {
		lineageErr = state.authorizeInitialStartup()
	}
	if lineageErr != nil {
		return hostbootstrap.Request{}, false, hostbootstrap.ErrHelperProtocol
	}
	if err := validateInitialHelperExecutable(
		request.Binding.Helper.Path,
		request.Binding.Helper.SHA256,
	); err != nil {
		return hostbootstrap.Request{}, false, err
	}
	return request, false, nil
}

func (state *diskHelperState) authorizeTransitionStartup(
	request hostbootstrap.Request,
	principal reviewedPrincipal,
	pending startupPendingRecord,
) error {
	markerRaw, markerErr := state.read(
		state.path("marker-" + string(hostbootstrap.ResourceHelper) + ".json"),
	)
	if markerErr != nil {
		return markerErr
	}
	var marker startupPriorMarker
	decoder := json.NewDecoder(bytes.NewReader(markerRaw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&marker) != nil ||
		!reflect.DeepEqual(marker, pending.PriorMarker) ||
		pending.ExpectedPre.Helper.Identity == nil ||
		marker.IdentitySHA256 !=
			pending.ExpectedPre.Helper.Identity.SHA256 ||
		pending.ExpectedPost.Helper.Identity == nil ||
		*pending.ExpectedPost.Helper.Identity != request.Binding.Helper {
		return hostbootstrap.ErrHelperProtocol
	}
	priorRaw, err := state.read(
		state.path("receipt-" + marker.OperationID + ".json"),
	)
	if err != nil {
		return err
	}
	prior, err := hostbootstrap.DecodeReceipt(priorRaw)
	if err != nil ||
		prior.OperationID != marker.OperationID ||
		!historicalMarkerAuthorized(
			request,
			prior,
			hostbootstrap.ResourceHelper,
			marker.IdentitySHA256,
		) ||
		!reviewedPrincipalMatchesAccount(
			principal,
			prior.Binding.Account.Name,
		) {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

func (state *diskHelperState) authorizeInitialStartup() error {
	_, markerErr := state.read(
		state.path("marker-" + string(hostbootstrap.ResourceHelper) + ".json"),
	)
	if markerErr == nil {
		return hostbootstrap.ErrHelperProtocol
	}
	if !errors.Is(markerErr, fs.ErrNotExist) {
		return markerErr
	}
	hasReceipts, err := state.hasHistoricalReceipts()
	if err != nil {
		return err
	}
	tombstoneRaw, tombstoneErr := state.read(
		state.path("uninstalled.json"),
	)
	if !hasReceipts {
		if errors.Is(tombstoneErr, fs.ErrNotExist) {
			return nil
		}
		return hostbootstrap.ErrHelperProtocol
	}
	if tombstoneErr != nil {
		return hostbootstrap.ErrHelperProtocol
	}
	return state.validateUninstallTombstone(tombstoneRaw)
}

func (state *diskHelperState) hasHistoricalReceipts() (bool, error) {
	entries, err := os.ReadDir(state.root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "receipt-") &&
			strings.HasSuffix(entry.Name(), ".json") {
			return true, nil
		}
	}
	return false, nil
}

func (state *diskHelperState) validateUninstallTombstone(raw []byte) error {
	fields, err := strictObject(raw, map[string]bool{
		"schemaVersion":              true,
		"receiptOperationId":         true,
		"receiptSha256":              true,
		"installationIdentitySha256": true,
		"confirmedAbsent":            true,
	})
	if err != nil || len(fields) != 5 {
		return hostbootstrap.ErrHelperProtocol
	}
	var tombstone struct {
		SchemaVersion              int                        `json:"schemaVersion"`
		ReceiptOperationID         string                     `json:"receiptOperationId"`
		ReceiptSHA256              string                     `json:"receiptSha256"`
		InstallationIdentitySHA256 string                     `json:"installationIdentitySha256"`
		ConfirmedAbsent            hostbootstrap.Observations `json:"confirmedAbsent"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&tombstone) != nil ||
		tombstone.SchemaVersion != hostbootstrap.SchemaVersion ||
		!helperHex32.MatchString(tombstone.ReceiptOperationID) ||
		!helperHex64.MatchString(tombstone.ReceiptSHA256) ||
		!helperHex64.MatchString(tombstone.InstallationIdentitySHA256) {
		return hostbootstrap.ErrHelperProtocol
	}
	receiptRaw, err := state.read(
		state.path("receipt-" + tombstone.ReceiptOperationID + ".json"),
	)
	if err != nil {
		return err
	}
	receipt, err := hostbootstrap.DecodeReceipt(receiptRaw)
	if err != nil {
		return hostbootstrap.ErrHelperProtocol
	}
	receiptSum := sha256.Sum256(receiptRaw)
	identity, err := helperInstallationIdentitySHA256(receipt.Binding)
	if err != nil ||
		hex.EncodeToString(receiptSum[:]) != tombstone.ReceiptSHA256 ||
		identity != tombstone.InstallationIdentitySHA256 ||
		tombstone.ConfirmedAbsent.Validate(receipt.Binding.Target) != nil ||
		!helperResourcesAbsent(tombstone.ConfirmedAbsent) {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

func helperInstallationIdentitySHA256(
	binding hostbootstrap.Binding,
) (string, error) {
	raw, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func helperResourcesAbsent(observed hostbootstrap.Observations) bool {
	return observed.SSHService.Ownership == hostbootstrap.OwnershipAbsent &&
		(observed.Firewall.Ownership == hostbootstrap.OwnershipAbsent ||
			observed.Firewall.Ownership ==
				hostbootstrap.OwnershipNotApplicable) &&
		observed.AuthorizedKey.Ownership == hostbootstrap.OwnershipAbsent &&
		observed.Helper.Ownership == hostbootstrap.OwnershipAbsent &&
		observed.Product.Ownership == hostbootstrap.OwnershipAbsent &&
		len(observed.RuntimeOwners) == 0
}

func initialHelperStartReached(
	platform hostbootstrap.Platform,
	effectID string,
) bool {
	if platform == hostbootstrap.PlatformLinux {
		return effectID == "apply:helper:command:2"
	}
	return effectID == "apply:helper:command:1"
}

func helperObservationsAfterAction(
	before hostbootstrap.Observations,
	request hostbootstrap.Request,
	action hostbootstrap.ResourceKind,
) hostbootstrap.Observations {
	after := before
	after.RuntimeOwners = append(
		[]hostbootstrap.RuntimeOwnerObservation(nil),
		before.RuntimeOwners...,
	)
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

func (state *diskHelperState) RunMutation(operation func() error) error {
	return state.mutations.Run(operation)
}

func (state *diskHelperState) path(name string) string {
	return helperStatePath(state.root, name)
}
