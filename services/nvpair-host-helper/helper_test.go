// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"nvpair-shared/hostbootstrap"
)

func validBootstrapHelperRequest(
	action hostbootstrap.HelperAction,
) hostbootstrap.HelperRequest {
	return validBootstrapHelperRequestForOperation(
		action,
		strings.Repeat("ab", 16),
	)
}

func validBootstrapHelperRequestForOperation(
	action hostbootstrap.HelperAction,
	operationID string,
) hostbootstrap.HelperRequest {
	return hostbootstrap.HelperRequest{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   operationID,
		Action:        action,
	}
}

func validRankHelperRequest() hostbootstrap.HelperRequest {
	return hostbootstrap.HelperRequest{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   strings.Repeat("ab", 16),
		Action:        hostbootstrap.HelperActionRankReconcile,
		RankReconcile: &hostbootstrap.RankReconcileRequest{
			RunID:      strings.Repeat("cd", 16),
			Generation: 7,
			Rank:       3,
			PlanDigest: strings.Repeat("12", 32),
			NodeID:     "node-01",
		},
	}
}

func TestDecodeHelperRequestIsStrictBoundedAndClosed(t *testing.T) {
	valid := []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"inspect"}`)
	request, err := hostbootstrap.DecodeHelperRequest(valid)
	if err != nil {
		t.Fatalf("DecodeHelperRequest(valid) error = %v", err)
	}
	if request.Action != hostbootstrap.HelperActionInspect {
		t.Fatalf("action = %q", request.Action)
	}

	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "unknown", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"inspect","extra":true}`)},
		{name: "duplicate", raw: []byte(`{"schemaVersion":1,"schemaVersion":1,"operationId":"abababababababababababababababab","action":"inspect"}`)},
		{name: "case-alias", raw: []byte(`{"SchemaVersion":1,"operationId":"abababababababababababababababab","action":"inspect"}`)},
		{name: "free-command", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"inspect","command":"whoami"}`)},
		{name: "path", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"apply","path":"/tmp/owned"}`)},
		{name: "argv", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"apply","argv":["x"]}`)},
		{name: "password", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"apply","password":"secret"}`)},
		{name: "token", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"apply","token":"secret"}`)},
		{name: "private-key", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"apply","privateKey":"secret"}`)},
		{name: "rank-on-bootstrap", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"verify","rankReconcile":{"runId":"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd","generation":1,"rank":0,"planDigest":"1212121212121212121212121212121212121212121212121212121212121212","nodeId":"node-01"}}`)},
		{name: "missing-rank", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"rank-reconcile"}`)},
		{name: "missing-explicit-zero-rank", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"rank-reconcile","rankReconcile":{"runId":"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd","generation":1,"planDigest":"1212121212121212121212121212121212121212121212121212121212121212","nodeId":"node-01"}}`)},
		{name: "negative-rank", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"rank-reconcile","rankReconcile":{"runId":"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd","generation":1,"rank":-1,"planDigest":"1212121212121212121212121212121212121212121212121212121212121212","nodeId":"node-01"}}`)},
		{name: "duplicate-rank-field", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"rank-reconcile","rankReconcile":{"runId":"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd","generation":1,"rank":0,"rank":1,"planDigest":"1212121212121212121212121212121212121212121212121212121212121212","nodeId":"node-01"}}`)},
		{name: "bad-node", raw: []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"rank-reconcile","rankReconcile":{"runId":"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd","generation":1,"rank":0,"planDigest":"1212121212121212121212121212121212121212121212121212121212121212","nodeId":"../../escape"}}`)},
		{name: "oversized", raw: bytes.Repeat([]byte("x"), hostbootstrap.MaxHelperFrameBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := hostbootstrap.DecodeHelperRequest(test.raw); err == nil {
				t.Fatal("DecodeHelperRequest() error = nil")
			}
		})
	}

	rank := validRankHelperRequest()
	raw, err := hostbootstrap.EncodeHelperRequest(rank)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hostbootstrap.DecodeHelperRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, rank) {
		t.Fatalf("rank request = %#v, want %#v", decoded, rank)
	}
}

func TestFrameRejectsOversizedTruncatedAndTrailingPayloads(t *testing.T) {
	var framed bytes.Buffer
	if err := hostbootstrap.WriteHelperFrame(&framed, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	got, err := hostbootstrap.ReadHelperFrame(&framed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Fatalf("frame = %q", got)
	}

	oversized := make([]byte, 4)
	binary.BigEndian.PutUint32(oversized, hostbootstrap.MaxHelperFrameBytes+1)
	if _, err := hostbootstrap.ReadHelperFrame(bytes.NewReader(oversized)); !errors.Is(err, hostbootstrap.ErrHelperFrameTooLarge) {
		t.Fatalf("oversized frame error = %v", err)
	}

	truncated := make([]byte, 4)
	binary.BigEndian.PutUint32(truncated, 12)
	truncated = append(truncated, []byte("short")...)
	if _, err := hostbootstrap.ReadHelperFrame(bytes.NewReader(truncated)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame error = %v", err)
	}
	if err := hostbootstrap.WriteHelperFrame(io.Discard, bytes.Repeat([]byte("x"), hostbootstrap.MaxHelperFrameBytes+1)); !errors.Is(err, hostbootstrap.ErrHelperFrameTooLarge) {
		t.Fatalf("write oversized error = %v", err)
	}
}

type fakeAuthorizer struct {
	err    error
	called bool
}

func (authorizer *fakeAuthorizer) Authorize(net.Conn, reviewedPrincipal) error {
	authorizer.called = true
	return authorizer.err
}

type fakeHelperState struct {
	principal reviewedPrincipal
	request   hostbootstrap.Request
	plan      hostbootstrap.Plan
	receipt   hostbootstrap.Receipt
	markers   ownedResourceSet
	mutations mutationCoordinator
	pending   bool
	loads     int
}

func (state *fakeHelperState) LoadPrincipal() (reviewedPrincipal, error) {
	return state.principal, nil
}

func (state *fakeHelperState) LoadOperation() (hostbootstrap.Request, hostbootstrap.Plan, error) {
	state.loads++
	return state.request, state.plan, nil
}

func (state *fakeHelperState) LoadRepairAuthorization(
	hostbootstrap.Request,
) (hostbootstrap.Receipt, ownedResourceSet, error) {
	if state.receipt.OperationID != "" {
		return state.receipt, state.markers, nil
	}
	exact := exactHelperObservations(state.request)
	return hostbootstrap.Receipt{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   state.request.OperationID,
		Phase:         hostbootstrap.PhaseComplete,
		Decision:      state.plan.Decision,
		Binding:       state.request.Binding,
		Verified:      exact,
	}, testOwnedMarkers(state.request), nil
}

func (state *fakeHelperState) HasPending() (bool, error) {
	return state.pending, nil
}

func (state *fakeHelperState) RunMutation(operation func() error) error {
	if state.mutations.acquire == nil {
		state.mutations.acquire = func() (func() error, error) {
			return func() error { return nil }, nil
		}
	}
	return state.mutations.Run(operation)
}

type fakeHelperExecutor struct {
	actions     []hostbootstrap.HelperAction
	plans       []hostbootstrap.Plan
	observed    hostbootstrap.Observations
	inspections int
}

func (executor *fakeHelperExecutor) Inspect(
	_ context.Context,
	request hostbootstrap.Request,
) (hostbootstrap.Status, error) {
	executor.actions = append(
		executor.actions,
		hostbootstrap.HelperActionInspect,
	)
	executor.inspections++
	observed := executor.observed
	if observed.RuntimeOwners == nil {
		observed = exactHelperObservations(request)
	}
	return hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         hostbootstrap.PhaseInspect,
		Binding:       request.Binding,
		Observed:      observed,
	}, nil
}

func (executor *fakeHelperExecutor) Apply(
	_ context.Context,
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) (hostbootstrap.Status, error) {
	executor.actions = append(
		executor.actions,
		hostbootstrap.HelperActionApply,
	)
	executor.plans = append(executor.plans, plan)
	phase := hostbootstrap.PhaseApply
	observed := plan.Observed
	if plan.Decision == hostbootstrap.DecisionNoOp {
		phase = hostbootstrap.PhaseVerify
		observed = exactHelperObservations(request)
	}
	return hostbootstrap.Status{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         phase,
		Decision:      plan.Decision,
		Binding:       request.Binding,
		Observed:      observed,
	}, nil
}

func (executor *fakeHelperExecutor) Verify(
	_ context.Context,
	request hostbootstrap.Request,
	plan hostbootstrap.Plan,
) (hostbootstrap.Receipt, error) {
	executor.actions = append(
		executor.actions,
		hostbootstrap.HelperActionVerify,
	)
	executor.plans = append(executor.plans, plan)
	receipt, err := hostbootstrap.NewReceipt(
		plan,
		exactHelperObservations(request),
	)
	if err != nil {
		return hostbootstrap.Receipt{}, err
	}
	return receipt, nil
}

func helperPipeExchange(
	t *testing.T,
	request hostbootstrap.HelperRequest,
	authorizer *fakeAuthorizer,
	state *fakeHelperState,
	executor *fakeHelperExecutor,
) (hostbootstrap.HelperResponse, error) {
	t.Helper()
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- handleConnection(context.Background(), server, authorizer, state, executor)
	}()
	raw, err := hostbootstrap.EncodeHelperRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- hostbootstrap.WriteHelperFrame(client, raw)
	}()
	responseRaw, readErr := hostbootstrap.ReadHelperFrame(client)
	_ = client.Close()
	if readErr != nil {
		return hostbootstrap.HelperResponse{}, readErr
	}
	<-writeDone
	response, decodeErr := hostbootstrap.DecodeHelperResponse(responseRaw)
	select {
	case serverErr := <-done:
		if serverErr != nil {
			return response, serverErr
		}
	case <-time.After(time.Second):
		t.Fatal("server did not complete")
	}
	return response, decodeErr
}

func TestHelperAuthorizesPeerBeforeLoadingStateOrDispatch(t *testing.T) {
	authorizer := &fakeAuthorizer{err: ErrPeerUnauthorized}
	state := &fakeHelperState{}
	executor := &fakeHelperExecutor{}
	response, err := helperPipeExchange(
		t,
		validBootstrapHelperRequest(hostbootstrap.HelperActionApply),
		authorizer,
		state,
		executor,
	)
	if err == nil {
		t.Fatal("unauthorized exchange error = nil")
	}
	if response.Accepted || !authorizer.called || state.loads != 0 || len(executor.actions) != 0 {
		t.Fatalf("unauthorized result response=%#v loads=%d actions=%#v", response, state.loads, executor.actions)
	}
}

func TestBootstrapActionsLoadOnlyFixedReviewedState(t *testing.T) {
	request, plan := helperTestOperation(t)
	for _, action := range []hostbootstrap.HelperAction{
		hostbootstrap.HelperActionInspect,
		hostbootstrap.HelperActionApply,
		hostbootstrap.HelperActionVerify,
	} {
		t.Run(string(action), func(t *testing.T) {
			authorizer := &fakeAuthorizer{}
			state := &fakeHelperState{
				principal: reviewedPrincipal{UID: 1000, GID: 1000, SID: "S-1-5-21-1000"},
				request:   request,
				plan:      plan,
			}
			executor := &fakeHelperExecutor{}
			response, err := helperPipeExchange(t, validBootstrapHelperRequest(action), authorizer, state, executor)
			if err != nil {
				t.Fatalf("exchange error = %v", err)
			}
			expected := []hostbootstrap.HelperAction{action}
			if action == hostbootstrap.HelperActionApply {
				expected = []hostbootstrap.HelperAction{
					hostbootstrap.HelperActionInspect,
					hostbootstrap.HelperActionApply,
				}
			}
			if !response.Accepted || state.loads != 1 ||
				!reflect.DeepEqual(executor.actions, expected) {
				t.Fatalf("response=%#v loads=%d actions=%#v", response, state.loads, executor.actions)
			}
		})
	}
}

func TestHelperResponsesCarryOnlyTargetProducedTypedResults(t *testing.T) {
	request, plan := helperTestOperation(t)
	state := &fakeHelperState{
		principal: reviewedPrincipal{UID: 1000, GID: 1000},
		request:   request,
		plan:      plan,
	}
	executor := &fakeHelperExecutor{}

	inspect, err := helperPipeExchange(
		t,
		validBootstrapHelperRequest(hostbootstrap.HelperActionInspect),
		&fakeAuthorizer{},
		state,
		executor,
	)
	if err != nil || inspect.Status == nil ||
		inspect.Status.Phase != hostbootstrap.PhaseInspect ||
		inspect.Receipt != nil {
		t.Fatalf("inspect response=%#v error=%v", inspect, err)
	}
	apply, err := helperPipeExchange(
		t,
		validBootstrapHelperRequest(hostbootstrap.HelperActionApply),
		&fakeAuthorizer{},
		state,
		executor,
	)
	if err != nil || apply.Status == nil ||
		(apply.Status.Phase != hostbootstrap.PhaseApply &&
			apply.Status.Phase != hostbootstrap.PhaseVerify) ||
		apply.Receipt != nil {
		t.Fatalf("apply response=%#v error=%v", apply, err)
	}
	verify, err := helperPipeExchange(
		t,
		validBootstrapHelperRequest(hostbootstrap.HelperActionVerify),
		&fakeAuthorizer{},
		state,
		executor,
	)
	if err != nil || verify.Status != nil || verify.Receipt == nil ||
		verify.Receipt.OperationID != request.OperationID {
		t.Fatalf("verify response=%#v error=%v", verify, err)
	}
}

func TestHelperProtocolExcludesSelfUninstall(t *testing.T) {
	raw := []byte(`{"schemaVersion":1,"operationId":"abababababababababababababababab","action":"bootstrap-uninstall"}`)
	if _, err := hostbootstrap.DecodeHelperRequest(raw); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("bootstrap-uninstall error = %v", err)
	}
}

func TestInitialHelperStartupRequiresStartBoundary(t *testing.T) {
	for _, effectID := range []string{
		"apply:helper:command:1",
	} {
		if !initialHelperStartReached(
			hostbootstrap.PlatformWindows,
			effectID,
		) {
			t.Fatalf("start boundary rejected %q", effectID)
		}
	}
	if !initialHelperStartReached(
		hostbootstrap.PlatformLinux,
		"apply:helper:command:2",
	) {
		t.Fatal("Linux helper start boundary was rejected")
	}
	for _, effectID := range []string{
		"",
		"helper:install-executable",
		"helper:definition:0",
		"apply:helper:command:0",
	} {
		if initialHelperStartReached(
			hostbootstrap.PlatformWindows,
			effectID,
		) {
			t.Fatalf("pre-start boundary accepted %q", effectID)
		}
	}
}

func TestStartupPriorMarkerRoutesOnlyToTransition(t *testing.T) {
	initial, err := classifyStartupLineage(startupPendingRecord{})
	if err != nil || initial != startupLineageInitial {
		t.Fatalf("initial mode=%v error=%v", initial, err)
	}
	prior := startupPriorMarker{
		SchemaVersion:  hostbootstrap.SchemaVersion,
		Kind:           hostbootstrap.ResourceHelper,
		OperationID:    strings.Repeat("ab", 16),
		IdentitySHA256: strings.Repeat("12", 32),
	}
	transition, err := classifyStartupLineage(startupPendingRecord{
		PriorMarker:      prior,
		PriorMarkerFound: true,
	})
	if err != nil || transition != startupLineageTransition {
		t.Fatalf("transition mode=%v error=%v", transition, err)
	}
	if _, err := classifyStartupLineage(startupPendingRecord{
		PriorMarker: prior,
	}); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("hidden prior marker error=%v", err)
	}
	prior.Kind = hostbootstrap.ResourceProduct
	if _, err := classifyStartupLineage(startupPendingRecord{
		PriorMarker:      prior,
		PriorMarkerFound: true,
	}); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("wrong transition marker error=%v", err)
	}
}

func TestReinstallLineageRequiresExactUninstallTombstone(t *testing.T) {
	request, plan := helperTestOperation(t)
	exact := exactHelperObservations(request)
	receipt, err := hostbootstrap.NewReceipt(plan, exact)
	if err != nil {
		t.Fatal(err)
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptSum := sha256.Sum256(receiptRaw)
	identity, err := helperInstallationIdentitySHA256(receipt.Binding)
	if err != nil {
		t.Fatal(err)
	}
	absent := plan.Observed
	tombstoneRaw, err := json.Marshal(struct {
		SchemaVersion              int                        `json:"schemaVersion"`
		ReceiptOperationID         string                     `json:"receiptOperationId"`
		ReceiptSHA256              string                     `json:"receiptSha256"`
		InstallationIdentitySHA256 string                     `json:"installationIdentitySha256"`
		ConfirmedAbsent            hostbootstrap.Observations `json:"confirmedAbsent"`
	}{
		SchemaVersion:              hostbootstrap.SchemaVersion,
		ReceiptOperationID:         receipt.OperationID,
		ReceiptSHA256:              hex.EncodeToString(receiptSum[:]),
		InstallationIdentitySHA256: identity,
		ConfirmedAbsent:            absent,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	state := newDiskHelperState(root)
	files := map[string][]byte{
		filepath.Join(
			root,
			"receipt-"+receipt.OperationID+".json",
		): receiptRaw,
	}
	state.read = func(path string) ([]byte, error) {
		raw, found := files[path]
		if !found {
			return nil, fs.ErrNotExist
		}
		return raw, nil
	}
	if err := state.validateUninstallTombstone(
		tombstoneRaw,
	); err != nil {
		t.Fatalf("valid tombstone error = %v", err)
	}
	var corrupted map[string]json.RawMessage
	if err := json.Unmarshal(tombstoneRaw, &corrupted); err != nil {
		t.Fatal(err)
	}
	badDigest, err := json.Marshal(strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	corrupted["receiptSha256"] = badDigest
	corruptedRaw, err := json.Marshal(corrupted)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.validateUninstallTombstone(
		corruptedRaw,
	); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("corrupt tombstone error = %v", err)
	}
	delete(files, filepath.Join(
		root,
		"receipt-"+receipt.OperationID+".json",
	))
	if err := state.validateUninstallTombstone(
		tombstoneRaw,
	); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing retained receipt error = %v", err)
	}
}

func TestRankReconcileIsValidatedAndTransportedWithoutCleanup(t *testing.T) {
	authorizer := &fakeAuthorizer{}
	state := &fakeHelperState{
		principal: reviewedPrincipal{UID: 1000, GID: 1000, SID: "S-1-5-21-1000"},
	}
	executor := &fakeHelperExecutor{}
	response, err := helperPipeExchange(t, validRankHelperRequest(), authorizer, state, executor)
	if err != nil {
		t.Fatalf("exchange error = %v", err)
	}
	if !response.Accepted ||
		response.Status != nil ||
		response.Receipt != nil {
		t.Fatalf("rank response = %#v, want accepted transport-only", response)
	}
	if state.loads != 0 || len(executor.actions) != 0 {
		t.Fatalf("rank request touched bootstrap/cleanup state")
	}
}

func TestHelperDerivesFreshRepairPlanAndRefusesForeignOrUnownedState(t *testing.T) {
	request, cleanPlan := helperTestOperation(t)
	exact := exactHelperObservations(request)
	receipt := hostbootstrap.Receipt{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         hostbootstrap.PhaseComplete,
		Decision:      cleanPlan.Decision,
		Binding:       request.Binding,
		Verified:      exact,
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	markers := testOwnedMarkers(request)
	drift := exact
	changedHelper := request.Binding.Helper
	changedHelper.SHA256 = strings.Repeat("56", 32)
	drift.Helper.Identity = &changedHelper
	repair, err := deriveHelperRepairPlan(
		request,
		receipt,
		markers,
		drift,
	)
	if err != nil {
		t.Fatal(err)
	}
	if repair.Decision != hostbootstrap.DecisionRepairOwned ||
		!reflect.DeepEqual(
			repair.Actions,
			[]hostbootstrap.ResourceKind{hostbootstrap.ResourceHelper},
		) {
		t.Fatalf("repair plan = %#v", repair)
	}
	foreign := drift
	foreign.Helper.Ownership = hostbootstrap.OwnershipForeign
	if _, err := deriveHelperRepairPlan(
		request,
		receipt,
		markers,
		foreign,
	); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("foreign error = %v", err)
	}
	unowned := exact
	unowned.Helper = hostbootstrap.ArtifactObservation{
		Ownership: hostbootstrap.OwnershipAbsent,
	}
	delete(markers, hostbootstrap.ResourceHelper)
	if _, err := deriveHelperRepairPlan(
		request,
		receipt,
		markers,
		unowned,
	); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("unowned error = %v", err)
	}
	markers[hostbootstrap.ResourceHelper] = ownedResourceMarker{
		IdentitySHA256: strings.Repeat("56", 32),
	}
	if _, err := deriveHelperRepairPlan(
		request,
		receipt,
		markers,
		exact,
	); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("changed helper marker error = %v", err)
	}
	markers[hostbootstrap.ResourceHelper] = ownedResourceMarker{
		IdentitySHA256: request.Binding.Helper.SHA256,
	}
	absentHeadless := exact
	absentHeadless.RuntimeOwners = []hostbootstrap.RuntimeOwnerObservation{}
	roleRepair, err := deriveHelperRepairPlan(
		request,
		receipt,
		markers,
		absentHeadless,
	)
	if err != nil ||
		!reflect.DeepEqual(
			roleRepair.Actions,
			[]hostbootstrap.ResourceKind{hostbootstrap.ResourceActiveRole},
		) {
		t.Fatalf("absent headless repair=%#v error=%v", roleRepair, err)
	}
	delete(markers, hostbootstrap.ResourceActiveRole)
	if _, err := deriveHelperRepairPlan(
		request,
		receipt,
		markers,
		absentHeadless,
	); !errors.Is(err, hostbootstrap.ErrHelperProtocol) {
		t.Fatalf("unmarked headless error = %v", err)
	}
}

func TestHelperApplyDispatchesFreshDerivedRepairPlan(t *testing.T) {
	request, plan := helperTestOperation(t)
	drift := exactHelperObservations(request)
	changedHelper := request.Binding.Helper
	changedHelper.SHA256 = strings.Repeat("56", 32)
	drift.Helper.Identity = &changedHelper
	state := &fakeHelperState{
		principal: reviewedPrincipal{UID: 1000, GID: 1000},
		request:   request,
		plan:      plan,
	}
	executor := &fakeHelperExecutor{observed: drift}
	response, err := helperPipeExchange(
		t,
		validBootstrapHelperRequest(hostbootstrap.HelperActionApply),
		&fakeAuthorizer{},
		state,
		executor,
	)
	if err != nil || !response.Accepted {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	if len(executor.plans) != 1 ||
		executor.plans[0].Decision != hostbootstrap.DecisionRepairOwned ||
		!reflect.DeepEqual(
			executor.plans[0].Actions,
			[]hostbootstrap.ResourceKind{hostbootstrap.ResourceHelper},
		) {
		t.Fatalf("executed plans = %#v", executor.plans)
	}
	foreign := drift
	foreign.Helper.Ownership = hostbootstrap.OwnershipForeign
	executor = &fakeHelperExecutor{observed: foreign}
	response, err = helperPipeExchange(
		t,
		validBootstrapHelperRequest(hostbootstrap.HelperActionApply),
		&fakeAuthorizer{},
		state,
		executor,
	)
	if err == nil || response.Accepted ||
		!reflect.DeepEqual(
			executor.actions,
			[]hostbootstrap.HelperAction{
				hostbootstrap.HelperActionInspect,
			},
		) {
		t.Fatalf("foreign response=%#v error=%v actions=%#v", response, err, executor.actions)
	}
}

func TestHelperPendingRecoveryUsesPersistedDerivedRepairPlan(t *testing.T) {
	request, _ := helperTestOperation(t)
	drift := exactHelperObservations(request)
	changedHelper := request.Binding.Helper
	changedHelper.SHA256 = strings.Repeat("56", 32)
	drift.Helper.Identity = &changedHelper
	repair, err := hostbootstrap.Reconcile(request, drift)
	if err != nil {
		t.Fatal(err)
	}
	state := &fakeHelperState{
		principal: reviewedPrincipal{UID: 1000, GID: 1000},
		request:   request,
		plan:      repair,
		pending:   true,
	}
	executor := &fakeHelperExecutor{
		observed: hostbootstrap.Observations{
			RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{},
		},
	}
	response, err := helperPipeExchange(
		t,
		validBootstrapHelperRequest(hostbootstrap.HelperActionApply),
		&fakeAuthorizer{},
		state,
		executor,
	)
	if err != nil || !response.Accepted {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	if executor.inspections != 0 ||
		len(executor.plans) != 1 ||
		!reflect.DeepEqual(executor.plans[0], repair) {
		t.Fatalf("inspections=%d plans=%#v", executor.inspections, executor.plans)
	}
}

func TestNoOpRolloverPreservesHistoricalMarkerRepairAuthorization(t *testing.T) {
	originalRequest, originalPlan := helperTestOperation(t)
	exact := exactHelperObservations(originalRequest)
	originalReceipt := hostbootstrap.Receipt{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   originalRequest.OperationID,
		Phase:         hostbootstrap.PhaseComplete,
		Decision:      originalPlan.Decision,
		Binding:       originalRequest.Binding,
		Verified:      exact,
	}
	request := originalRequest
	request.OperationID = strings.Repeat("cd", 16)
	noOp, err := hostbootstrap.Reconcile(request, exactHelperObservations(request))
	if err != nil {
		t.Fatal(err)
	}
	if noOp.Decision != hostbootstrap.DecisionNoOp {
		t.Fatalf("rollover plan = %#v", noOp)
	}
	for _, kind := range helperManagedResources {
		if !historicalMarkerAuthorized(
			request,
			originalReceipt,
			kind,
			helperResourceIdentitySHA256(kind, originalReceipt.Binding),
		) {
			t.Fatalf("historical marker rejected for %s", kind)
		}
	}
	receipt := hostbootstrap.Receipt{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   request.OperationID,
		Phase:         hostbootstrap.PhaseComplete,
		Decision:      noOp.Decision,
		Binding:       request.Binding,
		Verified:      exactHelperObservations(request),
	}
	drift := exactHelperObservations(request)
	helper := request.Binding.Helper
	helper.SHA256 = strings.Repeat("56", 32)
	drift.Helper.Identity = &helper
	state := &fakeHelperState{
		principal: reviewedPrincipal{UID: 1000, GID: 1000},
		request:   request,
		plan:      noOp,
		receipt:   receipt,
		markers:   testOwnedMarkers(request),
	}
	executor := &fakeHelperExecutor{observed: drift}
	response, err := helperPipeExchange(
		t,
		validBootstrapHelperRequestForOperation(
			hostbootstrap.HelperActionApply,
			request.OperationID,
		),
		&fakeAuthorizer{},
		state,
		executor,
	)
	if err != nil || !response.Accepted ||
		len(executor.plans) != 1 ||
		executor.plans[0].Decision != hostbootstrap.DecisionRepairOwned {
		t.Fatalf("response=%#v error=%v plans=%#v", response, err, executor.plans)
	}
}

func TestHistoricalMarkersAuthorizeOnlyTheirOwnStableIdentity(t *testing.T) {
	original, plan := helperTestOperation(t)
	exact := exactHelperObservations(original)
	receipt, err := hostbootstrap.NewReceipt(plan, exact)
	if err != nil {
		t.Fatal(err)
	}
	next := original
	next.OperationID = strings.Repeat("cd", 16)
	next.Binding.ControllerKey = helperTestPublicKey()
	next.Binding.ControllerKey.FingerprintSHA256 = strings.Repeat("56", 32)
	next.Binding.Helper.SHA256 = strings.Repeat("67", 32)
	next.Binding.Product.SHA256 = strings.Repeat("78", 32)
	for _, kind := range helperManagedResources {
		identity := helperResourceIdentitySHA256(kind, receipt.Binding)
		if !historicalMarkerAuthorized(next, receipt, kind, identity) {
			t.Fatalf("unrelated rollover rejected %s marker", kind)
		}
	}
	for _, test := range []struct {
		kind     hostbootstrap.ResourceKind
		identity string
	}{
		{
			kind:     hostbootstrap.ResourceAuthorizedKey,
			identity: next.Binding.ControllerKey.FingerprintSHA256,
		},
		{
			kind:     hostbootstrap.ResourceHelper,
			identity: next.Binding.Helper.SHA256,
		},
		{
			kind:     hostbootstrap.ResourceProduct,
			identity: next.Binding.Product.SHA256,
		},
	} {
		if historicalMarkerAuthorized(
			next,
			receipt,
			test.kind,
			test.identity,
		) {
			t.Fatalf("new %s identity authorized by historical receipt", test.kind)
		}
	}
	changedAccount := next
	changedAccount.Binding.Account.Name = "other"
	if historicalMarkerAuthorized(
		changedAccount,
		receipt,
		hostbootstrap.ResourceFirewall,
		helperResourceIdentitySHA256(
			hostbootstrap.ResourceFirewall,
			receipt.Binding,
		),
	) {
		t.Fatal("changed account retained historical marker authority")
	}
}

func exactHelperObservations(
	request hostbootstrap.Request,
) hostbootstrap.Observations {
	endpoint := request.Binding.Endpoint
	key := hostbootstrap.AuthorizedKeyIdentity{
		AccountName:       request.Binding.Account.Name,
		Path:              request.Binding.Account.AuthorizedKeysPath,
		FingerprintSHA256: request.Binding.ControllerKey.FingerprintSHA256,
	}
	product := request.Binding.Product
	helper := request.Binding.Helper
	return hostbootstrap.Observations{
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
			Identity:  &key,
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
}

func testOwnedMarkers(request hostbootstrap.Request) ownedResourceSet {
	markers := make(ownedResourceSet)
	for _, kind := range helperManagedResources {
		markers[kind] = ownedResourceMarker{
			IdentitySHA256: request.Binding.Helper.SHA256,
		}
	}
	return markers
}

func TestReviewedPrincipalAuthorization(t *testing.T) {
	principal := reviewedPrincipal{UID: 1000, GID: 1001, SID: "S-1-5-21-1000"}
	tests := []struct {
		name string
		peer peerIdentity
		want bool
	}{
		{name: "root", peer: peerIdentity{UID: 0, GID: 0}, want: true},
		{name: "uid", peer: peerIdentity{UID: 1000, GID: 99}, want: true},
		{name: "gid", peer: peerIdentity{UID: 99, GID: 1001}, want: true},
		{name: "sid", peer: peerIdentity{SID: "S-1-5-21-1000"}, want: true},
		{name: "system", peer: peerIdentity{System: true}, want: true},
		{name: "administrator", peer: peerIdentity{Administrator: true}, want: true},
		{name: "foreign", peer: peerIdentity{UID: 2000, GID: 2000, SID: "S-1-5-21-2000"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := authorizeIdentity(test.peer, principal)
			if (err == nil) != test.want {
				t.Fatalf("authorizeIdentity() error = %v, want allowed %t", err, test.want)
			}
		})
	}
}

func helperTestOperation(t *testing.T) (hostbootstrap.Request, hostbootstrap.Plan) {
	t.Helper()
	request := hostbootstrap.Request{
		SchemaVersion: hostbootstrap.SchemaVersion,
		OperationID:   strings.Repeat("ab", 16),
		Binding: hostbootstrap.Binding{
			Target: hostbootstrap.Target{
				Platform:     hostbootstrap.PlatformLinux,
				Architecture: hostbootstrap.ArchitectureAMD64,
			},
			Lane:         hostbootstrap.LaneQuickConnect,
			Role:         hostbootstrap.RoleHeadless,
			RuntimeOwner: hostbootstrap.RoleHeadless,
			Account: hostbootstrap.AccountIdentity{
				Name:               "pairuser",
				HomePath:           "/home/pairuser",
				AuthorizedKeysPath: "/home/pairuser/.ssh/authorized_keys",
			},
			ControllerKey: helperTestPublicKey(),
			Endpoint:      hostbootstrap.SSHEndpoint{Address: "192.0.2.10", Port: 22},
			Product: hostbootstrap.ArtifactIdentity{
				ID:      "nvpair",
				Version: "1.2.3",
				SHA256:  strings.Repeat("12", 32),
				Path:    "/opt/nvpair/nvpair",
			},
			Helper: hostbootstrap.ArtifactIdentity{
				ID:      "nvpair-host-helper",
				Version: "1.2.3",
				SHA256:  strings.Repeat("34", 32),
				Path:    "/usr/libexec/nvpair-host-helper",
			},
		},
	}
	observed := hostbootstrap.Observations{
		SSHService:    hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Firewall:      hostbootstrap.EndpointObservation{Ownership: hostbootstrap.OwnershipAbsent},
		AuthorizedKey: hostbootstrap.AuthorizedKeyObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Helper:        hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent},
		Product:       hostbootstrap.ArtifactObservation{Ownership: hostbootstrap.OwnershipAbsent},
		RuntimeOwners: []hostbootstrap.RuntimeOwnerObservation{},
	}
	plan, err := hostbootstrap.Reconcile(request, observed)
	if err != nil {
		t.Fatal(err)
	}
	return request, plan
}

func helperTestPublicKey() hostbootstrap.PublicKeyIdentity {
	algorithm := []byte(hostbootstrap.PublicKeyAlgorithmED25519)
	blob := make([]byte, 4+len(algorithm)+4+32)
	binary.BigEndian.PutUint32(blob[:4], uint32(len(algorithm)))
	copy(blob[4:], algorithm)
	offset := 4 + len(algorithm)
	binary.BigEndian.PutUint32(blob[offset:offset+4], 32)
	for index := offset + 4; index < len(blob); index++ {
		blob[index] = byte(index)
	}
	sum := sha256.Sum256(blob)
	return hostbootstrap.PublicKeyIdentity{
		Algorithm:         hostbootstrap.PublicKeyAlgorithmED25519,
		Material:          base64.StdEncoding.EncodeToString(blob),
		FingerprintSHA256: hex.EncodeToString(sum[:]),
	}
}
