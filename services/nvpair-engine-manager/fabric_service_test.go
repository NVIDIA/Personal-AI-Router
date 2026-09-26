// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"nvpair-shared/cableprobe"
)

// Synthetic inventory and injected worker/control results only. These tests
// never call native fabric I/O, start a server, or contact a participant.
func fabricServiceInventory(node string) fabricInventory {
	on := true
	facts := fabricInventory{NodeID: node, Principal: "principal-" + node, Digest: "digest-" + node,
		ObservedAt: time.Now().UnixMilli(), Routes: []string{}, Interfaces: []fabricObservedInterface{}}
	for lane := 0; lane < 2; lane++ {
		facts.Interfaces = append(facts.Interfaces, fabricObservedInterface{
			fabricInterface: fabricInterface{Name: fmt.Sprintf("eth%d", lane), Index: lane + 2,
				MAC: fmt.Sprintf("02:00:00:00:00:%02x", lane+1), Addresses: []string{},
				PhysicalPort: fabricPhysicalPort{Source: "linux-sysfs", SwitchID: "switch-" + node, PortName: "p0"},
				Driver:       "mlx5_core", RDMADevices: []string{fmt.Sprintf("mlx5_%d", lane)}, MTU: 1500},
			Up: true, Physical: &on, Carrier: &on, SpeedMbps: 200000,
		})
	}
	return facts
}

func fabricServiceTargets(t *testing.T) ([]fabricTarget, []fabricInventory) {
	t.Helper()
	inventories := []fabricInventory{fabricServiceInventory("node-a"), fabricServiceInventory("node-b")}
	targets := make([]fabricTarget, 0, 2)
	for _, facts := range inventories {
		target, err := fabricSelectedTarget(facts, cableprobe.PortRef{NodeID: facts.NodeID,
			SwitchID: "switch-" + facts.NodeID, PortName: "p0"})
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	return targets, inventories
}

func fabricServiceFixture(t *testing.T) (*fabricService, *fabricRunRecord) {
	t.Helper()
	s := newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	targets, inventories := fabricServiceTargets(t)
	if err := fabricAllocate(targets, inventories); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	now := time.Now()
	r := &fabricRunRecord{Public: fabricOperation{SchemaVersion: 1, OperationID: id, ReviewID: id,
		OwnerNodeID: "owner", State: "applying", Targets: targets, CreatedAt: now.UnixMilli(),
		ExpiresAt: 0}, OwnerPrincipal: "principal-owner",
		Plans:     []cableLaunchPlan{{NodeID: "node-a", Principal: "principal-node-a"}, {NodeID: "node-b", Principal: "principal-node-b"}},
		Attempted: make([]bool, 2), Reserved: make([]bool, 2), cancel: cancel, done: make(chan struct{})}
	s.runs[id] = r
	r.AdministratorApproved = true
	s.refreshAccess = func(_ context.Context, run *fabricRunRecord) ([]cableLaunchPlan, error) {
		return append([]cableLaunchPlan(nil), run.Plans...), nil
	}
	s.inventory = func(context.Context, string) (fabricInventory, error) {
		t.Fatal("unexpected inventory I/O")
		return fabricInventory{}, nil
	}
	s.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
		t.Fatal("unexpected worker I/O")
		return fabricWorkerResult{}, nil
	}
	s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
		t.Fatal("unexpected control I/O")
		return fabricControlResult{}, nil
	}
	return s, r
}

func TestFabricSelectedTargetRequiresExactlyTwoIndependentInterfaces(t *testing.T) {
	for name, change := range map[string]func(*fabricInventory){
		"one-function": func(f *fabricInventory) { f.Interfaces = f.Interfaces[:1] },
		"three-functions": func(f *fabricInventory) {
			third := f.Interfaces[1]
			third.Index = 4
			third.Name = "eth2"
			third.RDMADevices = []string{"mlx5_2"}
			f.Interfaces = append(f.Interfaces, third)
		},
		"duplicate-index":       func(f *fabricInventory) { f.Interfaces[1].Index = f.Interfaces[0].Index },
		"duplicate-name":        func(f *fabricInventory) { f.Interfaces[1].Name = f.Interfaces[0].Name },
		"shared-rdma-device":    func(f *fabricInventory) { f.Interfaces[1].RDMADevices = f.Interfaces[0].RDMADevices },
		"missing-rdma-device":   func(f *fabricInventory) { f.Interfaces[0].RDMADevices = nil },
		"unavailable-addresses": func(f *fabricInventory) { f.Interfaces[0].AddressesUnavailable = true },
		"unknown-addresses":     func(f *fabricInventory) { f.Interfaces[0].Addresses = nil },
		"existing-address":      func(f *fabricInventory) { f.Interfaces[0].Addresses = []string{"192.0.2.1/30"} },
		"unknown-physical-port": func(f *fabricInventory) { f.Interfaces[0].Physical = nil },
		"unknown-carrier":       func(f *fabricInventory) { f.Interfaces[0].Carrier = nil },
		"administratively-down": func(f *fabricInventory) { f.Interfaces[0].Up = false },
		"different-driver":      func(f *fabricInventory) { f.Interfaces[0].Driver = "other" },
		"different-speed":       func(f *fabricInventory) { f.Interfaces[0].SpeedMbps = 100000 },
		"foreign-source":        func(f *fabricInventory) { f.Interfaces[0].PhysicalPort.Source = "cache" },
		"ambiguous-unselected-interface": func(f *fabricInventory) {
			extra := f.Interfaces[0]
			extra.PhysicalPort.PortName = "p1"
			f.Interfaces = append(f.Interfaces, extra)
		},
	} {
		t.Run(name, func(t *testing.T) {
			facts := fabricServiceInventory("node-a")
			change(&facts)
			if _, err := fabricSelectedTarget(facts, cableprobe.PortRef{NodeID: "node-a", SwitchID: "switch-node-a", PortName: "p0"}); err == nil {
				t.Fatal("ambiguous or unsupported inventory admitted")
			}
		})
	}
}

func TestFabricAllocateTwoIndependentLanesPreservesUnselectedAddresses(t *testing.T) {
	targets, inventories := fabricServiceTargets(t)
	inventories[0].Routes = []string{"0.0.0.0/0", "172.31.240.0/30"}
	unselected := fabricObservedInterface{fabricInterface: fabricInterface{Name: "mgmt0", Index: 99, Addresses: []string{"172.31.240.5/30"}}}
	inventories[1].Interfaces = append(inventories[1].Interfaces, unselected)
	if err := fabricAllocate(targets, inventories); err != nil {
		t.Fatal(err)
	}
	lanes := make([]netip.Prefix, 2)
	for lane := 0; lane < 2; lane++ {
		a, err := netip.ParsePrefix(targets[0].Interfaces[lane].Address)
		if err != nil {
			t.Fatal(err)
		}
		b, err := netip.ParsePrefix(targets[1].Interfaces[lane].Address)
		if err != nil {
			t.Fatal(err)
		}
		if a.Bits() != 30 || a.Masked() != b.Masked() || a.Addr() == b.Addr() {
			t.Fatal("lane must use distinct endpoints of one /30")
		}
		for _, used := range []string{"172.31.240.0/30", "172.31.240.4/30"} {
			if a.Overlaps(netip.MustParsePrefix(used)) {
				t.Fatal("allocated lane overlaps route or unselected interface")
			}
		}
		lanes[lane] = a.Masked()
	}
	if lanes[0].Overlaps(lanes[1]) {
		t.Fatal("PCI functions reused the same lane")
	}
	if !reflect.DeepEqual(inventories[1].Interfaces[2], unselected) {
		t.Fatal("unselected interface changed")
	}
	if targets[0].Interfaces[0].Address != "172.31.240.9/30" {
		t.Fatal("allocation did not skip both route and address collision")
	}
}

func TestFabricAllocateRejectsIncompleteOrInvalidInventory(t *testing.T) {
	for _, invalid := range []string{"unknown", "192.0.2.1", "192.0.2.1/100"} {
		targets, inventories := fabricServiceTargets(t)
		inventories[0].Routes = []string{invalid}
		if err := fabricAllocate(targets, inventories); err == nil {
			t.Fatal("invalid route accepted")
		}
		for _, target := range targets {
			for _, iface := range target.Interfaces {
				if iface.Address != "" {
					t.Fatal("allocation partially published on validation failure")
				}
			}
		}
	}
	targets, inventories := fabricServiceTargets(t)
	if fabricAllocate(targets[:1], inventories) == nil || fabricAllocate(targets, inventories[:1]) == nil {
		t.Fatal("incomplete participant scope accepted")
	}
}

func TestFabricServiceApprovalAndUnknownStatusNeverImplyEffects(t *testing.T) {
	s, r := fabricServiceFixture(t)
	delete(s.runs, r.Public.OperationID)
	for _, approve := range []bool{false, true} {
		if _, err := s.approve(context.Background(), r.Public.OperationID, approve); err == nil {
			t.Fatal("unknown/unapproved review executed")
		}
	}
	if _, err := s.status(r.Public.OperationID); err == nil {
		t.Fatal("unknown status fabricated an operation")
	}
	if _, err := s.cancel(r.Public.OperationID); err == nil {
		t.Fatal("unknown cancellation fabricated cleanup")
	}
	if _, err := s.recover(context.Background(), r.Public.OperationID, false); err == nil {
		t.Fatal("recovery accepted absent consent")
	}
	if s.held() {
		t.Fatal("rejected requests acquired a reservation")
	}
}

func TestFabricServiceNewProfileHasNoRecoveryHold(t *testing.T) {
	s := newFabricService(&Manager{exec: &Executor{baseDir: t.TempDir()}})
	if s.held() || s.recoveryFailed || s.reservation != nil || len(s.runs) != 0 {
		t.Fatal("a missing reservation file must not create a recovery hold in an otherwise empty profile")
	}
}

func TestFabricServiceApplyFailureRollsBackOnlyAttemptedTargets(t *testing.T) {
	for _, failedNode := range []string{"node-a", "node-b"} {
		t.Run(failedNode, func(t *testing.T) {
			s, r := fabricServiceFixture(t)
			calls := []string{}
			s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
				calls = append(calls, request.Method+":"+target.NodeID)
				return fabricControlResult{}, nil
			}
			s.worker = func(_ context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
				if plan.NodeID != request.Target.NodeID || request.OperationID != r.Public.OperationID {
					t.Fatal("worker escaped reviewed operation scope")
				}
				calls = append(calls, request.Method+":"+plan.NodeID)
				if request.Method == "apply" && plan.NodeID == failedNode {
					return fabricWorkerResult{}, errors.New("synthetic apply failure")
				}
				return fabricWorkerResult{CleanupConfirmed: request.Method == "rollback"}, nil
			}
			s.execute(context.Background(), r)
			want := []string{"reserve:node-a", "reserve:node-b", "inspect:node-a", "inspect:node-b", "apply:node-a"}
			if failedNode == "node-b" {
				want = append(want, "apply:node-b", "rollback:node-b")
			}
			want = append(want, "release:node-b", "rollback:node-a", "release:node-a")
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("owned cleanup sequence = %v; want %v", calls, want)
			}
			if !r.Public.CleanupConfirmed || s.held() {
				t.Fatal("confirmed cleanup did not release admission")
			}
			if r.Public.State != "failed" {
				t.Fatalf("apply failure lost its terminal cause: %s / %s", r.Public.State, r.Public.Message)
			}
		})
	}
}

func TestFabricServiceCancellationCleansBothAppliedTargets(t *testing.T) {
	s, r := fabricServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.cancel = cancel
	rolledBack, released := []string{}, []string{}
	s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		if request.Method == "release" {
			released = append(released, target.NodeID)
		}
		return fabricControlResult{}, nil
	}
	s.worker = func(_ context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if request.Method == "apply" && plan.NodeID == "node-b" {
			if _, err := s.cancel(r.Public.OperationID); err != nil {
				t.Fatal(err)
			}
		}
		if request.Method == "rollback" {
			rolledBack = append(rolledBack, plan.NodeID)
		}
		return fabricWorkerResult{CleanupConfirmed: request.Method == "rollback"}, nil
	}
	s.execute(ctx, r)
	if !reflect.DeepEqual(rolledBack, []string{"node-b", "node-a"}) || !reflect.DeepEqual(released, rolledBack) {
		t.Fatal("cancellation did not clean and release both selected participants")
	}
	if r.Public.State != "cancelled" || !r.Public.CleanupConfirmed || s.held() {
		t.Fatalf("cancellation not reconciled: %+v", r.Public)
	}
}

func TestFabricServiceRollbackFailurePreservesReservationAndRestartHold(t *testing.T) {
	s, r := fabricServiceFixture(t)
	r.Attempted = []bool{true, false}
	r.Reserved = []bool{true, true}
	released := []string{}
	s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		released = append(released, target.NodeID)
		return fabricControlResult{}, nil
	}
	s.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
		return fabricWorkerResult{}, errors.New("synthetic cleanup unavailable")
	}
	s.rollback(r)
	if !reflect.DeepEqual(released, []string{"node-b"}) {
		t.Fatal("released reservation whose owned cleanup was not confirmed")
	}
	if r.Public.State != "recovery-required" || r.Public.CleanupConfirmed || !s.held() {
		t.Fatal("unconfirmed cleanup was not held")
	}
	restored := newFabricService(s.m)
	op, err := restored.status(r.Public.OperationID)
	if err != nil || op.State != "recovery-required" || op.CleanupConfirmed || !restored.held() {
		t.Fatalf("restart lost owned cleanup hold: %+v %v", op, err)
	}
	if restored.runs[r.Public.OperationID].cancel != nil || restored.runs[r.Public.OperationID].done != nil {
		t.Fatal("restart invented a live worker")
	}
}

func TestFabricServiceRecoveryCannotOverlapLiveCleanup(t *testing.T) {
	s, r := fabricServiceFixture(t)
	r.Public.State = "recovery-required"
	// An open done channel models the existing execute/rollback goroutine after
	// a journal failure. Rejection must happen before any access or worker I/O.
	if _, err := s.recover(context.Background(), r.Public.OperationID, true); err == nil {
		t.Fatal("recovery overlapped the original cleanup")
	}
}

func TestFabricServiceAppliedAddressesAreStableAndDoNotFenceWorkloads(t *testing.T) {
	s, r := fabricServiceFixture(t)
	calls := []string{}
	s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
		calls = append(calls, request.Method+":"+target.NodeID)
		return fabricControlResult{}, nil
	}
	s.worker = func(_ context.Context, plan cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		calls = append(calls, request.Method+":"+plan.NodeID)
		return fabricWorkerResult{}, nil
	}
	s.execute(context.Background(), r)
	want := []string{"reserve:node-a", "reserve:node-b", "inspect:node-a", "inspect:node-b", "apply:node-a", "apply:node-b", "release:node-a", "release:node-b"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("stable apply unexpectedly rolled back or retained admission: %v", calls)
	}
	if r.Public.State != "active" || r.Public.ExpiresAt != 0 || !r.Public.EffectsApplied || r.Public.EffectsUnconfirmed || r.Public.CleanupConfirmed || s.held() {
		t.Fatalf("applied configuration is not usable stable state: %+v", r.Public)
	}
	if !reflect.DeepEqual(r.Reserved, []bool{false, false}) {
		t.Fatal("stable configuration retained participant reservations")
	}
	select {
	case <-r.done:
	default:
		t.Fatal("stable configuration still owns a live lifecycle worker")
	}
	restored := newFabricService(s.m)
	op, err := restored.status(r.Public.OperationID)
	if err != nil || op.State != "active" || op.ExpiresAt != 0 || restored.held() {
		t.Fatalf("restart fenced stable applied addresses: %+v %v", op, err)
	}
}

func TestFabricServiceActiveRollbackRequiresFreshConsentAndCompletedApply(t *testing.T) {
	s, r := fabricServiceFixture(t)
	r.Public.State = "active"
	for _, consent := range [][]bool{nil, {false}} {
		if _, err := s.cancel(r.Public.OperationID, consent...); err == nil {
			t.Fatal("active rollback accepted absent administrator approval")
		}
		if r.Public.State != "active" {
			t.Fatal("rejected rollback changed applied state")
		}
	}
	// The state is committed before initial reservation release finishes. Until
	// done closes, another rollback waits outside the lock and remains bounded.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.cancelContext(ctx, r.Public.OperationID, true); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("active rollback did not wait for unfinished initial admission release: %v", err)
	}
	if r.Public.State != "active" || r.rollbackWaiters != 0 {
		t.Fatal("bounded rollback wait changed state or retained intent")
	}
	close(r.done)
}

func TestFabricServiceBusyExplicitRollbackPreservesAppliedAddresses(t *testing.T) {
	for _, releaseUncertain := range []bool{false, true} {
		t.Run(fmt.Sprintf("release-unconfirmed-%v", releaseUncertain), func(t *testing.T) {
			s, r := fabricServiceFixture(t)
			r.Public.State = "rolling-back"
			r.Public.EffectsApplied = true
			r.Attempted = []bool{true, true}
			workerCalls, acquiredReleased := 0, false
			s.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
				workerCalls++
				return fabricWorkerResult{}, nil
			}
			s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
				if request.Method == "reserve-rollback" && target.NodeID == "node-b" {
					return fabricControlResult{}, errors.New("synthetic NCCL workload is busy")
				}
				if request.Method == "release" && target.NodeID == "node-a" {
					acquiredReleased = true
					if releaseUncertain {
						return fabricControlResult{}, errors.New("synthetic reservation release unavailable")
					}
				}
				return fabricControlResult{Reserved: request.Method == "reserve-rollback"}, nil
			}
			s.explicitRollback(r, true)
			if workerCalls != 0 || !acquiredReleased || r.Public.CleanupConfirmed || !r.Public.EffectsApplied {
				t.Fatal("busy rollback mutated addresses or failed to unwind acquired admission")
			}
			if releaseUncertain {
				if r.Public.State != "recovery-required" || !s.held() {
					t.Fatal("uncertain admission release did not retain its recovery hold")
				}
			} else if r.Public.State != "active" || s.held() || !strings.Contains(strings.ToLower(r.Public.Message), "busy") {
				t.Fatalf("busy refusal did not preserve usable configuration and reason: %+v", r.Public)
			}
		})
	}
}

// A participant that holds reserve-rollback until the shared deadline must not
// also fail the compensating releases and get the wrong stage retained.
func TestFabricRollbackReleasesAfterReserveExhaustsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, r := fabricServiceFixture(t)
		r.Public.State = "rolling-back"
		r.Public.EffectsApplied = true
		r.Attempted = []bool{true, true}
		released := map[string]bool{}
		s.control = func(ctx context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
			if request.Method == "reserve-rollback" && target.NodeID == "node-b" {
				<-ctx.Done()
				return fabricControlResult{}, ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return fabricControlResult{}, err
			}
			if request.Method == "release" {
				released[target.NodeID] = true
			}
			return fabricControlResult{Reserved: request.Method == "reserve-rollback"}, nil
		}
		s.explicitRollback(r, false)
		want := fabricRollbackFailure{NodeID: "node-b", Stage: "reserve-rollback", Code: "deadline-exceeded"}
		got, parsed := parseFabricRollbackFailureMessage(r.Public.Message, r.Public.Targets)
		if !parsed || got != want || !released["node-a"] || !released["node-b"] || slices.Contains(r.Reserved, true) {
			t.Fatalf("compensating release shared the exhausted deadline: operation=%+v parsed=%+v released=%v", r.Public, got, released)
		}
	})
}

func TestFabricServiceRecoveryRollbackRetainsBoundedControlFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stage    string
		nodeID   string
		wantCode string
	}{
		{name: "reserve", stage: "reserve-rollback", nodeID: "node-b", wantCode: "admission-unavailable"},
		{name: "release", stage: "release", nodeID: "node-a", wantCode: "release-unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := fabricServiceFixture(t)
			r.Public.State = "rolling-back"
			r.Public.EffectsApplied = true
			r.Attempted = []bool{true, true}
			secret := "synthetic-private-control-detail-" + strings.Repeat("x", 2048)
			controlErr := errors.New("remote /v1/fabric/control: HTTP 409: fabric identity, inventory or admission unavailable; " + secret)
			s.control = func(_ context.Context, target fabricTarget, request fabricControlRequest) (fabricControlResult, error) {
				if request.Method == "reserve-rollback" && target.NodeID == "node-b" {
					return fabricControlResult{}, controlErr
				}
				if tc.stage == "release" && request.Method == "release" && target.NodeID == "node-a" {
					return fabricControlResult{}, controlErr
				}
				return fabricControlResult{Reserved: request.Method == "reserve-rollback"}, nil
			}

			s.explicitRollback(r, false)
			want := fabricRollbackFailure{NodeID: tc.nodeID, Stage: tc.stage, Code: tc.wantCode}
			got, parsed := parseFabricRollbackFailureMessage(r.Public.Message, r.Public.Targets)
			if r.Public.State != "recovery-required" || !parsed || got != want || r.Public.Message != fabricRollbackFailureMessage(want) {
				t.Fatalf("rollback control failure was not retained exactly: operation=%+v parsed=%+v", r.Public, got)
			}
			if len(r.Public.Message) > maxFabricRollbackFailureMessage || strings.Contains(r.Public.Message, secret) || !validFabricRecord(*r) {
				t.Fatalf("rollback diagnosis was unbounded, private, or invalid: %q", r.Public.Message)
			}
			raw, err := readOnboardingFile(s.file(r.Public.OperationID), maxFabricRecordBytes)
			if err != nil || strings.Contains(string(raw), secret) {
				t.Fatalf("private rollback cause entered retained state: %v", err)
			}
			restored := newFabricService(s.m)
			retained := restored.runs[r.Public.OperationID]
			if restored.recoveryFailed || retained == nil || retained.Public.Message != r.Public.Message {
				t.Fatalf("restart erased the bounded rollback diagnosis: recoveryFailed=%v retained=%+v", restored.recoveryFailed, retained)
			}
		})
	}
}

func TestFabricServiceUnavailableRollbackAccessPreservesActiveAddresses(t *testing.T) {
	s, r := fabricServiceFixture(t)
	r.Public.State = "active"
	r.Public.EffectsApplied = true
	r.Attempted = []bool{true, true}
	r.done = nil // Restored applied configuration has no live execution goroutine.
	reserves, workers, refreshes := 0, 0, 0
	s.refreshAccess = func(context.Context, *fabricRunRecord) ([]cableLaunchPlan, error) {
		refreshes++
		return nil, errors.New("synthetic account access unavailable")
	}
	s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
		reserves++
		return fabricControlResult{}, nil
	}
	s.worker = func(context.Context, cableLaunchPlan, fabricWorkerRequest) (fabricWorkerResult, error) {
		workers++
		return fabricWorkerResult{}, nil
	}
	s.explicitRollback(r, true)
	if refreshes != 1 || reserves != 0 || workers != 0 {
		t.Fatalf("unavailable access reached reservation or native effects: refresh=%d control=%d worker=%d", refreshes, reserves, workers)
	}
	if r.Public.State != "active" || r.Public.CleanupConfirmed || !r.Public.EffectsApplied || s.held() {
		t.Fatalf("unavailable access did not preserve usable addresses: %+v", r.Public)
	}
	if !strings.Contains(strings.ToLower(r.Public.Message), "access") || !strings.Contains(strings.ToLower(r.Public.Message), "unavailable") {
		t.Fatalf("access failure was not explained: %s", r.Public.Message)
	}
}

func TestFabricServiceCurrentStatusHoldsUnavailableAppliedInventory(t *testing.T) {
	s, r := fabricServiceFixture(t)
	close(r.done) // a settled apply; no lifecycle worker is running
	r.Public.State = "active"
	r.Public.EffectsApplied = true
	reads := []string{}
	s.inventory = func(_ context.Context, node string) (fabricInventory, error) {
		reads = append(reads, node)
		return fabricInventory{}, errors.New("synthetic current inventory unavailable")
	}
	stored, err := s.status(r.Public.OperationID)
	if err != nil || stored.State != "active" || len(reads) != 0 {
		t.Fatal("stored status unexpectedly claimed a current inventory read")
	}
	op, err := s.currentStatus(context.Background(), r.Public.OperationID)
	if err != nil || op.State != "recovery-required" || op.CleanupConfirmed || !op.EffectsApplied || !s.held() {
		t.Fatalf("failed current inventory did not hold the exact configuration: %+v %v", op, err)
	}
	if !reflect.DeepEqual(reads, []string{"node-a", "node-b"}) {
		t.Fatalf("failed reads retried or escaped selected scope: %v", reads)
	}
	restored := newFabricService(s.m)
	retained, err := restored.status(r.Public.OperationID)
	if err != nil || retained.State != "recovery-required" || !restored.held() {
		t.Fatal("current-status recovery hold was not retained for restart")
	}
}
