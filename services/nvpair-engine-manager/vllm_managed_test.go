// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVLLMInstallSupportMatrixUsesOfficialWheelFloors(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		arch   string
		glibc  string
		gpus   []vllmGPUSupport
		allow  bool
		needle string
	}{
		{name: "linux x64 floor", goos: "linux", arch: "amd64", glibc: "2.28", gpus: []vllmGPUSupport{{Compute: "7.5"}}, allow: true},
		{name: "linux arm64 spark class", goos: "linux", arch: "arm64", glibc: "2.39", gpus: []vllmGPUSupport{{Compute: "12.1"}}, allow: true},
		{name: "native windows unsupported", goos: "windows", arch: "amd64", glibc: "2.39", gpus: []vllmGPUSupport{{Compute: "8.9"}}, needle: "selected user-owned Linux distribution"},
		{name: "macOS unsupported", goos: "darwin", arch: "arm64", glibc: "2.39", gpus: []vllmGPUSupport{{Compute: "8.9"}}, needle: "unsupported on macOS"},
		{name: "unsupported architecture", goos: "linux", arch: "riscv64", glibc: "2.39", gpus: []vllmGPUSupport{{Compute: "8.9"}}, needle: "recipe"},
		{name: "old glibc", goos: "linux", arch: "amd64", glibc: "2.27", gpus: []vllmGPUSupport{{Compute: "8.9"}}, needle: "2.28"},
		{name: "old compute capability", goos: "linux", arch: "amd64", glibc: "2.39", gpus: []vllmGPUSupport{{Compute: "7.0"}}, needle: "7.5"},
		{name: "gpu unobserved", goos: "linux", arch: "amd64", glibc: "2.39", needle: "NVIDIA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason := vllmInstallSupportReason(tc.goos, tc.arch, tc.glibc, tc.gpus)
			if tc.allow && reason != "" {
				t.Fatalf("qualified host rejected: %s", reason)
			}
			if !tc.allow && (reason == "" || !strings.Contains(strings.ToLower(reason), strings.ToLower(tc.needle))) {
				t.Fatalf("reason = %q, want %q", reason, tc.needle)
			}
			if strings.Contains(strings.ToLower(reason), "r580") {
				t.Fatalf("support reason contains an unsupported driver-number floor: %q", reason)
			}
		})
	}
}

func TestVLLMUnsupportedPlatformReasonsKeepWSLOwnershipExplicit(t *testing.T) {
	windows := vllmUnsupportedPlatformReason("windows", "amd64")
	for _, text := range []string{"no native Windows recipe", "WSL2", "selected user-owned Linux distribution", "does not yet own", "never installs or enables WSL", "use a qualified Linux amd64 or arm64 PAIR node today"} {
		if !strings.Contains(windows, text) {
			t.Fatalf("Windows prerequisite %q does not contain %q", windows, text)
		}
	}
	if got := vllmUnsupportedPlatformReason("darwin", "arm64"); !strings.Contains(got, "unsupported on macOS") {
		t.Fatalf("macOS reason = %q", got)
	}
	if got := vllmUnsupportedPlatformReason("linux", "riscv64"); !strings.Contains(got, "Linux amd64 or arm64") {
		t.Fatalf("Linux architecture reason = %q", got)
	}
}

func TestUnavailableVLLMStatusPublishesTheHostPlatformReason(t *testing.T) {
	other := "linux/amd64"
	if other == runtime.GOOS+"/"+runtime.GOARCH {
		other = "darwin/arm64"
	}
	ex := newTestExecutor(t, &Manifest{
		Engine: "vllm", DisplayName: "vLLM", ManifestVersion: 1,
		Platforms: map[string]Platform{other: {}},
	})
	want := vllmUnsupportedPlatformReason(runtime.GOOS, runtime.GOARCH)
	status, err := ex.Status("vllm")
	if err != nil || status.InstallSupported == nil || *status.InstallSupported || status.InstallReason != want {
		t.Fatalf("Status = %+v, %v; want unsupported reason %q", status, err, want)
	}
	installed := ex.GetInstalled()
	if len(installed) != 1 || installed[0].InstallSupported == nil || *installed[0].InstallSupported || installed[0].InstallReason != want {
		t.Fatalf("GetInstalled = %+v; want unsupported reason %q", installed, want)
	}
	if err := ex.Install(context.Background(), "vllm"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Install error = %v; want unsupported reason %q", err, want)
	}
}

func TestManagedVLLMDependencyReportSortsStructuredFilesByPath(t *testing.T) {
	if strings.Contains(managedVLLMDependencyReport, "files=sorted(files)") ||
		!strings.Contains(managedVLLMDependencyReport, "files=sorted(files,key=lambda item:item['path'])") {
		t.Fatal("dependency report sorts dictionaries without a deterministic scalar key")
	}
}

func TestManagedVLLMDependencyReportAcceptsItsBoundedRuntimePayload(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 2<<20)
	report := append([]byte(`{"schema":1,"packages":[{"name":"fixture","files":"`), payload...)
	report = append(report, []byte(`"}]}`)...)
	if err := validateManagedVLLMDependencyReport(report); err != nil {
		t.Fatalf("valid report above diagnostic control's 128 KiB limit was rejected: %v", err)
	}

	root := t.TempDir()
	path := filepath.Join(root, "pip-report.json")
	if err := writeManagedVLLMDependencyReport(root, path, report); err != nil {
		t.Fatalf("valid report above the ownership-record 1 MiB limit was rejected: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, report) {
		t.Fatalf("retained report mismatch: bytes=%d err=%v", len(got), err)
	}

	if err := validateManagedVLLMDependencyReport(append(report, []byte(` {}`)...)); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
	if err := validateManagedVLLMDependencyReport(bytes.Repeat([]byte("x"), vllmDependencyReportMaxBytes+1)); err == nil {
		t.Fatal("oversized dependency report was accepted")
	}
}

func TestManagedVLLMStageFailureCleansExactCandidate(t *testing.T) {
	st := &engineState{installDir: t.TempDir()}
	id := "v0.29.0-fixture"
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMRuntimeRecord(st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Staged: id}); err != nil {
		t.Fatal(err)
	}
	primary := errors.New("injected post-stage failure")
	resultErr := error(primary)
	cleanupFailedManagedVLLMStage(st, id, &resultErr)
	if !errors.Is(resultErr, primary) {
		t.Fatalf("primary stage failure was lost: %v", resultErr)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("failed candidate was not removed: %v", err)
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || record.Staged != "" {
		t.Fatalf("staged ownership was not cleared: record=%+v err=%v", record, err)
	}
}

func TestManagedVLLMPointerWriteFailureRetainsCandidateOwnership(t *testing.T) {
	st := managedVLLMTestState(t)
	oldID, newID := "v0-old", "v0-new"
	writeManagedVLLMTestEnvironment(t, st, oldID, managedVLLMVersion)
	writeManagedVLLMTestEnvironment(t, st, newID, managedVLLMVersion)
	old := vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: oldID, Staged: newID}
	if err := writeVLLMRuntimeRecord(st, old); err != nil {
		t.Fatal(err)
	}
	writes := 0
	control := vllmRuntimeControl{
		stop: func() error { return nil }, start: func(context.Context) error { return nil },
		recoveryStart: func(context.Context) error { return nil }, detect: func(bool) error { return nil },
		write: func(record vllmRuntimeRecord) error {
			writes++
			if writes == 2 {
				return errors.New("injected pointer write failure")
			}
			return writeVLLMRuntimeRecord(st, record)
		},
	}
	err := (&Executor{reporter: NewReporter(nil)}).activateManagedVLLM(context.Background(), st, old, newID, false, control)
	if err == nil {
		t.Fatal("pointer write failure was not surfaced")
	}
	restored, readErr := readVLLMRuntimeRecord(st)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if restored.Active != oldID || restored.Activating != nil || restored.Staged != "" || !containsString(restored.Retired, newID) {
		t.Fatalf("candidate ownership was lost: %+v", restored)
	}
}

func TestManagedVLLMCrashAfterActiveSwapRollsBackDurably(t *testing.T) {
	st := managedVLLMTestState(t)
	oldID, newID := "v0-old", "v0-new"
	writeManagedVLLMTestEnvironment(t, st, oldID, managedVLLMVersion)
	writeManagedVLLMTestEnvironment(t, st, newID, managedVLLMVersion)
	record := vllmRuntimeRecord{
		Schema: vllmRuntimeRecordSchema, Active: oldID, Staged: newID,
		Activating: &vllmActivationIntent{Candidate: newID, PriorActive: oldID, WasRunning: true},
	}
	if err := writeVLLMRuntimeRecord(st, record); err != nil {
		t.Fatal(err)
	}
	restarted := 0
	control := vllmRuntimeControl{
		stop: func() error { return nil }, start: func(context.Context) error { return nil },
		recoveryStart: func(context.Context) error { restarted++; return nil }, detect: func(bool) error { return nil },
		write: func(record vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, record) },
	}
	if err := (&Executor{reporter: NewReporter(nil)}).reconcileManagedVLLMActivationWithControl(context.Background(), st, control); err != nil {
		t.Fatal(err)
	}
	restored, err := readVLLMRuntimeRecord(st)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Active != oldID || restored.Activating != nil || !containsString(restored.Retired, newID) || restarted != 1 {
		t.Fatalf("crash rollback = %+v restarted=%d", restored, restarted)
	}
}

func TestManagedVLLMCrashRecoveryHonorsExplicitDesiredOff(t *testing.T) {
	st := managedVLLMTestState(t)
	oldID, newID := "v0-old", "v0-new"
	writeManagedVLLMTestEnvironment(t, st, oldID, managedVLLMVersion)
	writeManagedVLLMTestEnvironment(t, st, newID, managedVLLMVersion)
	record := vllmRuntimeRecord{
		Schema: vllmRuntimeRecordSchema, Active: oldID, Staged: newID,
		Activating: &vllmActivationIntent{Candidate: newID, PriorActive: oldID, WasRunning: true},
	}
	if err := writeVLLMRuntimeRecord(st, record); err != nil {
		t.Fatal(err)
	}
	desired := newDesiredStateStore(t.TempDir())
	if err := desired.set("vllm", false); err != nil {
		t.Fatal(err)
	}
	st.running, st.healthy, st.proc = true, true, &managedProc{}
	restarted := 0
	control := vllmRuntimeControl{
		stop: func() error { return nil }, start: func(context.Context) error { return nil },
		recoveryStart: func(context.Context) error { restarted++; return nil }, detect: func(bool) error { return nil },
		write: func(record vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, record) },
	}
	ex := &Executor{
		reg: NewRegistry(), reporter: NewReporter(nil), desired: desired,
		emit: func(string, any) {}, engines: map[string]*engineState{"vllm": st},
	}
	ex.reg.engines["vllm"] = st.manifest
	if err := ex.reconcileManagedVLLMActivationWithControl(context.Background(), st, control); err != nil {
		t.Fatal(err)
	}
	if restarted != 0 {
		t.Fatalf("explicit OFF was undone by crash recovery: restarts=%d", restarted)
	}
	if st.running || st.proc != nil {
		t.Fatal("explicit OFF left the prepared-phase prior runtime running")
	}
	restored, err := readVLLMRuntimeRecord(st)
	if err != nil || restored.Active != oldID || !containsString(restored.Retired, newID) {
		t.Fatalf("prepared OFF rollback = %+v err=%v", restored, err)
	}
}

func TestManagedVLLMPreparedCrashRestartsPriorAndRetainsCandidate(t *testing.T) {
	st := managedVLLMTestState(t)
	oldID, newID := "v0-old", "v0-new"
	writeManagedVLLMTestEnvironment(t, st, oldID, managedVLLMVersion)
	writeManagedVLLMTestEnvironment(t, st, newID, managedVLLMVersion)
	record := vllmRuntimeRecord{
		Schema: vllmRuntimeRecordSchema, Active: oldID, Staged: newID,
		Activating: &vllmActivationIntent{Candidate: newID, PriorActive: oldID, WasRunning: true},
	}
	if err := writeVLLMRuntimeRecord(st, record); err != nil {
		t.Fatal(err)
	}
	restarted := 0
	control := vllmRuntimeControl{
		stop: func() error { return nil }, start: func(context.Context) error { return nil },
		recoveryStart: func(context.Context) error { restarted++; return nil }, detect: func(bool) error { return nil },
		write: func(record vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, record) },
	}
	if err := (&Executor{reporter: NewReporter(nil)}).reconcileManagedVLLMActivationWithControl(context.Background(), st, control); err != nil {
		t.Fatal(err)
	}
	restored, err := readVLLMRuntimeRecord(st)
	if err != nil || restored.Active != oldID || !containsString(restored.Retired, newID) || restarted != 1 {
		t.Fatalf("prepared recovery = %+v restarted=%d err=%v", restored, restarted, err)
	}
}

func TestManagedVLLMFinalCommitFailureStopsCandidateAndRestoresPrior(t *testing.T) {
	st := managedVLLMTestState(t)
	oldID, newID := "v0-old", "v0-new"
	writeManagedVLLMTestEnvironment(t, st, oldID, managedVLLMVersion)
	writeManagedVLLMTestEnvironment(t, st, newID, managedVLLMVersion)
	old := vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: oldID, Staged: newID}
	if err := writeVLLMRuntimeRecord(st, old); err != nil {
		t.Fatal(err)
	}
	writes, stops, starts, recoveries := 0, 0, 0, 0
	control := vllmRuntimeControl{
		stop: func() error { stops++; return nil }, start: func(context.Context) error { starts++; return nil },
		recoveryStart: func(context.Context) error { recoveries++; return nil }, detect: func(bool) error { return nil },
		write: func(record vllmRuntimeRecord) error {
			writes++
			if writes == 3 {
				return errors.New("injected activation commit failure")
			}
			return writeVLLMRuntimeRecord(st, record)
		},
	}
	if err := (&Executor{reporter: NewReporter(nil)}).activateManagedVLLM(context.Background(), st, old, newID, true, control); err == nil {
		t.Fatal("final commit failure was not surfaced")
	}
	restored, err := readVLLMRuntimeRecord(st)
	if err != nil || restored.Active != oldID || !containsString(restored.Retired, newID) || stops != 2 || starts != 1 || recoveries != 1 {
		t.Fatalf("commit rollback=%+v stops=%d starts=%d recoveries=%d err=%v", restored, stops, starts, recoveries, err)
	}
}

func TestVLLMRuntimeRecordRotationKeepsReceiptScopedOwnership(t *testing.T) {
	old := vllmRuntimeRecord{
		Schema:   vllmRuntimeRecordSchema,
		Active:   "v0-active",
		Previous: "v0-previous",
		Retired:  []string{"v0-retired"},
	}
	next := nextVLLMRuntimeRecord(old, "v0-new")
	if next.Schema != vllmRuntimeRecordSchema || next.Active != "v0-new" || next.Previous != "v0-active" {
		t.Fatalf("next record = %+v", next)
	}
	if !reflect.DeepEqual(next.Retired, []string{"v0-retired", "v0-previous"}) {
		t.Fatalf("retired ownership = %v", next.Retired)
	}
	if got := vllmRecordedIDs(next); !reflect.DeepEqual(got, []string{"v0-retired", "v0-previous", "v0-active", "v0-new"}) {
		t.Fatalf("recorded ids = %v", got)
	}
}

func TestManagedVLLMReceiptRecognitionKeepsUpdateReachableWithoutTrustingArbitraryRecipes(t *testing.T) {
	st := &engineState{}
	recipe := testVLLMRecipe(t)
	receipt := vllmRuntimeReceipt{
		Schema: vllmEnvironmentReceiptSchema, RecipeID: recipe.RecipeID, Version: recipe.Version, Architecture: recipe.Architecture,
		UVURL: recipe.UVURL, UVSHA256: recipe.UVSHA256,
	}
	if !recognizedManagedVLLMReceipt(st, receipt) {
		t.Fatal("catalogued receipt was not recognized")
	}
	if !currentManagedVLLMReceipt(st, receipt) {
		t.Fatal("runnable receipt was not admitted")
	}
	receipt.Version = "0.28.0"
	if recognizedManagedVLLMReceipt(st, receipt) {
		t.Fatal("arbitrary version sharing the current uv tuple was trusted")
	}
}

func TestManagedVLLMRecordedImageOwnershipIncludesRetiredCandidate(t *testing.T) {
	st := managedVLLMTestState(t)
	activeID, retiredID := "v0-active", "v0-retired"
	writeManagedVLLMTestEnvironment(t, st, activeID, managedVLLMVersion)
	retiredDir := writeManagedVLLMTestEnvironment(t, st, retiredID, managedVLLMVersion)
	if err := writeVLLMRuntimeRecord(st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: activeID, Retired: []string{retiredID}}); err != nil {
		t.Fatal(err)
	}
	if !managedVLLMRecordedImageMatches(st, filepath.Join(retiredDir, "venv", "bin", "vllm")) {
		t.Fatal("receipt-owned retired candidate was not reclaimable")
	}
}

func TestVLLMRuntimeRecordRoundTripIsBoundedToOwnedRoot(t *testing.T) {
	st := &engineState{installDir: t.TempDir()}
	want := vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: "v0-active", Previous: "v0-previous", Retired: []string{"v0-retired"}}
	if err := writeVLLMRuntimeRecord(st, want); err != nil {
		t.Fatal(err)
	}
	got, err := readVLLMRuntimeRecord(st)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %+v, want %+v", got, want)
	}
	for _, id := range []string{"../escape", "v", "v0/escape", "active"} {
		if _, err := vllmEnvironmentPath(st, id); err == nil {
			t.Errorf("environment id %q escaped validation", id)
		}
	}
}

func TestManagedVLLMLegacyStableOwnershipIsReadCompatible(t *testing.T) {
	st := &engineState{installDir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(st.installDir, vllmRuntimeRecordFile), []byte(`{"schema":1,"active":"v0-legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || record.Schema != vllmRuntimeRecordSchema || record.Active != "v0-legacy" {
		t.Fatalf("legacy stable record = %+v err=%v", record, err)
	}
	if err := os.WriteFile(filepath.Join(st.installDir, vllmRuntimeRecordFile), []byte(`{"schema":1,"active":"v0-legacy","staged":{"environmentId":"v0-stage"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readVLLMRuntimeRecord(st); err == nil {
		t.Fatal("legacy staged recovery state was accepted")
	}
}

func TestManagedVLLMLegacyReceiptIsOwnershipOnly(t *testing.T) {
	st := &engineState{installDir: t.TempDir()}
	var recipe vllmLegacyRecipe
	for _, candidate := range managedVLLMLegacyOwnershipRecipes {
		if candidate.Architecture == runtime.GOARCH {
			recipe = candidate
			break
		}
	}
	if recipe.Architecture == "" {
		t.Skipf("no retained vLLM recipe for %s", runtime.GOARCH)
	}
	id := "v0-legacy"
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	python, cli, report := filepath.Join(bin, "python"), filepath.Join(bin, "vllm"), filepath.Join(dir, "pip-report.json")
	for path, body := range map[string][]byte{python: []byte("legacy-python"), cli: []byte("legacy-cli"), report: []byte("legacy-report")} {
		if err := os.WriteFile(path, body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pythonHash, _ := managedVLLMFileHash(dir, python)
	reportHash, _ := managedVLLMFileHash(dir, report)
	rootAbs, _ := filepath.Abs(st.installDir)
	dirAbs, _ := filepath.Abs(dir)
	receipt := vllmRuntimeReceipt{
		Schema: vllmLegacyReceiptSchema, Engine: "vllm", OwnerRoot: rootAbs, Environment: dirAbs,
		Version: recipe.Version, Architecture: recipe.Architecture, WheelURL: recipe.WheelURL, WheelSHA256: recipe.WheelSHA256,
		PythonSource: legacyVLLMPythonSources[0], PythonSHA256: pythonHash, PipReportSHA256: reportHash,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmEnvironmentReceiptFile), receipt); err != nil {
		t.Fatal(err)
	}
	gotBin, gotReceipt, err := validateVLLMEnvironment(st, id)
	if err != nil || gotBin != python || !recognizedManagedVLLMReceipt(st, gotReceipt) || admittedManagedVLLMReceipt(gotReceipt) {
		t.Fatalf("legacy ownership bin=%q receipt=%+v admitted=%v err=%v", gotBin, gotReceipt, admittedManagedVLLMReceipt(gotReceipt), err)
	}
	if !managedVLLMImageMatches(st, id, python) || managedVLLMImageMatches(st, id, cli) {
		t.Fatal("legacy ownership was not limited to the receipt-bound interpreter image")
	}
	recoveryStarts := 0
	control := vllmRuntimeControl{
		write:         func(record vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, record) },
		detect:        func(bool) error { return nil },
		recoveryStart: func(context.Context) error { recoveryStarts++; return nil },
	}
	if err := (&Executor{}).restoreManagedVLLM(context.Background(), st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: id}, "", true, control); err != nil || recoveryStarts != 0 {
		t.Fatalf("legacy rollback recovery starts=%d err=%v", recoveryStarts, err)
	}
	receipt.WheelSHA256 = strings.Repeat("0", 64)
	if err := validateVLLMReceiptMetadata(st, id, receipt); err == nil {
		t.Fatal("uncatalogued legacy receipt was trusted")
	}
}

func TestManagedVLLMSelectionMustStayInsideRetainedModelRoot(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "selected")
	if err := validateManagedVLLMSelection(root); err == nil {
		t.Fatal("missing selection was accepted")
	}
	id := "owner/model@" + strings.Repeat("a", 40)
	st := &engineState{modelDir: root}
	dir, err := vllmModelPath(st, id)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		t.Fatal(err)
	}
	data := []byte(`{"model_type":"fixture"}`)
	configSHA := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	files := []vllmModelFile{{Path: "config.json", Size: int64(len(data)), SHA256: hex.EncodeToString(configSHA[:])}}
	manifest, _ := json.Marshal(files)
	digest := sha256.Sum256(manifest)
	if err := writeVLLMJSON(dir, filepath.Join(dir, "pair-model.json"), vllmModel{ID: id, Source: "huggingface", Revision: strings.Repeat("a", 40), License: "fixture", Digest: hex.EncodeToString(digest[:]), Bytes: int64(len(data)), Files: files}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(dir), selected); err != nil {
		t.Skipf("symlink unavailable on this host: %v", err)
	}
	if err := validateManagedVLLMSelection(root); err != nil {
		t.Fatalf("owned selection rejected: %v", err)
	}
	if err := os.Remove(selected); err != nil {
		t.Fatal(err)
	}
	escape := t.TempDir()
	if err := os.Symlink(escape, selected); err != nil {
		t.Skipf("symlink unavailable on this host: %v", err)
	}
	if err := validateManagedVLLMSelection(root); err == nil {
		t.Fatalf("escaping selection error = %v", err)
	}
}

func managedVLLMTestState(t *testing.T) *engineState {
	t.Helper()
	root := t.TempDir()
	recipe := testVLLMRecipe(t)
	return &engineState{
		manifest:   &Manifest{Engine: "vllm", DisplayName: "vLLM"},
		plat:       &Platform{Install: &Install{Fetch: &Fetch{URL: recipe.UVURL, SHA256: recipe.UVSHA256}}, Runtime: Runtime{Port: 0}},
		installDir: filepath.Join(root, "runtime"),
		modelDir:   filepath.Join(root, "models"),
		logs:       newLogBuffer(),
	}
}

func testVLLMRecipe(t *testing.T) vllmAdmittedRecipe {
	t.Helper()
	for _, recipe := range managedVLLMOwnershipRecipes {
		if recipe.Architecture == runtime.GOARCH {
			return recipe
		}
	}
	t.Skipf("no managed vLLM fixture recipe for %s", runtime.GOARCH)
	return vllmAdmittedRecipe{}
}

func writeManagedVLLMTestEnvironment(t *testing.T, st *engineState, id, version string) string {
	recipe := testVLLMRecipe(t)
	recipe.Version = version
	return writeManagedVLLMTestEnvironmentWithRecipe(t, st, id, recipe)
}

func writeManagedVLLMTestEnvironmentWithRecipe(t *testing.T, st *engineState, id string, recipe vllmAdmittedRecipe) string {
	t.Helper()
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "venv", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(bin, "python")
	cli := filepath.Join(bin, "vllm")
	if err := os.WriteFile(python, []byte("fixture-python"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("fixture-vllm"), 0o700); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "pip-report.json")
	if err := os.WriteFile(report, []byte(`{"version":"1","pip_version":"fixture","install":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pythonHash, err := managedVLLMFileHash(dir, python)
	if err != nil {
		t.Fatal(err)
	}
	reportHash, err := managedVLLMFileHash(dir, report)
	if err != nil {
		t.Fatal(err)
	}
	cliHash, err := managedVLLMFileHash(dir, cli)
	if err != nil {
		t.Fatal(err)
	}
	rootAbs, _ := filepath.Abs(st.installDir)
	dirAbs, _ := filepath.Abs(dir)
	receipt := vllmRuntimeReceipt{
		Schema: vllmEnvironmentReceiptSchema, Engine: "vllm", RecipeID: recipe.RecipeID, OwnerRoot: rootAbs,
		Environment: dirAbs, Version: recipe.Version, Architecture: recipe.Architecture,
		UVURL: recipe.UVURL, UVSHA256: recipe.UVSHA256,
		PythonSHA256: pythonHash, CLISHA256: cliHash, PipReportSHA256: reportHash, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmEnvironmentReceiptFile), receipt); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestManagedVLLMSchema2RequiresBoundDependencyReport(t *testing.T) {
	st := managedVLLMTestState(t)
	id := "v0-dependencies"
	dir := writeManagedVLLMTestEnvironment(t, st, id, managedVLLMVersion)
	if err := os.Remove(filepath.Join(dir, "pip-report.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, _, err := validateVLLMEnvironment(st, id); err == nil {
		t.Fatal("schema2 runtime without its dependency report was admitted")
	}
}

func TestManagedVLLMOutdatedPreviousRollbackUsesOwnershipCatalogOnly(t *testing.T) {
	st := managedVLLMTestState(t)
	current := testVLLMRecipe(t)
	legacy := current
	legacy.RecipeID, legacy.Version = "vllm-0.28.0-test-recovery", "0.28.0"
	originalOwnership := managedVLLMOwnershipRecipes
	managedVLLMOwnershipRecipes = append(append([]vllmAdmittedRecipe{}, originalOwnership...), legacy)
	t.Cleanup(func() { managedVLLMOwnershipRecipes = originalOwnership })
	oldID, newID := "v0-old", "v0-new"
	writeManagedVLLMTestEnvironmentWithRecipe(t, st, oldID, legacy)
	writeManagedVLLMTestEnvironmentWithRecipe(t, st, newID, current)
	record := vllmRuntimeRecord{
		Schema: vllmRuntimeRecordSchema, Active: newID, Previous: oldID,
		Activating: &vllmActivationIntent{Candidate: newID, PriorActive: oldID, WasRunning: true},
	}
	if err := writeVLLMRuntimeRecord(st, record); err != nil {
		t.Fatal(err)
	}
	restarted := 0
	control := vllmRuntimeControl{
		stop: func() error { return nil }, start: func(context.Context) error { return nil },
		recoveryStart: func(context.Context) error { restarted++; return nil }, detect: func(bool) error { return nil },
		write: func(record vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, record) },
	}
	if err := (&Executor{reporter: NewReporter(nil)}).reconcileManagedVLLMActivationWithControl(context.Background(), st, control); err != nil {
		t.Fatal(err)
	}
	restored, err := readVLLMRuntimeRecord(st)
	if err != nil || restored.Active != oldID || restarted != 1 {
		t.Fatalf("outdated rollback = %+v restarted=%d err=%v", restored, restarted, err)
	}
	_, oldReceipt, err := validateVLLMEnvironment(st, oldID)
	if err != nil || admittedManagedVLLMReceipt(oldReceipt) {
		t.Fatalf("legacy runtime was not ownership-only: admitted=%v err=%v", admittedManagedVLLMReceipt(oldReceipt), err)
	}
}

func TestManagedVLLMFailedActivationRestoresPreviousPointer(t *testing.T) {
	st := managedVLLMTestState(t)
	oldID, newID := "v0-old", "v0-new"
	writeManagedVLLMTestEnvironment(t, st, oldID, managedVLLMVersion)
	writeManagedVLLMTestEnvironment(t, st, newID, managedVLLMVersion)
	old := vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: oldID, Staged: newID}
	if err := writeVLLMRuntimeRecord(st, old); err != nil {
		t.Fatal(err)
	}
	err := (&Executor{reporter: NewReporter(nil), client: newEngineHTTPClient(time.Second)}).activateManagedVLLM(
		context.Background(), st, old, newID, false,
		vllmRuntimeControl{stop: func() error { return nil }, start: func(context.Context) error { return nil }},
	)
	if err == nil {
		t.Fatal("invalid staged runtime unexpectedly activated")
	}
	restored, readErr := readVLLMRuntimeRecord(st)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if restored.Active != oldID || restored.Staged != "" || !containsString(restored.Retired, newID) {
		t.Fatalf("rollback record = %+v", restored)
	}
}

func TestManagedVLLMUninstallRemovesOnlyReceiptOwnedRuntime(t *testing.T) {
	st := managedVLLMTestState(t)
	id := "v0-active"
	environment := writeManagedVLLMTestEnvironment(t, st, id, managedVLLMVersion)
	if err := os.MkdirAll(st.modelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(st.modelDir, "keep-model")
	if err := os.WriteFile(sentinel, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMRuntimeRecord(st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: id}); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{reporter: NewReporter(nil), client: newEngineHTTPClient(time.Second)}
	var removed []string
	err := ex.uninstallManagedVLLMWithIO(context.Background(), st, func(path string) error {
		removed = append(removed, path)
		return os.RemoveAll(path)
	}, func(record vllmRuntimeRecord) error {
		return writeVLLMRuntimeRecord(st, record)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{environment}) {
		t.Fatalf("removed paths = %v", removed)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("retained model data was removed: %v", err)
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || len(vllmRecordedIDs(record)) != 0 || record.Removing {
		t.Fatalf("final ownership record = %+v, err=%v", record, err)
	}
}

func TestManagedVLLMUninstallResumesFromDurablePartialRemoval(t *testing.T) {
	st := managedVLLMTestState(t)
	previousID, activeID := "v0-previous", "v0-active"
	previousDir := writeManagedVLLMTestEnvironment(t, st, previousID, managedVLLMVersion)
	activeDir := writeManagedVLLMTestEnvironment(t, st, activeID, managedVLLMVersion)
	if err := writeVLLMRuntimeRecord(st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: activeID, Previous: previousID}); err != nil {
		t.Fatal(err)
	}
	ex := &Executor{reporter: NewReporter(nil), client: newEngineHTTPClient(time.Second)}
	injected := true
	remove := func(path string) error {
		if path == activeDir && injected {
			injected = false
			return os.ErrPermission
		}
		return os.RemoveAll(path)
	}
	write := func(record vllmRuntimeRecord) error { return writeVLLMRuntimeRecord(st, record) }
	if err := ex.uninstallManagedVLLMWithIO(context.Background(), st, remove, write); err == nil {
		t.Fatal("partial removal unexpectedly reported success")
	}
	record, err := readVLLMRuntimeRecord(st)
	if err != nil || !record.Removing || len(record.RemovalProofs) != 2 {
		t.Fatalf("durable removal journal = %+v, err=%v", record, err)
	}
	if _, err := os.Stat(previousDir); !os.IsNotExist(err) {
		t.Fatalf("first receipt-owned runtime was not removed: %v", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("failed runtime was not retained for retry: %v", err)
	}
	if err := ex.uninstallManagedVLLMWithIO(context.Background(), st, remove, write); err != nil {
		t.Fatalf("resume removal: %v", err)
	}
	record, err = readVLLMRuntimeRecord(st)
	if err != nil || len(vllmRecordedIDs(record)) != 0 || record.Removing {
		t.Fatalf("final ownership record = %+v, err=%v", record, err)
	}
}

func TestPublicVLLMUninstallResumesAfterFinalRecordClearFailure(t *testing.T) {
	recipe := testVLLMRecipe(t)
	key := runtime.GOOS + "/" + runtime.GOARCH
	ex := newTestExecutor(t, &Manifest{
		Engine: "vllm", DisplayName: "vLLM", ManifestVersion: 1,
		Platforms: map[string]Platform{key: {
			Install:   &Install{Driver: "vllm-python", Fetch: &Fetch{URL: recipe.UVURL, SHA256: recipe.UVSHA256}},
			Uninstall: &Uninstall{Driver: "vllm-python"}, Runtime: Runtime{Driver: "vllm-python"},
		}},
	})
	st, err := ex.state("vllm")
	if err != nil {
		t.Fatal(err)
	}
	id := "v0-active"
	dir := writeManagedVLLMTestEnvironment(t, st, id, managedVLLMVersion)
	_, receipt, err := validateVLLMEnvironment(st, id)
	if err != nil {
		t.Fatal(err)
	}
	record := vllmRuntimeRecord{
		Schema: vllmRuntimeRecordSchema, Active: id, Removing: true,
		RemovalProofs: map[string]vllmRuntimeReceipt{id: receipt},
	}
	if err := writeVLLMRuntimeRecord(st, record); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := ex.Uninstall(context.Background(), "vllm"); err != nil {
		t.Fatalf("public removal resume: %v", err)
	}
	final, err := readVLLMRuntimeRecord(st)
	if err != nil || final.Removing || len(vllmRecordedIDs(final)) != 0 {
		t.Fatalf("final record = %+v err=%v", final, err)
	}
}

func TestManagedVLLMStatusFailsClosedForCorruptAndRemovingRecords(t *testing.T) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	newExecutor := func() (*Executor, *engineState) {
		ex := newTestExecutor(t, &Manifest{
			Engine: "vllm", DisplayName: "vLLM", ManifestVersion: 1,
			Platforms: map[string]Platform{key: {Runtime: Runtime{Bin: "missing", Port: 0}}},
		})
		st, err := ex.state("vllm")
		if err != nil {
			t.Fatal(err)
		}
		return ex, st
	}
	t.Run("corrupt", func(t *testing.T) {
		ex, st := newExecutor()
		if err := os.MkdirAll(st.installDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(st.installDir, vllmRuntimeRecordFile), []byte(`{"schema":999}`), 0o600); err != nil {
			t.Fatal(err)
		}
		status, err := ex.Status("vllm")
		if err != nil || status.InstallSupported == nil || *status.InstallSupported || !strings.Contains(status.InstallReason, "Managed state unavailable") {
			t.Fatalf("status=%+v err=%v", status, err)
		}
		installed := ex.GetInstalled()
		if len(installed) != 1 || installed[0].InstallSupported == nil || *installed[0].InstallSupported || !strings.Contains(installed[0].InstallReason, "Managed state unavailable") {
			t.Fatalf("installed=%+v", installed)
		}
	})
	t.Run("removing", func(t *testing.T) {
		ex, st := newExecutor()
		recipe := testVLLMRecipe(t)
		st.plat.Install = &Install{Fetch: &Fetch{URL: recipe.UVURL, SHA256: recipe.UVSHA256}}
		id := "v0-active"
		writeManagedVLLMTestEnvironment(t, st, id, managedVLLMVersion)
		_, receipt, err := validateVLLMEnvironment(st, id)
		if err != nil {
			t.Fatal(err)
		}
		record := vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: id, Removing: true, RemovalProofs: map[string]vllmRuntimeReceipt{id: receipt}}
		if err := writeVLLMRuntimeRecord(st, record); err != nil {
			t.Fatal(err)
		}
		status, err := ex.Status("vllm")
		if err != nil || status.InstallSupported == nil || *status.InstallSupported || !strings.Contains(status.InstallReason, "removal") {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	})
}

func TestReceiptOwnedVLLMOrphanIsManagedAndForeignImageIsNot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed vLLM orphan ownership is a Linux-only runtime contract")
	}
	st := managedVLLMTestState(t)
	id := "v0-active"
	dir, err := vllmEnvironmentPath(st, id)
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(dir, "venv", "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(fakeEngineBin)
	if err != nil {
		t.Fatal(err)
	}
	python, cli := filepath.Join(binDir, "python"), filepath.Join(binDir, "vllm")
	for _, path := range []string{python, cli} {
		if err := os.WriteFile(path, fixture, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	report := filepath.Join(dir, "pip-report.json")
	if err := os.WriteFile(report, []byte(`{"version":"1","pip_version":"fixture","install":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pythonHash, _ := managedVLLMFileHash(dir, python)
	cliHash, _ := managedVLLMFileHash(dir, cli)
	reportHash, _ := managedVLLMFileHash(dir, report)
	recipe := testVLLMRecipe(t)
	rootAbs, _ := filepath.Abs(st.installDir)
	dirAbs, _ := filepath.Abs(dir)
	receipt := vllmRuntimeReceipt{
		Schema: vllmEnvironmentReceiptSchema, RecipeID: recipe.RecipeID, Engine: "vllm", OwnerRoot: rootAbs,
		Environment: dirAbs, Version: recipe.Version, Architecture: recipe.Architecture,
		UVURL: recipe.UVURL, UVSHA256: recipe.UVSHA256, PythonSHA256: pythonHash, CLISHA256: cliHash, PipReportSHA256: reportHash,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeManagedVLLMJSON(st.installDir, filepath.Join(dir, vllmEnvironmentReceiptFile), receipt); err != nil {
		t.Fatal(err)
	}
	if err := writeVLLMRuntimeRecord(st, vllmRuntimeRecord{Schema: vllmRuntimeRecordSchema, Active: id}); err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	st.port = port
	st.plat.Runtime.Port = port
	st.plat.Runtime.Ready = &Probe{HTTP: "http://127.0.0.1:{port}/v1/models", Status: 200, Identity: "vllm"}
	cmd := exec.Command(python)
	cmd.Env = append(os.Environ(), "FAKE_ADDR=127.0.0.1:"+fmt.Sprint(port), "FAKE_VLLM=1")
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { terminatePID(cmd.Process.Pid, 20*time.Millisecond) })
	ex := &Executor{
		reporter: NewReporter(nil), client: newEngineHTTPClient(time.Second), desired: newDesiredStateStore(t.TempDir()),
		emit: func(string, any) {}, engines: map[string]*engineState{"vllm": st},
	}
	readyCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ex.waitReady(readyCtx, st.plat.Runtime.Ready, port); err != nil {
		t.Fatal(err)
	}
	owned, occupied, err := ex.observeManagedVLLMListener(context.Background(), st, id)
	if err != nil || !owned || !occupied || st.adopted {
		t.Fatalf("owned=%v occupied=%v adopted=%v err=%v", owned, occupied, st.adopted, err)
	}
	t.Run("expired observation preserves cached health", func(t *testing.T) {
		if !st.healthy {
			t.Fatal("owned listener was not healthy before the expired observation")
		}
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if ex.probe(ctx, st.plat.Runtime.Ready, port) {
			t.Fatal("canceled identity probe unexpectedly succeeded")
		}
		owned, occupied, err := ex.observeManagedVLLMListener(ctx, st, id)
		if err != nil || !owned || !occupied || !st.running || !st.healthy || st.adopted {
			t.Fatalf("expired observation changed owned listener state: owned=%v occupied=%v running=%v healthy=%v adopted=%v err=%v", owned, occupied, st.running, st.healthy, st.adopted, err)
		}
	})
	if managedVLLMImageMatches(st, id, os.Args[0]) {
		t.Fatal("foreign process image matched receipt ownership")
	}
	if err := ex.doStop(st, "vllm"); err != nil {
		t.Fatal(err)
	}
}
