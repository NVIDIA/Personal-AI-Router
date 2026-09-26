// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func historyExistingFixture(current bool) historyExistingInstallation {
	return historyExistingFixtureWithComponents(current, 15)
}

func historyExistingFixtureWithComponents(current bool, componentCount int) historyExistingInstallation {
	home := "/home/test"
	digest := strings.Repeat("1", 64)
	executableSHA := strings.Repeat("2", 64)
	bundle := home + "/.local/share/Nvidia Corporation/Personal AI Router/bundles/" + digest + "/bin"
	executable := bundle + "/nvpair-tui"
	broker := bundle + "/nvpair-ui-broker"
	config := home + "/.config"
	unit := historyUpgradeUnitText(executable, broker, config, 25)
	unitHash := sha256.Sum256([]byte(unit))
	binaries := onboardingSupportedBinaries(componentCount)
	components := make([]historyInstalledComponent, 0, len(binaries))
	for _, name := range binaries {
		hash := strings.Repeat("3", 64)
		if name == "nvpair-tui" {
			hash = executableSHA
		}
		components = append(components, historyInstalledComponent{Name: name, SHA256: hash, Bytes: 1})
	}
	existing := historyExistingInstallation{
		NodeID: "node", ClusterID: "cluster", CertFingerprint: "sha256:" + strings.Repeat("4", 64), Version: "1.0.0", SourceFingerprint: strings.Repeat("5", 64),
		UID: 1000, Home: home, ConfigHome: config, StartupLifetime: "session", Unit: historyUpgradeUnit, UnitPath: config + "/systemd/user/" + historyUpgradeUnit,
		UnitSHA256: hex.EncodeToString(unitHash[:]), UnitBytes: unit, Bundle: bundle, Executable: executable, Broker: broker, ManifestSHA256: strings.Repeat("6", 64),
		ProcessID: 1, ProcessStartTicks: "1", ExecutableSHA256: executableSHA, IdentitySHA256: strings.Repeat("7", 64), NodeIdentitySHA256: strings.Repeat("8", 64),
		LauncherPath: home + "/.local/bin/nvpair", Components: components,
	}
	if current {
		existing.Retention = &historyRetentionReview{Owner: "absent", ActiveDigest: digest, HeldPruneDigests: []string{}, NextHeldPruneDigests: []string{}, NextGeneration: 1}
	}
	return existing
}

func TestOnboardingHistoryAcceptsExactSupportedBundleGenerations(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, onboardingHistoryDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	digests := func(values ...string) []string {
		out := make([]string, 0, len(values))
		for _, value := range values {
			out = append(out, strings.Repeat(value, 64))
		}
		return out
	}
	tests := []struct {
		generation uint64
		rollback   string
		held       []string
		next       []string
	}{
		{generation: 3, rollback: strings.Repeat("d", 64), held: digests("a", "b"), next: digests("a", "b", "d")},
		{generation: 4, rollback: strings.Repeat("e", 64), held: digests("a", "b", "d"), next: digests("a", "b", "d", "e")},
		{generation: 5, rollback: strings.Repeat("f", 64), held: digests("a", "b", "d", "e"), next: digests("b", "d", "e", "f")},
	}
	for index, test := range tests {
		id := fmt.Sprintf("%032x", index+1)
		plan, target := historyPlanFixture("upgrade", true)
		existing := historyExistingFixtureWithComponents(true, 12)
		existing.Retention = &historyRetentionReview{
			Owner: "nvidia-pair-bundle-retention-v2", ActiveDigest: strings.Repeat("1", 64), RollbackDigest: test.rollback,
			HeldPruneDigests: test.held, Operation: fmt.Sprintf("%032x", index+100), Generation: test.generation,
			RegistrySHA256: strings.Repeat("f", 64), NextHeldPruneDigests: test.next, NextGeneration: test.generation + 1,
		}
		plan.ExistingInstallation = &existing
		summary := existing.summary()
		plan.Review.ExistingInstallation = &summary
		plan.Receipt.Installed, plan.Receipt.ServiceInstalled, plan.Receipt.ServiceStarted = true, true, true
		target.Stage = "paired"
		run := onboardingHistoryRun{Operation: historyOperation{OperationID: id, Revision: 1, State: "completed", FinishedAt: 1, Targets: []historyTargetState{target}}, Plans: map[string]historyPlan{"candidate": plan}}
		data, err := json.Marshal(run)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := loadOnboardingHistory(base)
	if got.Total != 3 || got.Current != 3 || got.Invalid != 0 || got.RecoveryRequired || got.DiscoveryBlocked {
		t.Fatalf("summary = %+v", got)
	}
}

func TestOnboardingHistoryComponentShapesMatchEffectfulValidator(t *testing.T) {
	info := historyPlatformInfo{OS: "Linux", Arch: "x86_64", Home: "/home/test", UID: 1000, ExistingPAIR: true, UserRuntime: true}
	for _, count := range []int{12, 13, 15} {
		if err := validateHistoryExisting(historyExistingFixtureWithComponents(true, count), info); err != nil {
			t.Fatalf("supported %d-component inventory: %v", count, err)
		}
	}
	tests := []struct {
		name   string
		mutate func(*historyExistingInstallation)
	}{
		{name: "missing", mutate: func(existing *historyExistingInstallation) {
			existing.Components = existing.Components[:len(existing.Components)-1]
		}},
		{name: "extra", mutate: func(existing *historyExistingInstallation) {
			existing.Components = append(existing.Components, historyInstalledComponent{Name: "extra-proxy", SHA256: strings.Repeat("a", 64), Bytes: 1})
		}},
		{name: "renamed", mutate: func(existing *historyExistingInstallation) {
			existing.Components[len(existing.Components)-1].Name = "renamed-proxy"
		}},
		{name: "duplicate", mutate: func(existing *historyExistingInstallation) {
			existing.Components[len(existing.Components)-1].Name = existing.Components[0].Name
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			existing := historyExistingFixtureWithComponents(true, 12)
			test.mutate(&existing)
			if err := validateHistoryExisting(existing, info); err == nil {
				t.Fatal("invalid unified component inventory was accepted")
			}
		})
	}
}

func historyPlanFixture(action string, current bool) (historyPlan, historyTargetState) {
	plan := historyPlan{
		Candidate: historyCandidate{CandidateID: "candidate", Address: "192.0.2.10"},
		Info:      historyPlatformInfo{OS: "Linux", Arch: "x86_64", Home: "/home/test", UID: 1000, ExistingPAIR: true, UserRuntime: true},
		Artifact:  historyArtifactSource{historyArtifact: historyArtifact{SHA256: strings.Repeat("9", 64)}},
		Review:    historyReviewTarget{Action: action},
		Receipt:   historyInstallReceipt{CleanupConfirmed: true},
	}
	target := historyTargetState{CandidateID: "candidate", CleanupConfirmed: true}
	if action == "upgrade" {
		existing := historyExistingFixture(current)
		plan.ExistingInstallation = &existing
		summary := existing.summary()
		plan.Review.ExistingInstallation = &summary
		plan.UpgradePhase = "retired"
		plan.Receipt.NodeID = existing.NodeID
		plan.Receipt.ArtifactSHA256 = plan.Artifact.SHA256
		target.NodeID = existing.NodeID
	}
	return plan, target
}

func writeHistoryFixture(t *testing.T, base string, ordinal int, state, action, phase string, currentMarker bool) {
	t.Helper()
	id := fmt.Sprintf("%032x", ordinal)
	plan, target := historyPlanFixture(action, currentMarker)
	stage, cleanup := "paired", true
	installed, serviceInstalled, serviceStarted := true, true, true
	canRetry, canCancel := false, false
	if state == "cancelled" {
		stage = "cancelled"
		if action == "upgrade" {
			phase = "cancelled-before-stop"
		} else {
			phase = ""
		}
		installed, serviceInstalled, serviceStarted = false, false, false
		canRetry, canCancel = true, true
	}
	plan.UpgradePhase = phase
	plan.Receipt.Installed, plan.Receipt.ServiceInstalled, plan.Receipt.ServiceStarted, plan.Receipt.CleanupConfirmed = installed, serviceInstalled, serviceStarted, cleanup
	target.Stage, target.CanRetry, target.CanCancel, target.CleanupConfirmed = stage, canRetry, canCancel, cleanup
	run := onboardingHistoryRun{Operation: historyOperation{OperationID: id, Revision: 1, State: state, FinishedAt: 1, Targets: []historyTargetState{target}}, Plans: map[string]historyPlan{"candidate": plan}}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, onboardingHistoryDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingHistoryRejectsReducedSchemaCounterexample(t *testing.T) {
	base := t.TempDir()
	writeHistoryFixture(t, base, 1, "completed", "upgrade", "retired", false)
	file := filepath.Join(base, onboardingHistoryDir, fmt.Sprintf("%032x.json", 1))
	var run onboardingHistoryRun
	data, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &run) != nil {
		t.Fatal(err)
	}
	plan := run.Plans["candidate"]
	plan.Candidate.Address = ""
	run.Plans["candidate"] = plan
	data, err = json.Marshal(run)
	if err != nil || os.WriteFile(file, data, 0o600) != nil {
		t.Fatal(err)
	}
	got := loadOnboardingHistory(base)
	if got.Invalid != 1 || !got.RecoveryRequired || !got.DiscoveryBlocked || got.Operations[0].Classification != "invalid" {
		t.Fatalf("summary = %+v", got)
	}
}

func TestOnboardingHistoryMixedCurrentAndLegacyIsReadOnlyHistory(t *testing.T) {
	base := t.TempDir()
	id := fmt.Sprintf("%032x", 1)
	legacy, legacyTarget := historyPlanFixture("upgrade", false)
	legacy.Candidate.CandidateID, legacyTarget.CandidateID = "legacy", "legacy"
	legacy.Receipt.Installed, legacy.Receipt.ServiceInstalled, legacy.Receipt.ServiceStarted = true, true, true
	legacyTarget.Stage = "paired"
	current, currentTarget := historyPlanFixture("upgrade", true)
	current.Candidate.CandidateID, currentTarget.CandidateID = "current", "current"
	current.Receipt.Installed, current.Receipt.ServiceInstalled, current.Receipt.ServiceStarted = true, true, true
	currentTarget.Stage = "paired"
	run := onboardingHistoryRun{Operation: historyOperation{OperationID: id, Revision: 1, State: "completed", FinishedAt: 1, Targets: []historyTargetState{legacyTarget, currentTarget}}, Plans: map[string]historyPlan{"legacy": legacy, "current": current}}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, onboardingHistoryDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadOnboardingHistory(base)
	if got.HistoryOnly != 1 || got.Invalid != 0 || got.RecoveryRequired || got.DiscoveryBlocked || got.Operations[0].MutationAllowed {
		t.Fatalf("summary = %+v", got)
	}
}

func TestOnboardingHistoryCurrentCancelledUpgradeRemainsCurrent(t *testing.T) {
	base := t.TempDir()
	writeHistoryFixture(t, base, 1, "cancelled", "upgrade", "cancelled-before-stop", true)
	got := loadOnboardingHistory(base)
	if got.Current != 1 || got.Invalid != 0 || got.RecoveryRequired || got.DiscoveryBlocked || got.Operations[0].MutationAllowed {
		t.Fatalf("summary = %+v", got)
	}
}

func TestOnboardingHistoryCurrentCancelledAfterRollbackRequiresVerifiedCleanup(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*historyPlan, *historyTargetState)
		valid  bool
	}{
		{name: "verified", valid: true},
		{name: "target cleanup unconfirmed", mutate: func(_ *historyPlan, target *historyTargetState) { target.CleanupConfirmed = false }},
		{name: "receipt cleanup unconfirmed", mutate: func(plan *historyPlan, _ *historyTargetState) { plan.Receipt.CleanupConfirmed = false }},
		{name: "successor service installed", mutate: func(plan *historyPlan, _ *historyTargetState) { plan.Receipt.ServiceInstalled = true }},
		{name: "successor service running", mutate: func(plan *historyPlan, _ *historyTargetState) { plan.Receipt.ServiceStarted = true }},
		{name: "successor not recoverable", mutate: func(plan *historyPlan, _ *historyTargetState) { plan.Receipt.Recoverable = false }},
		{name: "peer identity unbound", mutate: func(_ *historyPlan, target *historyTargetState) { target.NodeID = "other" }},
		{name: "artifact unbound", mutate: func(plan *historyPlan, _ *historyTargetState) { plan.Receipt.ArtifactSHA256 = strings.Repeat("a", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			id := fmt.Sprintf("%032x", 1)
			plan, target := historyPlanFixture("upgrade", true)
			plan.UpgradePhase = "rolled-back"
			plan.Receipt.Installed, plan.Receipt.Recoverable = true, true
			plan.Receipt.ServiceInstalled, plan.Receipt.ServiceStarted = false, false
			target.Stage, target.CanRetry, target.CanCancel = "cancelled", true, true
			if test.mutate != nil {
				test.mutate(&plan, &target)
			}
			run := onboardingHistoryRun{Operation: historyOperation{OperationID: id, Revision: 1, State: "cancelled", FinishedAt: 1, Targets: []historyTargetState{target}}, Plans: map[string]historyPlan{"candidate": plan}}
			data, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(base, onboardingHistoryDir)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			got := loadOnboardingHistory(base)
			if test.valid {
				if got.Current != 1 || got.Invalid != 0 || got.RecoveryRequired || got.DiscoveryBlocked || got.Operations[0].MutationAllowed {
					t.Fatalf("summary = %+v", got)
				}
			} else if got.Invalid != 1 || !got.RecoveryRequired || !got.DiscoveryBlocked || got.Operations[0].Classification != "invalid" {
				t.Fatalf("summary = %+v", got)
			}
		})
	}
}

func TestOnboardingHistoryCountOverflowIsReconciledAndFailClosed(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, onboardingHistoryDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= onboardingHistoryMaxFiles; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%04d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := loadOnboardingHistory(base)
	if err := got.validate(); err != nil || got.Total != 1 || got.Invalid != 1 || !got.RecoveryRequired || !got.DiscoveryBlocked {
		t.Fatalf("summary = %+v, validate = %v", got, err)
	}
}

func TestReadOnboardingHistoryFileEnforcesBoundAndObjectIdentity(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "record.json")
	if err := os.WriteFile(file, make([]byte, onboardingHistoryMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnboardingHistoryFile(file); err == nil {
		t.Fatal("oversize history file was admitted")
	}
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnboardingHistoryFileAfterStat(file, func() {
		if truncateErr := os.Truncate(file, onboardingHistoryMaxBytes+1); truncateErr != nil {
			t.Fatal(truncateErr)
		}
	}); err == nil {
		t.Fatal("history file growth after initial stat was admitted")
	}
	nonregular := filepath.Join(dir, "directory")
	if err := os.Mkdir(nonregular, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnboardingHistoryFile(nonregular); err == nil {
		t.Fatal("non-regular history path was admitted")
	}
}

func TestOnboardingHistoryLoadsTwentyTwoWithoutGrantingAuthority(t *testing.T) {
	base := t.TempDir()
	for i := 1; i <= 18; i++ {
		writeHistoryFixture(t, base, i, "completed", "upgrade", "retired", false)
	}
	writeHistoryFixture(t, base, 19, "cancelled", "upgrade", "cancelled-before-stop", false)
	writeHistoryFixture(t, base, 20, "completed", "", "", false)
	writeHistoryFixture(t, base, 21, "cancelled", "", "", false)
	writeHistoryFixture(t, base, 22, "completed", "upgrade", "retired", true)

	got := loadOnboardingHistory(base)
	if err := got.validate(); err != nil {
		t.Fatal(err)
	}
	if got.Total != 22 || got.HistoryOnly != 19 || got.Current != 3 || got.Invalid != 0 || got.RecoveryRequired || got.DiscoveryBlocked || got.MutationSupported {
		t.Fatalf("summary = %+v", got)
	}
	for _, operation := range got.Operations {
		if operation.MutationAllowed {
			t.Fatalf("history view granted mutation authority: %+v", operation)
		}
	}
}

func TestOnboardingHistoryKeepsNonterminalMissingRetentionFailClosed(t *testing.T) {
	base := t.TempDir()
	writeHistoryFixture(t, base, 1, "running", "upgrade", "staging", false)
	writeHistoryFixture(t, base, 2, "running", "", "", false)
	got := loadOnboardingHistory(base)
	if got.Invalid != 2 || !got.RecoveryRequired || !got.DiscoveryBlocked || got.Operations[0].Classification != "invalid" || got.Operations[1].Classification != "invalid" {
		t.Fatalf("summary = %+v", got)
	}
}

func TestManagerExposesReadOnlyOnboardingHistory(t *testing.T) {
	base := t.TempDir()
	writeHistoryFixture(t, base, 1, "completed", "upgrade", "retired", false)
	var out bytes.Buffer
	ex := NewExecutor(NewRegistry(), NewReporter(nil), func(string, any) {}, base)
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.handleMessage(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:onboarding-history"})
	if !strings.Contains(out.String(), `"history_only":1`) || !strings.Contains(out.String(), `"mutation_supported":false`) {
		t.Fatalf("response = %s", out.String())
	}
}
