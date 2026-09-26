// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingRankPreservesDifferentClosedMemoryOwner(t *testing.T) {
	f, b, system := missingRankFixture(t)
	old := &vllmManagedRank{binding: vllmGroupBinding{RunID: strings.Repeat("d", 32), Generation: 2, PlanDigest: b.PlanDigest, Rank: 1}, receipt: vllmRankReceipt{State: "stopped", CleanupConfirmed: true}}
	f.st.vllmRank = old
	_, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) { return system, nil }, func(p vllmRankSystemPlan) (vllmSystemRankResult, error) {
		if f.st.opMu.TryLock() {
			f.st.opMu.Unlock()
			t.Fatal("fence escaped Prepare serialization")
		}
		return fenceResult(p), nil
	})
	if err != nil || f.st.vllmRank != old || !old.receipt.CleanupConfirmed {
		t.Fatal("different closed owner was replaced", err)
	}
}
func TestMissingRankFenceWithoutLocalMarkerPersistenceStaysUnconfirmed(t *testing.T) {
	f, b, system := missingRankFixture(t)
	_, err := closeMissingVLLMRank(context.Background(), f.st, b, "reconcile", func() (vllmRankSystemPlan, error) { return system, nil }, func(p vllmRankSystemPlan) (vllmSystemRankResult, error) {
		if err := os.WriteFile(filepath.Join(f.st.installDir, "closed-ranks"), []byte("ordinary unavailable marker directory"), 0600); err != nil {
			t.Fatal(err)
		}
		return fenceResult(p), nil
	})
	if err == nil {
		t.Fatal("root fence without required local marker became confirmed app cleanup")
	}
}
