// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticNativeProcessTimeoutAndOutputBound(t *testing.T) {
	for _, script := range []string{"exec sleep 30", "yes excessive-output"} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		started := time.Now()
		output, err := diagnosticProcess(ctx, "/bin/sh", []string{"-c", script}, nil, nil)
		cancel()
		if err == nil || len(output) > 1<<20 || time.Since(started) > 3*time.Second {
			t.Fatalf("bounded process: len=%d error=%v elapsed=%s", len(output), err, time.Since(started))
		}
	}
}

func TestDiagnosticNativeCancelBeforeLaunchLeavesTombstone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "owned-run")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	clean, err := diagnosticCancelRank(ctx, dir)
	if !clean || err != nil {
		t.Fatalf("cancel=%v %v", clean, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cancelled")); err != nil {
		t.Fatal("late rank could miss cancellation tombstone")
	}
}

func TestDiagnosticNativeJoinedCancellationIsConfirmedCleanup(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSONAtomic(filepath.Join(dir, "rank.json"), diagnosticRankRecord{Done: true, Clean: true, Error: "signal: killed; context canceled"}); err != nil {
		t.Fatal(err)
	}
	clean, err := diagnosticCancelRank(context.Background(), dir)
	if !clean || err != nil {
		t.Fatalf("execution cancellation incorrectly became cleanup uncertainty: %v %v", clean, err)
	}
}

func TestDiagnosticNativeCleanupDoesNotKillReusedPID(t *testing.T) {
	dir := t.TempDir()
	rank := diagnosticRankRecord{PID: os.Getpid(), StartTicks: "not-the-current-process"}
	if err := writeJSONAtomic(filepath.Join(dir, "rank.json"), rank); err != nil {
		t.Fatal(err)
	}
	clean, err := diagnosticCancelRank(context.Background(), dir)
	if !clean || err != nil {
		t.Fatalf("reused PID cleanup=%v %v", clean, err)
	}
}

func TestDiagnosticNativeOwnedRankLeaseAndDuplicateLaunch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, base := userPaths()
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	p := diagnosticTestProfile()
	p.Members[0].NCCL = diagnosticTool{Path: "/bin/true", SHA256: hex.EncodeToString(sum[:])}
	profileJSON, _ := json.Marshal(map[string]any{"groups": []diagnosticProfile{p}})
	if err := os.WriteFile(filepath.Join(base, "diagnostic-groups.json"), profileJSON, 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	dir := filepath.Join(base, "diagnostic-runs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lease := diagnosticLeaseRecord{Request: diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: id, ProfileDigest: profileDigest(p), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}, Member: p.Members[0]}
	if err := writeJSONAtomic(filepath.Join(dir, "lease.json"), lease); err != nil {
		t.Fatal(err)
	}
	if code := diagnosticRankMain(p.GroupID, id); code != 0 {
		t.Fatalf("owned rank exit=%d", code)
	}
	var rank diagnosticRankRecord
	if err := readDiagnosticJSON(filepath.Join(dir, "rank.json"), &rank); err != nil || !rank.Done || !rank.Clean || rank.PID == 0 {
		t.Fatalf("owned rank receipt=%+v err=%v", rank, err)
	}
	if code := diagnosticRankMain(p.GroupID, id); code == 0 {
		t.Fatal("same rank lease launched twice")
	}
	clean, err := diagnosticCancelRank(context.Background(), dir)
	if !clean || err != nil {
		t.Fatalf("owned rank cleanup=%v %v", clean, err)
	}
}

func TestDiagnosticNativeExpiredRankCannotStart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, base := userPaths()
	p := diagnosticTestProfile()
	data, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	p.Members[0].NCCL = diagnosticTool{Path: "/bin/true", SHA256: hex.EncodeToString(sum[:])}
	id := strings.Repeat("b", 32)
	dir := filepath.Join(base, "diagnostic-runs", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	profileJSON, _ := json.Marshal(map[string]any{"groups": []diagnosticProfile{p}})
	if err := os.WriteFile(filepath.Join(base, "diagnostic-groups.json"), profileJSON, 0600); err != nil {
		t.Fatal(err)
	}
	lease := diagnosticLeaseRecord{Request: diagnosticParticipantRequest{GroupID: p.GroupID, OperationID: id, ProfileDigest: profileDigest(p), ExpiresAt: time.Now().Add(-time.Second).UnixMilli()}, Member: p.Members[0]}
	if err := writeJSONAtomic(filepath.Join(dir, "lease.json"), lease); err != nil {
		t.Fatal(err)
	}
	if code := diagnosticRankMain(p.GroupID, id); code == 0 {
		t.Fatal("expired rank launched")
	}
	if _, err := os.Stat(filepath.Join(dir, "rank.json")); !os.IsNotExist(err) {
		t.Fatal("expired rank created a process receipt")
	}
}
