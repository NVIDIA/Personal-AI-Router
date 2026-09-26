// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticMPIReconcileUsesExactCleanupForMissingAndStaleRanks(t *testing.T) {
	d, p, _, _ := mpiRecoveryFixture(t)
	nativeRoot := t.TempDir()
	build := strings.Repeat("c", 32)
	diagnosticMPIReconcileRun(t, d, p, strings.Repeat("a", 32), build, nil, nativeRoot)
	diagnosticMPIReconcileRun(t, d, p, strings.Repeat("e", 32), build, &diagnosticRankRecord{PID: 2147483647, StartTicks: "1"}, nativeRoot)
	diagnosticMPIReconcileNativeStub(t, d, d.m.mesh.NodeUUID())
	d.recoveryFailed = true
	result, err := d.reconcileRetainedMPIAt(context.Background(), d.m.mesh.NodeUUID(), diagnosticMPIReconcileControlFixture(t, d, build), nativeRoot)
	if err != nil || result.RecoveryRequired || len(result.Operations) != 2 || d.recoveryRequired() {
		t.Fatalf("exact cleanup did not release stale receipts: %+v hold=%t err=%v", result, d.recoveryRequired(), err)
	}
}

func TestDiagnosticMPIReconcileKeepsLiveOwnedRankAndForeignNativeRoot(t *testing.T) {
	for _, scenario := range []string{"live-rank", "symlink-root", "symlink-run"} {
		t.Run(scenario, func(t *testing.T) {
			d, p, _, _ := mpiRecoveryFixture(t)
			nativeRoot := t.TempDir()
			build := strings.Repeat("c", 32)
			id := strings.Repeat("a", 32)
			var rank *diagnosticRankRecord
			if scenario == "live-rank" {
				rank = &diagnosticRankRecord{PID: os.Getpid(), StartTicks: diagnosticProcessTicks(os.Getpid())}
			}
			diagnosticMPIReconcileRun(t, d, p, id, build, rank, nativeRoot)
			if scenario == "symlink-root" {
				foreign := filepath.Join(nativeRoot, strings.Repeat("f", 32))
				if err := os.Symlink(filepath.Join(nativeRoot, id), foreign); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink-run" {
				foreign := filepath.Join(filepath.Dir(d.runDir(id)), strings.Repeat("f", 32))
				if err := os.Symlink(d.runDir(id), foreign); err != nil {
					t.Fatal(err)
				}
			}
			diagnosticMPIReconcileNativeStub(t, d, d.m.mesh.NodeUUID())
			d.recoveryFailed = true
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			result, err := d.reconcileRetainedMPIAt(ctx, d.m.mesh.NodeUUID(), diagnosticMPIReconcileControlFixture(t, d, build), nativeRoot)
			if err != nil || !result.RecoveryRequired || !d.recoveryRequired() {
				t.Fatalf("%s escaped recovery fence: %+v hold=%t err=%v", scenario, result, d.recoveryRequired(), err)
			}
		})
	}
}
