// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVLLMSystemStartPublishesUnlockedEntryAndTerminalStatus(t *testing.T) {
	f := vllmResourceFixture(t)
	if err := os.MkdirAll(f.st.installDir, 0700); err != nil {
		t.Fatal(err)
	}
	runtimeID := "v0-starting-status"
	writeManagedVLLMTestEnvironment(t, f.st, runtimeID, managedVLLMVersion)
	if err := writeVLLMRuntimeRecord(f.st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: runtimeID}); err != nil {
		t.Fatal(err)
	}
	_, _, runtimeReceipt, err := validateVLLMRuntimeBinaries(f.st, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	plan := vllmGroupTestPlan(2)
	plan.Members[0].RuntimeDigest = vllmRankRuntimeDigest(runtimeReceipt)
	plan.Members[0].RuntimeCompatibilitySHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	f.e.vllmRuntimeLock = func(context.Context, *engineState) (vllmRuntimeLock, error) {
		return vllmRuntimeLock{
			SourceRuntimeDigest: plan.Members[0].RuntimeDigest,
			CompatibilitySHA256: plan.Members[0].RuntimeCompatibilitySHA256,
		}, nil
	}
	rank := &vllmManagedRank{e: f.e, st: f.st, path: filepath.Join(f.st.installDir, "serving-rank.json"),
		binding: vllmGroupBinding{Plan: plan},
		receipt: vllmRankReceipt{State: "prepared", SystemPlan: &vllmRankSystemPlan{NodeID: "node-a"}}}
	f.st.vllmRank = rank
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var events []EngineStatus
	f.e.emit = func(method string, value any) {
		if method != "engine:state-changed" {
			return
		}
		// snapshot already acquired both locks; verify callbacks also run outside
		// them. Cancel before the native boundary; no process or administrator call.
		if !rank.mu.TryLock() {
			t.Fatal("publisher retained the rank lock")
		}
		rank.mu.Unlock()
		if !f.st.mu.TryLock() {
			t.Fatal("publisher retained the engine lock")
		}
		f.st.mu.Unlock()
		status, ok := value.(EngineStatus)
		if !ok {
			t.Fatal("unexpected engine-state payload")
		}
		events = append(events, status)
		cancel()
	}
	if err := rank.startSystem(ctx, nil); err == nil {
		t.Fatal("canceled, unauthorized fixture admitted a native rank")
	}
	if len(events) != 2 || !events[0].Starting || events[0].Running || events[0].Healthy || events[1].Starting || events[1].Running || events[1].Healthy {
		t.Fatalf("startup entry/terminal publication lost truth: %+v", events)
	}
}

func TestVLLMSystemAcknowledgedRanksStayStartingWithoutInventedReadiness(t *testing.T) {
	for _, index := range []int{0, 1} {
		f := vllmResourceFixture(t)
		rank := &vllmManagedRank{e: f.e, st: f.st, binding: vllmGroupBinding{Rank: index, Plan: vllmGroupTestPlan(2)}}
		f.st.vllmRank = rank
		for _, stage := range []struct {
			state    string
			running  bool
			starting bool
			healthy  bool
			closed   bool
		}{
			{state: "prepared", starting: true},
			{state: "starting", starting: true},
			{state: "started", running: true, starting: index == 0},
			{state: "ready", running: true, healthy: true},
			{state: "stopped", closed: true},
		} {
			rank.receipt.State, rank.receipt.CleanupConfirmed = stage.state, stage.closed
			f.st.running, f.st.healthy = stage.running, stage.healthy
			status := f.e.snapshot("vllm", f.st)
			if status.Starting != stage.starting || status.Running != stage.running || status.Healthy != (stage.healthy && index == 0) || (status.ServingGroup == nil) != stage.closed {
				t.Fatalf("rank %d/%s conflated launch, readiness or cleanup: %+v", index, stage.state, status)
			}
			if index != 0 {
				raw, err := rank.currentSystemModels(context.Background())
				var models struct {
					Data []json.RawMessage `json:"data"`
				}
				if err != nil || json.Unmarshal(raw, &models) != nil || len(models.Data) != 0 {
					t.Fatal("headless rank advertised model ownership")
				}
			}
		}
	}
}
