// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func missingRankFixture(t *testing.T) (*vllmInstallFixture, vllmGroupBinding, vllmRankSystemPlan) {
	t.Helper()
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	p := vllmGroupTestPlan(2)
	digest, _ := vllmGroupPlanDigest(p)
	b := vllmGroupBinding{RunID: strings.Repeat("c", 32), Generation: 3, PlanDigest: digest, Plan: p, Rank: 1}
	system := vllmRankSystemPlan{Owner: vllmSystemRankOwner, RunID: b.RunID, Generation: b.Generation, Rank: b.Rank, PlanDigest: b.PlanDigest, NodeID: p.Members[1].NodeID, UID: os.Getuid()}
	old := vllmRankReceipt{RunID: strings.Repeat("d", 32), Generation: 2, Rank: 1, PlanDigest: digest, State: "stopped", CleanupConfirmed: true}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), old); err != nil {
		t.Fatal(err)
	}
	return f, b, system
}
func fenceResult(p vllmRankSystemPlan) vllmSystemRankResult {
	hash, _ := vllmSystemPlanHash(p)
	return vllmSystemRankResult{State: "closed", Tombstone: true, StartFenced: true, CleanupConfirmed: true, Unit: fmt.Sprintf("nvpair-vllm-rank-%s-%d-%d.service", p.RunID, p.Generation, p.Rank), PlanHash: hash}
}
func staleMissingRankFixture(t *testing.T) (*vllmInstallFixture, vllmGroupBinding, vllmRankSystemPlan, vllmRankSystemPlan, *vllmManagedRank) {
	t.Helper()
	f, b, requested := missingRankFixture(t)
	for i := range b.Plan.Members {
		b.Plan.Members[i].Placement = &vllmGroupPlacement{ModelPath: filepath.Join(f.st.installDir, "model"), Address: fmt.Sprintf("10.0.0.%d", i+1), APIPort: 8181, MasterPort: 29500}
	}
	digest, err := vllmGroupPlanDigest(b.Plan)
	if err != nil {
		t.Fatal(err)
	}
	b.PlanDigest = digest
	b.Generation, requested.Generation = 14, 14
	requested.PlanDigest = digest
	member, coordinator := b.Plan.Members[b.Rank], b.Plan.Members[0]
	requested.Owner, requested.Model, requested.ModelDigest = vllmSystemRankOwner, b.Plan.Model, member.ModelDigest
	requested.GPUUUID, requested.ConfigSHA256 = member.GPUUUID, b.Plan.Topology.ConfigSHA256
	requested.LocalAddress, requested.CoordinatorAddress = member.Placement.Address, coordinator.Placement.Address
	requested.APIPort, requested.MasterPort = member.Placement.APIPort, member.Placement.MasterPort
	stale := requested
	stale.RunID, stale.Generation = strings.Repeat("e", 32), 13
	stale.PlanDigest = strings.Repeat("f", 64)
	stale.Manager = vllmRankSystemIdentity{PID: 1300, StartTicks: "13", UID: os.Getuid()}
	stale.Devices = []string{"/dev/stale-gpu", "/dev/stale-rdma"}
	old := vllmRankReceipt{RunID: stale.RunID, Generation: stale.Generation, Rank: stale.Rank, PlanDigest: stale.PlanDigest,
		State: "cleanup-required", SystemEffectsApplied: true, SystemPlan: &stale}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), old); err != nil {
		t.Fatal(err)
	}
	rank := f.e.restoreVLLMRankHold(f.st)
	if rank == nil {
		t.Fatal("stale retained rank was not restored")
	}
	f.st.vllmRank = rank
	return f, b, requested, stale, rank
}

func TestMissingRankReconcilesExactStaleGenerationBeforeRequestedFence(t *testing.T) {
	f, b, requested, stale, rank := staleMissingRankFixture(t)
	elevation := &diagnosticPackageElevation{NodeID: requested.NodeID, NonInteractive: true}
	var generations []uint64
	makeCalls := 0
	got, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
		makeCalls++
		return requested, nil
	}, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
		generations = append(generations, plan.Generation)
		if f.st.opMu.TryLock() {
			f.st.opMu.Unlock()
			t.Fatal("two-phase cleanup escaped participant lifecycle serialization")
		}
		if _, _, err := vllmSystemRankInvocation("reconcile", plan, nil, elevation); err != nil {
			t.Fatal("one fresh node-scoped elevation did not cover both exact plans", err)
		}
		if plan.Generation == stale.Generation && (!reflect.DeepEqual(plan.Manager, stale.Manager) || !slices.Equal(plan.Devices, stale.Devices)) {
			t.Fatal("stale retained custody was rebuilt")
		}
		return fenceResult(plan), nil
	})
	if err != nil || !got.CleanupConfirmed || makeCalls != 1 || !slices.Equal(generations, []uint64{13, 14}) {
		t.Fatalf("stale/current cleanup order = %v, make=%d, result=%+v, err=%v", generations, makeCalls, got, err)
	}
	var retained vllmRankReceipt
	if err := readVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), 16<<10, &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Generation != 13 || retained.RunID != stale.RunID || !retained.CleanupConfirmed || retained.State != "stopped" || !reflect.DeepEqual(retained.SystemPlan, &stale) {
		t.Fatalf("historical receipt was replaced instead of reconciled: %+v", retained)
	}
	rank.mu.Lock()
	memoryClosed := rank.receipt.Generation == 13 && rank.receipt.State == "stopped" && rank.receipt.CleanupConfirmed
	rank.mu.Unlock()
	if !memoryClosed {
		t.Fatal("restored historical owner did not observe its durable cleanup")
	}
	if _, err := readVLLMRankClosure(f.st, b); err != nil {
		t.Fatal("requested generation did not receive its distinct closure marker", err)
	}
}

func TestMissingRankExistingRequestedMarkerDoesNotTouchStalePredecessor(t *testing.T) {
	f, b, requested, _, rank := staleMissingRankFixture(t)
	path := filepath.Join(f.st.installDir, "serving-rank.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := vllmSystemPlanHash(requested)
	if err != nil {
		t.Fatal(err)
	}
	markerPath, err := vllmRankClosurePath(f.st, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0700); err != nil {
		t.Fatal(err)
	}
	marker := vllmClosedRank{RunID: b.RunID, Generation: b.Generation, Rank: b.Rank, PlanDigest: b.PlanDigest,
		NodeID: requested.NodeID, UID: os.Getuid(), Unit: fmt.Sprintf("nvpair-vllm-rank-%s-%d-%d.service", b.RunID, b.Generation, b.Rank),
		PlanHash: hash, StartFenced: true, CleanupConfirmed: true}
	if err := writeVLLMJSON(f.st.installDir, markerPath, marker); err != nil {
		t.Fatal(err)
	}
	got, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
		t.Fatal("existing requested marker rebuilt a plan")
		return requested, nil
	}, func(vllmRankSystemPlan) (vllmSystemRankResult, error) {
		t.Fatal("existing requested marker touched an unrelated stale predecessor")
		return vllmSystemRankResult{}, nil
	})
	after, readErr := os.ReadFile(path)
	rank.mu.Lock()
	staleHeld := !rank.receipt.CleanupConfirmed
	rank.mu.Unlock()
	if err != nil || !got.CleanupConfirmed || readErr != nil || !bytes.Equal(before, after) || !staleHeld {
		t.Fatalf("idempotent requested closure changed stale custody: result=%+v err=%v read=%v held=%v", got, err, readErr, staleHeld)
	}
}

func TestMissingRankStaleCleanupFailureKeepsRequestedGenerationOpen(t *testing.T) {
	for _, kind := range []string{"native-error", "unbound-proof"} {
		t.Run(kind, func(t *testing.T) {
			f, b, requested, stale, rank := staleMissingRankFixture(t)
			calls := 0
			_, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
				t.Fatal("requested plan was built after stale cleanup failure")
				return requested, nil
			}, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
				calls++
				if plan.Generation != stale.Generation {
					t.Fatal("requested generation was fenced before stale cleanup completed")
				}
				if kind == "native-error" {
					return vllmSystemRankResult{}, errors.New("stale cleanup unavailable")
				}
				result := fenceResult(plan)
				result.PlanHash = strings.Repeat("f", 64)
				return result, nil
			})
			if err == nil || calls != 1 {
				t.Fatalf("stale failure crossed into requested closure: calls=%d err=%v", calls, err)
			}
			if _, err := readVLLMRankClosure(f.st, b); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed stale cleanup created a requested closure marker", err)
			}
			rank.mu.Lock()
			reserved := !rank.receipt.CleanupConfirmed && rank.receipt.Generation == 13
			rank.mu.Unlock()
			if !reserved {
				t.Fatal("stale cleanup failure released or replaced its retained hold")
			}
		})
	}
}

func TestMissingRankRejectsStaleCustodyOutsideRequestedLocalIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*vllmRankReceipt, vllmGroupBinding)
	}{
		{"missing-plan", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan = nil }},
		{"receipt-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.Generation-- }},
		{"uid-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.UID++ }},
		{"node-mismatch", func(old *vllmRankReceipt, b vllmGroupBinding) { old.SystemPlan.NodeID = b.Plan.Members[0].NodeID }},
		{"owner-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.Owner = "other-owner" }},
		{"local-address-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.LocalAddress = "10.0.0.99" }},
		{"coordinator-address-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.CoordinatorAddress = "10.0.0.99" }},
		{"model-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.Model = "other/model" }},
		{"model-digest-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.ModelDigest = strings.Repeat("9", 64) }},
		{"gpu-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) {
			old.SystemPlan.GPUUUID = "GPU-99999999-0000-0000-0000-000000000001"
		}},
		{"config-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.ConfigSHA256 = strings.Repeat("9", 64) }},
		{"api-port-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.APIPort++ }},
		{"master-port-mismatch", func(old *vllmRankReceipt, _ vllmGroupBinding) { old.SystemPlan.MasterPort++ }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, b, requested, _, _ := staleMissingRankFixture(t)
			path := filepath.Join(f.st.installDir, "serving-rank.json")
			var old vllmRankReceipt
			if err := readVLLMJSON(f.st.installDir, path, 16<<10, &old); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&old, b)
			if err := writeVLLMJSON(f.st.installDir, path, old); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f.st.vllmRank = f.e.restoreVLLMRankHold(f.st)
			calls := 0
			_, err = closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
				return requested, nil
			}, func(vllmRankSystemPlan) (vllmSystemRankResult, error) {
				calls++
				return vllmSystemRankResult{}, nil
			})
			if err == nil || calls != 0 {
				t.Fatalf("invalid stale custody reached root cleanup: calls=%d err=%v", calls, err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected unrelated custody was rewritten", readErr)
			}
			if _, err := readVLLMRankClosure(f.st, b); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid stale custody closed the requested generation", err)
			}
		})
	}
}

func TestMissingRankOlderRequestCannotCleanNewerPreparedOwner(t *testing.T) {
	f, b, requested, _, _ := staleMissingRankFixture(t)
	newer := b
	newer.RunID, newer.Generation = strings.Repeat("7", 32), 15
	newerPlan := requested
	newerPlan.RunID, newerPlan.Generation = newer.RunID, newer.Generation
	path := filepath.Join(f.st.installDir, "serving-rank.json")
	receipt := vllmRankReceipt{RunID: newer.RunID, Generation: newer.Generation, Rank: newer.Rank, PlanDigest: newer.PlanDigest, State: "prepared", SystemPlan: &newerPlan}
	if err := writeVLLMJSON(f.st.installDir, path, receipt); err != nil {
		t.Fatal(err)
	}
	rank := &vllmManagedRank{e: f.e, st: f.st, binding: newer, path: path, receipt: receipt}
	rank.binding.Plan = cloneVLLMGroupPlan(newer.Plan)
	f.st.vllmRank = rank
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	_, err = closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
		t.Fatal("older request built a closure plan around a newer owner")
		return requested, nil
	}, func(vllmRankSystemPlan) (vllmSystemRankResult, error) {
		calls++
		return vllmSystemRankResult{}, nil
	})
	after, readErr := os.ReadFile(path)
	rank.mu.Lock()
	startable := rank.receipt.State == "prepared" && !rank.receipt.CleanupConfirmed && rank.matches(newer)
	rank.mu.Unlock()
	if err == nil || calls != 0 || readErr != nil || !bytes.Equal(before, after) || !startable {
		t.Fatalf("older request changed newer prepared owner: calls=%d err=%v read=%v startable=%v", calls, err, readErr, startable)
	}
}

func TestMissingRankRequestedFenceFailureRetainsOldCleanupAndRetriesOnlyRequested(t *testing.T) {
	f, b, requested, stale, _ := staleMissingRankFixture(t)
	var generations []uint64
	failRequested := true
	run := func() error {
		_, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) { return requested, nil }, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
			generations = append(generations, plan.Generation)
			if plan.Generation == requested.Generation && failRequested {
				return vllmSystemRankResult{}, errors.New("requested fence unavailable")
			}
			return fenceResult(plan), nil
		})
		return err
	}
	if err := run(); err == nil {
		t.Fatal("requested fence failure became cleanup success")
	}
	var retained vllmRankReceipt
	if err := readVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), 16<<10, &retained); err != nil || retained.Generation != stale.Generation || !retained.CleanupConfirmed {
		t.Fatal("successful stale cleanup was lost or overwritten", err)
	}
	if _, err := readVLLMRankClosure(f.st, b); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed requested fence created a closure marker", err)
	}
	failRequested = false
	if err := run(); err != nil {
		t.Fatal("retry did not resume at the requested fence", err)
	}
	if !slices.Equal(generations, []uint64{13, 14, 14}) {
		t.Fatalf("retry repeated or reordered cleanup: %v", generations)
	}
	if _, err := readVLLMRankClosure(f.st, b); err != nil {
		t.Fatal("retry did not close the requested generation", err)
	}
}

func TestMissingRankPersistsProvenStaleCleanupBeforeCancellation(t *testing.T) {
	f, b, requested, stale, rank := staleMissingRankFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := closeMissingVLLMRank(ctx, f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
		t.Fatal("cancelled stale phase reached the requested plan")
		return requested, nil
	}, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
		calls++
		if plan.Generation != stale.Generation {
			t.Fatal("cancelled stale phase reached the requested fence")
		}
		cancel()
		return fenceResult(plan), nil
	})
	var retained vllmRankReceipt
	readErr := readVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), 16<<10, &retained)
	rank.mu.Lock()
	memoryClosed := rank.receipt.CleanupConfirmed && rank.receipt.Generation == stale.Generation
	rank.mu.Unlock()
	if !errors.Is(err, context.Canceled) || calls != 1 || readErr != nil || !retained.CleanupConfirmed || retained.Generation != stale.Generation || !memoryClosed {
		t.Fatalf("proven stale cleanup was not checkpointed: calls=%d err=%v read=%v receipt=%+v memory=%v", calls, err, readErr, retained, memoryClosed)
	}
	if _, err := readVLLMRankClosure(f.st, b); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancellation after stale proof closed the requested generation", err)
	}
}
func TestMissingRankReconcileRequiresRealFenceAndPreservesHistory(t *testing.T) {
	f, b, system := missingRankFixture(t)
	path := filepath.Join(f.st.installDir, "serving-rank.json")
	before, _ := os.ReadFile(path)
	calls := 0
	got, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) { return system, nil }, func(p vllmRankSystemPlan) (vllmSystemRankResult, error) { calls++; return fenceResult(p), nil })
	if err != nil || !got.CleanupConfirmed || got.EffectsApplied || got.State != "stopped" || calls != 1 {
		t.Fatal("closing fence failed", got, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || f.st.vllmRank != nil {
		t.Fatal("old history/owner replaced")
	}
	if rejectVLLMClosedRank(f.st, b) == nil {
		t.Fatal("late Prepare not fenced")
	}
	_, err = closeMissingVLLMRank(context.Background(), f.st, b, "stop", func() (vllmRankSystemPlan, error) { t.Fatal("closed marker rebuilt a plan"); return system, nil }, func(vllmRankSystemPlan) (vllmSystemRankResult, error) {
		t.Fatal("closed marker repeated root call")
		return vllmSystemRankResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestMissingRankUncertainOrUnboundFenceDoesNotClose(t *testing.T) {
	for _, kind := range []string{"uncertain", "plain-absence", "wrong-hash", "effects", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f, b, system := missingRankFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := closeMissingVLLMRank(ctx, f.st, b, "reconcile", func() (vllmRankSystemPlan, error) { return system, nil }, func(p vllmRankSystemPlan) (vllmSystemRankResult, error) {
				r := fenceResult(p)
				switch kind {
				case "uncertain":
					return r, errors.New("unconfirmed")
				case "plain-absence":
					r.State = "stopped"
					r.Tombstone = false
				case "wrong-hash":
					r.PlanHash = strings.Repeat("f", 64)
				case "effects":
					r.EffectsApplied = true
				case "cancelled":
					cancel()
				}
				return r, nil
			})
			if err == nil {
				t.Fatal("unconfirmed fence became success")
			}
			if _, err := readVLLMRankClosure(f.st, b); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unconfirmed closure marker was retained", err)
			}
		})
	}
}
func TestMissingRankRejectsActiveUnclosedAndStart(t *testing.T) {
	for _, kind := range []string{"engine", "memory-rank", "disk-rank", "busy", "start"} {
		t.Run(kind, func(t *testing.T) {
			f, b, system := missingRankFixture(t)
			action := "reconcile"
			switch kind {
			case "engine":
				f.st.running = true
			case "memory-rank":
				f.st.vllmRank = &vllmManagedRank{receipt: vllmRankReceipt{State: "prepared"}}
			case "disk-rank":
				_ = writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), vllmRankReceipt{State: "cleanup-required"})
			case "busy":
				f.st.opMu.Lock()
				defer f.st.opMu.Unlock()
			case "start":
				action = "start"
			}
			_, err := closeMissingVLLMRank(context.Background(), f.st, b, action, func() (vllmRankSystemPlan, error) { t.Fatal("conflict rebuilt plan"); return system, nil }, func(vllmRankSystemPlan) (vllmSystemRankResult, error) {
				t.Fatal("conflict reached root fence")
				return vllmSystemRankResult{}, nil
			})
			if err == nil {
				t.Fatal("active/uncertain/action conflict admitted")
			}
		})
	}
}
func TestMissingRankRestoredSameFencedOperationReusesOriginalRootPlan(t *testing.T) {
	f, b, system := missingRankFixture(t)
	system.Manager = vllmRankSystemIdentity{PID: 123, StartTicks: "old-manager", UID: os.Getuid()}
	old := vllmRankReceipt{RunID: b.RunID, Generation: b.Generation, Rank: b.Rank, PlanDigest: b.PlanDigest, State: "stopped", CleanupConfirmed: true, StartFenced: true, SystemPlan: &system}
	if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), old); err != nil {
		t.Fatal(err)
	}
	_, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
		t.Fatal("original fenced plan replaced with current manager")
		return system, nil
	}, func(p vllmRankSystemPlan) (vllmSystemRankResult, error) {
		if p.Manager != system.Manager {
			t.Fatal("root binding changed")
		}
		return fenceResult(p), nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A manager that dies after its rank's confirmed normal stop but before the
// group journal records it must not leave the group, or a leased fabric,
// permanently held: the exact receipt is the cleanup proof.
func TestMissingRankConfirmedNormalStopClosesFromItsOwnReceipt(t *testing.T) {
	f, b, system := missingRankFixture(t)
	system.Manager = vllmRankSystemIdentity{PID: 123, StartTicks: "dead-manager", UID: os.Getuid()}
	old := vllmRankReceipt{RunID: b.RunID, Generation: b.Generation, Rank: b.Rank, PlanDigest: b.PlanDigest, State: "stopped",
		CleanupConfirmed: true, SystemEffectsApplied: true, SystemPlan: &system}
	path := filepath.Join(f.st.installDir, "serving-rank.json")
	if err := writeVLLMJSON(f.st.installDir, path, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := closeMissingVLLMRank(context.Background(), f.st, b, "stop", func() (vllmRankSystemPlan, error) {
		t.Fatal("a confirmed exact stop rebuilt a closing plan")
		return system, nil
	}, func(vllmRankSystemPlan) (vllmSystemRankResult, error) {
		t.Fatal("a confirmed exact stop was fenced again")
		return vllmSystemRankResult{}, nil
	})
	if err != nil || got.State != "stopped" || !got.CleanupConfirmed || got.EffectsApplied {
		t.Fatalf("the confirmed stop was not closed: %+v %v", got, err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("the rank receipt was rewritten")
	}
	for name, spoil := range map[string]func(*vllmRankReceipt){
		"unconfirmed":  func(r *vllmRankReceipt) { r.CleanupConfirmed = false },
		"stopping":     func(r *vllmRankReceipt) { r.State = "stopping" },
		"start-fenced": func(r *vllmRankReceipt) { r.StartFenced = true },
	} {
		f, b, system := missingRankFixture(t)
		unclean := old
		unclean.RunID, unclean.Generation, unclean.Rank, unclean.PlanDigest = b.RunID, b.Generation, b.Rank, b.PlanDigest
		unclean.SystemPlan = &system
		spoil(&unclean)
		if err := writeVLLMJSON(f.st.installDir, filepath.Join(f.st.installDir, "serving-rank.json"), unclean); err != nil {
			t.Fatal(err)
		}
		fenced := false
		_, _ = closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) { return system, nil },
			func(p vllmRankSystemPlan) (vllmSystemRankResult, error) { fenced = true; return fenceResult(p), nil })
		if !fenced {
			t.Fatalf("a %s receipt skipped the root fence", name)
		}
	}
}

func TestMissingRankUncleanHistoricalReceiptReusesOriginalRootPlan(t *testing.T) {
	f, b, system := missingRankFixture(t)
	system.Manager = vllmRankSystemIdentity{PID: 123, StartTicks: "historical-manager", UID: os.Getuid()}
	system.Devices = []string{"/dev/historical-gpu", "/dev/historical-rdma"}
	old := vllmRankReceipt{
		RunID: b.RunID, Generation: b.Generation, Rank: b.Rank, PlanDigest: b.PlanDigest,
		State: "stopping", CleanupConfirmed: false, SystemEffectsApplied: true, SystemPlan: &system,
	}
	path := filepath.Join(f.st.installDir, "serving-rank.json")
	if err := writeVLLMJSON(f.st.installDir, path, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) {
		t.Fatal("unclean historical custody was rebuilt from current manager or hardware state")
		return vllmRankSystemPlan{}, nil
	}, func(plan vllmRankSystemPlan) (vllmSystemRankResult, error) {
		if plan.Manager != system.Manager || !slices.Equal(plan.Devices, system.Devices) {
			t.Fatal("historical manager or device custody changed")
		}
		return fenceResult(plan), nil
	})
	if err != nil || !got.CleanupConfirmed {
		t.Fatalf("historical root fence did not close: %+v %v", got, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("historical rank receipt was rewritten during closure")
	}
}
