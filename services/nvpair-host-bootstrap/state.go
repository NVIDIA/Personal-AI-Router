// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"nvpair-shared/hostbootstrap"
)

const (
	rootOwner            = "root"
	maxStateFileBytes    = 16 << 20
	operationStateFile   = "operation.json"
	principalStateFile   = "principal.json"
	pendingStateFile     = "pending.json"
	uninstallPendingFile = "uninstall-pending.json"
	uninstalledStateFile = "uninstalled.json"
)

var stateHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
var stateHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

type secureMetadata struct {
	Regular bool
	Symlink bool
	Reparse bool
	Links   uint64
	Mode    fs.FileMode
	Owner   string
}

type secureFileSystem interface {
	EnsurePrivateDirectory(string) error
	ReadNoFollow(string, int64) ([]byte, secureMetadata, error)
	AtomicWriteNoFollow(string, []byte, fs.FileMode) error
	RemoveNoFollow(string) error
}

type resourceMarker struct {
	SchemaVersion     int                        `json:"schemaVersion"`
	Kind              hostbootstrap.ResourceKind `json:"kind"`
	OperationID       string                     `json:"operationId"`
	IdentitySHA256    string                     `json:"identitySha256"`
	AuthorizedKeyLine string                     `json:"authorizedKeyLine"`
	FirewallFamilies  []string                   `json:"firewallFamilies"`
	Files             []payloadManifestFile      `json:"files"`
}

type reviewedPrincipalState struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
	SID string `json:"sid"`
}

type operationEnvelope struct {
	Request hostbootstrap.Request `json:"request"`
	Plan    hostbootstrap.Plan    `json:"plan"`
}

type uninstallTombstone struct {
	SchemaVersion              int                        `json:"schemaVersion"`
	ReceiptOperationID         string                     `json:"receiptOperationId"`
	ReceiptSHA256              string                     `json:"receiptSha256"`
	InstallationIdentitySHA256 string                     `json:"installationIdentitySha256"`
	ConfirmedAbsent            hostbootstrap.Observations `json:"confirmedAbsent"`
}

type diskState struct {
	root        string
	filesystem  secureFileSystem
	mutationMu  sync.Mutex
	acquireLock func() (func() error, error)
}

func newDiskState(root string, filesystem secureFileSystem) *diskState {
	return &diskState{
		root:       root,
		filesystem: filesystem,
		acquireLock: func() (func() error, error) {
			return acquireBootstrapOperationLock(root)
		},
	}
}

func (state *diskState) RunMutation(operation func() error) error {
	state.mutationMu.Lock()
	defer state.mutationMu.Unlock()
	if state.acquireLock == nil {
		return ErrStateIdentity
	}
	release, err := state.acquireLock()
	if err != nil {
		return err
	}
	if release == nil {
		return ErrStateIdentity
	}
	operationErr := operation()
	releaseErr := release()
	if operationErr != nil {
		return operationErr
	}
	return releaseErr
}

func (state *diskState) SaveOperation(
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	if request.OperationID != plan.OperationID || request.Binding != plan.Binding {
		return ErrStateIdentity
	}
	if err := state.filesystem.EnsurePrivateDirectory(state.root); err != nil {
		return err
	}
	existingRequest, existingPlan, err := state.LoadOperation()
	switch {
	case err == nil:
		if existingRequest == request && reflect.DeepEqual(existingPlan, plan) {
			return nil
		}
		if existingRequest.OperationID == request.OperationID {
			return ErrStateIdentity
		}
		if _, err := state.loadReceiptFor(existingRequest.OperationID); err != nil {
			return ErrStateIdentity
		}
	case !errors.Is(err, ErrStateMissing):
		return err
	}
	raw, err := json.Marshal(operationEnvelope{
		Request: request,
		Plan:    plan,
	})
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(
		state.path(operationStateFile),
		raw,
		0600,
	)
}

func (state *diskState) SaveRepairOperation(
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	if request.OperationID != plan.OperationID ||
		request.Binding != plan.Binding ||
		(plan.Decision != hostbootstrap.DecisionRepairOwned &&
			plan.Decision != hostbootstrap.DecisionNoOp) {
		return ErrStateIdentity
	}
	existingRequest, existingPlan, err := state.LoadOperation()
	if err != nil ||
		existingRequest != request ||
		existingPlan.OperationID != plan.OperationID ||
		existingPlan.Binding != plan.Binding {
		return ErrStateIdentity
	}
	if reflect.DeepEqual(existingPlan, plan) {
		return nil
	}
	receipt, err := state.loadReceiptFor(request.OperationID)
	if err != nil ||
		receipt.OperationID != request.OperationID ||
		receipt.Binding != request.Binding {
		return ErrStateIdentity
	}
	if _, err := state.LoadPending(); err == nil {
		return ErrStateIdentity
	} else if !errors.Is(err, ErrStateMissing) {
		return err
	}
	raw, err := json.Marshal(operationEnvelope{
		Request: request,
		Plan:    plan,
	})
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(
		state.path(operationStateFile),
		raw,
		0600,
	)
}

func (state *diskState) LoadOperation() (
	hostbootstrap.Request,
	hostbootstrap.Plan,
	error,
) {
	raw, err := state.read(operationStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, ErrStateMissing
	}
	if err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, err
	}
	fields, err := decodeExactObject(raw, map[string]bool{
		"request": true,
		"plan":    true,
	})
	if err != nil || len(fields) != 2 {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, ErrUnsafeState
	}
	var encoded struct {
		Request json.RawMessage `json:"request"`
		Plan    json.RawMessage `json:"plan"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&encoded); err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, ErrUnsafeState
	}
	request, err := hostbootstrap.DecodeRequest(encoded.Request)
	if err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, ErrUnsafeState
	}
	plan, err := hostbootstrap.DecodePlan(encoded.Plan)
	if err != nil {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, ErrUnsafeState
	}
	if request.OperationID != plan.OperationID || request.Binding != plan.Binding {
		return hostbootstrap.Request{}, hostbootstrap.Plan{}, ErrStateIdentity
	}
	return request, plan, nil
}

func (state *diskState) SaveReceipt(receipt hostbootstrap.Receipt) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := state.filesystem.EnsurePrivateDirectory(state.root); err != nil {
		return err
	}
	request, plan, err := state.LoadOperation()
	if err != nil ||
		receipt.OperationID != request.OperationID ||
		receipt.Binding != request.Binding ||
		receipt.Decision != plan.Decision {
		return ErrReceiptImmutable
	}
	existing, err := state.loadReceiptFor(receipt.OperationID)
	switch {
	case err == nil:
		if !reflect.DeepEqual(existing, receipt) {
			return ErrReceiptImmutable
		}
		return state.removeUninstalledTombstone()
	case !errors.Is(err, ErrStateMissing):
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := state.filesystem.AtomicWriteNoFollow(
		state.receiptPath(receipt.OperationID),
		raw,
		0600,
	); err != nil {
		return err
	}
	return state.removeUninstalledTombstone()
}

func (state *diskState) removeUninstalledTombstone() error {
	err := state.filesystem.RemoveNoFollow(state.path(uninstalledStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (state *diskState) LoadReceipt() (hostbootstrap.Receipt, error) {
	request, _, err := state.LoadOperation()
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	return state.loadReceiptFor(request.OperationID)
}

func (state *diskState) SaveUninstalled(
	receipt hostbootstrap.Receipt,
	observed hostbootstrap.Observations,
) error {
	if err := receipt.Validate(); err != nil ||
		observed.Validate(receipt.Binding.Target) != nil {
		return ErrStateIdentity
	}
	for _, kind := range uninstallOrder {
		if !resourceRemoved(observed, kind) {
			return ErrStateIdentity
		}
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	receiptSum := sha256.Sum256(receiptRaw)
	identity, err := installationIdentitySHA256(receipt.Binding)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(uninstallTombstone{
		SchemaVersion:              hostbootstrap.SchemaVersion,
		ReceiptOperationID:         receipt.OperationID,
		ReceiptSHA256:              hex.EncodeToString(receiptSum[:]),
		InstallationIdentitySHA256: identity,
		ConfirmedAbsent:            observed,
	})
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(
		state.path(uninstalledStateFile),
		raw,
		0600,
	)
}

func installationIdentitySHA256(
	binding hostbootstrap.Binding,
) (string, error) {
	raw, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (state *diskState) SavePending(pending pendingEffect) error {
	if err := state.validatePending(pending); err != nil {
		return err
	}
	existing, err := state.LoadPending()
	switch {
	case err == nil:
		if !reflect.DeepEqual(existing, pending) {
			return ErrStateIdentity
		}
		return nil
	case !errors.Is(err, ErrStateMissing):
		return err
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(state.path(pendingStateFile), raw, 0600)
}

func (state *diskState) UpdatePending(
	expected pendingEffect,
	next pendingEffect,
) error {
	if err := state.validatePending(expected); err != nil {
		return err
	}
	if err := state.validatePending(next); err != nil {
		return err
	}
	existing, err := state.LoadPending()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing, expected) ||
		!pendingTransitionValid(expected, next) {
		return ErrStateIdentity
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(state.path(pendingStateFile), raw, 0600)
}

func (state *diskState) AdoptPending(
	expected pendingEffect,
	next pendingEffect,
) error {
	if err := state.validatePending(expected); err != nil {
		return err
	}
	if err := state.validatePending(next); err != nil {
		return err
	}
	existing, err := state.LoadPending()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing, expected) ||
		next.Revision != expected.Revision+1 ||
		next.EffectID != "role:marker-advance" ||
		!next.EffectComplete ||
		!pendingIdentityEqual(expected, next) {
		return ErrStateIdentity
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(
		state.path(pendingStateFile),
		raw,
		0600,
	)
}

func pendingIdentityEqual(left, right pendingEffect) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.OperationID == right.OperationID &&
		left.PlanSHA256 == right.PlanSHA256 &&
		left.Resource == right.Resource &&
		left.ActionIndex == right.ActionIndex &&
		reflect.DeepEqual(left.ExpectedPre, right.ExpectedPre) &&
		reflect.DeepEqual(left.ExpectedPost, right.ExpectedPost) &&
		reflect.DeepEqual(left.PriorMarker, right.PriorMarker) &&
		left.PriorMarkerFound == right.PriorMarkerFound
}

func pendingTransitionValid(expected, next pendingEffect) bool {
	if !pendingIdentityEqual(expected, next) ||
		next.Revision != expected.Revision+1 {
		return false
	}
	if expected.EffectID == next.EffectID {
		return expected.EffectID != "" &&
			!expected.EffectComplete &&
			next.EffectComplete
	}
	return next.EffectID != "" &&
		!next.EffectComplete &&
		(expected.EffectID == "" || expected.EffectComplete)
}

func (state *diskState) LoadPending() (pendingEffect, error) {
	raw, err := state.read(pendingStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return pendingEffect{}, ErrStateMissing
	}
	if err != nil {
		return pendingEffect{}, err
	}
	fields, err := decodeExactObject(raw, map[string]bool{
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
		return pendingEffect{}, ErrUnsafeState
	}
	var pending pendingEffect
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		return pendingEffect{}, ErrUnsafeState
	}
	if err := state.validatePending(pending); err != nil {
		return pendingEffect{}, err
	}
	return pending, nil
}

func (state *diskState) ClearPending() error {
	if _, err := state.LoadPending(); errors.Is(err, ErrStateMissing) {
		return nil
	} else if err != nil {
		return err
	}
	return state.filesystem.RemoveNoFollow(state.path(pendingStateFile))
}

func (state *diskState) SaveUninstallPending(pending uninstallEffect) error {
	if err := state.validateUninstallPending(pending); err != nil {
		return err
	}
	existing, err := state.LoadUninstallPending()
	if err == nil {
		if reflect.DeepEqual(existing, pending) {
			return nil
		}
		return ErrStateIdentity
	}
	if !errors.Is(err, ErrStateMissing) {
		return err
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(
		state.path(uninstallPendingFile),
		raw,
		0600,
	)
}

func (state *diskState) UpdateUninstallPending(
	expected uninstallEffect,
	next uninstallEffect,
) error {
	if err := state.validateUninstallPending(expected); err != nil {
		return err
	}
	if err := state.validateUninstallPending(next); err != nil {
		return err
	}
	existing, err := state.LoadUninstallPending()
	if err != nil ||
		!reflect.DeepEqual(existing, expected) ||
		!uninstallPendingTransitionValid(expected, next) {
		return ErrStateIdentity
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(
		state.path(uninstallPendingFile),
		raw,
		0600,
	)
}

func (state *diskState) LoadUninstallPending() (uninstallEffect, error) {
	raw, err := state.read(uninstallPendingFile)
	if errors.Is(err, fs.ErrNotExist) {
		return uninstallEffect{}, ErrStateMissing
	}
	if err != nil {
		return uninstallEffect{}, err
	}
	fields, err := decodeExactObject(raw, map[string]bool{
		"schemaVersion":  true,
		"operationId":    true,
		"resource":       true,
		"actionIndex":    true,
		"expectedPre":    true,
		"expectedPost":   true,
		"effectId":       true,
		"effectComplete": true,
		"revision":       true,
		"priorMarker":    true,
	})
	if err != nil || len(fields) != 10 {
		return uninstallEffect{}, ErrUnsafeState
	}
	var pending uninstallEffect
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil {
		return uninstallEffect{}, ErrUnsafeState
	}
	if err := state.validateUninstallPending(pending); err != nil {
		return uninstallEffect{}, err
	}
	return pending, nil
}

func (state *diskState) ClearUninstallPending() error {
	if _, err := state.LoadUninstallPending(); errors.Is(err, ErrStateMissing) {
		return nil
	} else if err != nil {
		return err
	}
	return state.filesystem.RemoveNoFollow(state.path(uninstallPendingFile))
}

func (state *diskState) validateUninstallPending(
	pending uninstallEffect,
) error {
	request, _, err := state.LoadOperation()
	if err != nil ||
		pending.SchemaVersion != hostbootstrap.SchemaVersion ||
		pending.OperationID != request.OperationID ||
		pending.ActionIndex < 0 ||
		pending.ActionIndex >= len(uninstallOrder) ||
		pending.Resource != uninstallOrder[pending.ActionIndex] ||
		pending.PriorMarker.Kind != pending.Resource ||
		pending.PriorMarker.validate() != nil ||
		len(pending.EffectID) > 128 ||
		strings.ContainsAny(pending.EffectID, "\r\n") ||
		(pending.EffectID == "" && pending.EffectComplete) ||
		pending.ExpectedPre.Validate(request.Binding.Target) != nil ||
		pending.ExpectedPost.Validate(request.Binding.Target) != nil {
		return ErrStateIdentity
	}
	return nil
}

func uninstallPendingTransitionValid(
	expected uninstallEffect,
	next uninstallEffect,
) bool {
	if expected.SchemaVersion != next.SchemaVersion ||
		expected.OperationID != next.OperationID ||
		expected.Resource != next.Resource ||
		expected.ActionIndex != next.ActionIndex ||
		!reflect.DeepEqual(expected.ExpectedPre, next.ExpectedPre) ||
		!reflect.DeepEqual(expected.ExpectedPost, next.ExpectedPost) ||
		!reflect.DeepEqual(expected.PriorMarker, next.PriorMarker) ||
		next.Revision != expected.Revision+1 {
		return false
	}
	if expected.EffectID == next.EffectID {
		return expected.EffectID != "" &&
			!expected.EffectComplete &&
			next.EffectComplete
	}
	return next.EffectID != "" &&
		!next.EffectComplete &&
		(expected.EffectID == "" || expected.EffectComplete)
}

func (state *diskState) validatePending(pending pendingEffect) error {
	request, plan, err := state.LoadOperation()
	if err != nil {
		return err
	}
	digest, err := digestPlan(plan)
	if err != nil {
		return err
	}
	if pending.SchemaVersion != hostbootstrap.SchemaVersion ||
		pending.OperationID != request.OperationID ||
		pending.PlanSHA256 != digest ||
		pending.ActionIndex < 0 ||
		pending.ActionIndex >= len(plan.Actions) ||
		pending.Resource != plan.Actions[pending.ActionIndex] ||
		len(pending.EffectID) > 128 ||
		strings.ContainsAny(pending.EffectID, "\r\n") ||
		(pending.EffectID == "" && pending.EffectComplete) ||
		(pending.PriorMarkerFound &&
			(pending.PriorMarker.Kind != pending.Resource ||
				pending.PriorMarker.validate() != nil)) ||
		(!pending.PriorMarkerFound &&
			!reflect.DeepEqual(pending.PriorMarker, resourceMarker{})) ||
		pending.ExpectedPre.Validate(request.Binding.Target) != nil ||
		pending.ExpectedPost.Validate(request.Binding.Target) != nil {
		return ErrStateIdentity
	}
	return nil
}

func (state *diskState) loadReceiptFor(operationID string) (hostbootstrap.Receipt, error) {
	if !stateHex32.MatchString(operationID) {
		return hostbootstrap.Receipt{}, ErrStateIdentity
	}
	raw, err := state.read("receipt-" + operationID + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return hostbootstrap.Receipt{}, ErrStateMissing
	}
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	receipt, err := hostbootstrap.DecodeReceipt(raw)
	if err != nil {
		return hostbootstrap.Receipt{}, ErrUnsafeState
	}
	return receipt, nil
}

func (state *diskState) SavePrincipal(principal reviewedPrincipalState) error {
	if principal.SID == "" && principal.UID == 0 && principal.GID == 0 {
		return ErrStateIdentity
	}
	existing, err := state.LoadPrincipal()
	switch {
	case err == nil:
		if existing != principal {
			return ErrStateIdentity
		}
		return nil
	case !errors.Is(err, ErrStateMissing):
		return err
	}
	raw, err := json.Marshal(principal)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(state.path(principalStateFile), raw, 0600)
}

func (state *diskState) LoadPrincipal() (reviewedPrincipalState, error) {
	raw, err := state.read(principalStateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return reviewedPrincipalState{}, ErrStateMissing
	}
	if err != nil {
		return reviewedPrincipalState{}, err
	}
	fields, err := decodeExactObject(raw, map[string]bool{
		"uid": true,
		"gid": true,
		"sid": true,
	})
	if err != nil || len(fields) != 3 {
		return reviewedPrincipalState{}, ErrUnsafeState
	}
	var principal reviewedPrincipalState
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&principal); err != nil ||
		(principal.SID == "" && principal.UID == 0 && principal.GID == 0) {
		return reviewedPrincipalState{}, ErrUnsafeState
	}
	return principal, nil
}

func (state *diskState) SaveMarker(marker resourceMarker) error {
	if err := marker.validate(); err != nil {
		return err
	}
	if err := state.filesystem.EnsurePrivateDirectory(state.root); err != nil {
		return err
	}
	existing, found, err := state.LoadMarker(marker.Kind)
	if err != nil {
		return err
	}
	if found {
		if !reflect.DeepEqual(existing, marker) {
			return ErrStateIdentity
		}
		return nil
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(state.markerPath(marker.Kind), raw, 0600)
}

func (state *diskState) ReplaceMarker(
	expected resourceMarker,
	next resourceMarker,
) error {
	if err := expected.validate(); err != nil {
		return err
	}
	if err := next.validate(); err != nil {
		return err
	}
	existing, found, err := state.LoadMarker(expected.Kind)
	if err != nil {
		return err
	}
	if !found ||
		!reflect.DeepEqual(existing, expected) ||
		expected.Kind != next.Kind {
		return ErrStateIdentity
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return state.filesystem.AtomicWriteNoFollow(
		state.markerPath(next.Kind),
		raw,
		0600,
	)
}

func (state *diskState) LoadMarker(
	kind hostbootstrap.ResourceKind,
) (resourceMarker, bool, error) {
	if !validResourceKind(kind) {
		return resourceMarker{}, false, ErrStateIdentity
	}
	raw, err := state.read("marker-" + string(kind) + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return resourceMarker{}, false, nil
	}
	if err != nil {
		return resourceMarker{}, false, err
	}
	marker, err := decodeResourceMarker(raw)
	if err != nil || marker.Kind != kind {
		return resourceMarker{}, false, ErrStateIdentity
	}
	return marker, true, nil
}

func (state *diskState) RemoveMarker(kind hostbootstrap.ResourceKind) error {
	marker, found, err := state.LoadMarker(kind)
	if err != nil {
		return err
	}
	if !found || marker.Kind != kind {
		return nil
	}
	return state.filesystem.RemoveNoFollow(state.markerPath(kind))
}

func (state *diskState) RemoveMarkerCAS(expected resourceMarker) error {
	if err := expected.validate(); err != nil {
		return err
	}
	existing, found, err := state.LoadMarker(expected.Kind)
	if err != nil {
		return err
	}
	if !found || !reflect.DeepEqual(existing, expected) {
		return ErrStateIdentity
	}
	return state.filesystem.RemoveNoFollow(
		state.markerPath(expected.Kind),
	)
}

func (state *diskState) RemoveOperation() error {
	for _, kind := range uninstallOrder {
		if err := state.RemoveMarker(kind); err != nil {
			return err
		}
	}
	for _, name := range []string{
		principalStateFile,
		pendingStateFile,
		uninstallPendingFile,
		operationStateFile,
	} {
		if err := state.filesystem.RemoveNoFollow(
			state.path(name),
		); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (state *diskState) read(name string) ([]byte, error) {
	raw, metadata, err := state.filesystem.ReadNoFollow(state.path(name), maxStateFileBytes)
	if err != nil {
		return nil, err
	}
	if !metadata.Regular ||
		metadata.Symlink ||
		metadata.Reparse ||
		metadata.Links != 1 ||
		metadata.Mode.Perm() != 0600 ||
		metadata.Owner != rootOwner {
		return nil, ErrUnsafeState
	}
	return raw, nil
}

func (state *diskState) markerPath(kind hostbootstrap.ResourceKind) string {
	return state.path("marker-" + string(kind) + ".json")
}

func (state *diskState) receiptPath(operationID string) string {
	return state.path("receipt-" + operationID + ".json")
}

func (state *diskState) path(name string) string {
	separator := "/"
	if strings.Contains(state.root, `\`) {
		separator = `\`
	}
	return strings.TrimRight(state.root, `/\`) + separator + name
}

func (marker resourceMarker) validate() error {
	if marker.SchemaVersion != hostbootstrap.SchemaVersion ||
		!validResourceKind(marker.Kind) ||
		!stateHex32.MatchString(marker.OperationID) ||
		!stateHex64.MatchString(marker.IdentitySHA256) {
		return ErrStateIdentity
	}
	if marker.Kind == hostbootstrap.ResourceProduct {
		if len(marker.Files) == 0 {
			return ErrStateIdentity
		}
		seen := make(map[string]bool)
		for _, file := range marker.Files {
			if !canonicalArchivePath(file.Path) ||
				seen[strings.ToLower(file.Path)] {
				return ErrStateIdentity
			}
			seen[strings.ToLower(file.Path)] = true
		}
	} else if len(marker.Files) != 0 {
		return ErrStateIdentity
	}
	if marker.Kind == hostbootstrap.ResourceAuthorizedKey {
		fields := strings.SplitN(marker.AuthorizedKeyLine, " ", 3)
		if len(fields) != 3 ||
			!strings.HasPrefix(fields[2], ownedKeyCommentPrefix) {
			return ErrStateIdentity
		}
		blob, err := base64.StdEncoding.Strict().DecodeString(fields[1])
		if err != nil {
			return ErrStateIdentity
		}
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) != marker.IdentitySHA256 {
			return ErrStateIdentity
		}
	} else if marker.AuthorizedKeyLine != "" {
		return ErrStateIdentity
	}
	if marker.Kind == hostbootstrap.ResourceFirewall {
		if len(marker.FirewallFamilies) > 2 {
			return ErrStateIdentity
		}
		for index, family := range marker.FirewallFamilies {
			if (family != "ipv4" && family != "ipv6") ||
				(index > 0 &&
					marker.FirewallFamilies[index-1] >= family) {
				return ErrStateIdentity
			}
		}
	} else if len(marker.FirewallFamilies) != 0 {
		return ErrStateIdentity
	}
	return nil
}

func validResourceKind(kind hostbootstrap.ResourceKind) bool {
	switch kind {
	case hostbootstrap.ResourceSSHService,
		hostbootstrap.ResourceFirewall,
		hostbootstrap.ResourceAuthorizedKey,
		hostbootstrap.ResourceHelper,
		hostbootstrap.ResourceProduct,
		hostbootstrap.ResourceActiveRole:
		return true
	default:
		return false
	}
}

func decodeResourceMarker(raw []byte) (resourceMarker, error) {
	var marker resourceMarker
	allowed := map[string]bool{
		"schemaVersion":     true,
		"kind":              true,
		"operationId":       true,
		"identitySha256":    true,
		"authorizedKeyLine": true,
		"firewallFamilies":  true,
		"files":             true,
	}
	seen, err := decodeExactObject(raw, allowed)
	if err != nil {
		return marker, ErrStateIdentity
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, ErrStateIdentity
	}
	if len(seen) != len(allowed) || marker.validate() != nil {
		return marker, ErrStateIdentity
	}
	return marker, nil
}

func decodeExactObject(
	raw []byte,
	allowed map[string]bool,
) (map[string]bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrStateIdentity
	}
	seen := make(map[string]bool)
	for decoder.More() {
		fieldToken, fieldErr := decoder.Token()
		field, ok := fieldToken.(string)
		if fieldErr != nil || !ok || !allowed[field] || seen[field] {
			return nil, ErrStateIdentity
		}
		seen[field] = true
		var discarded json.RawMessage
		if err := decoder.Decode(&discarded); err != nil {
			return nil, ErrStateIdentity
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, ErrStateIdentity
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrStateIdentity
	}
	return seen, nil
}
