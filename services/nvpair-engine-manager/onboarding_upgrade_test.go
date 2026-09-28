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
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
)

func onboardingUpgradeFixture(t *testing.T, count int) (onboardingPlan, string) {
	t.Helper()
	info, pkg, id := onboardingInstallFixture(t)
	info.ExistingPAIR, info.UserRuntime = true, true
	d := onboardingExistingInstallation{NodeID: "paired-peer", ClusterID: "same-cluster", CertFingerprint: "sha256:" + strings.Repeat("c", 64), Version: "0.91.7", SourceFingerprint: strings.Repeat("d", 64), UID: info.UID, Home: info.Home, ConfigHome: path.Join(info.Home, ".config"), StartupLifetime: "session", Unit: onboardingUpgradeUnit, Bundle: path.Join(info.Home, "application", "resources", "cli-bin"), ManifestSHA256: strings.Repeat("e", 64), ProcessID: 1234, ProcessStartTicks: "5555", IdentitySHA256: strings.Repeat("a", 64), NodeIdentitySHA256: strings.Repeat("b", 64), LauncherPath: path.Join(info.Home, ".local", "bin", "nvpair"), LegacyPackageStatus: "absent"}
	d.Executable, d.Broker = path.Join(d.Bundle, "nvpair-tui"), path.Join(d.Bundle, "nvpair-ui-broker")
	d.UnitPath = path.Join(d.ConfigHome, "systemd", "user", onboardingUpgradeUnit)
	d.UnitBytes = onboardingUpgradeUnitText(d.Executable, d.Broker, d.ConfigHome, 25)
	sum := sha256.Sum256([]byte(d.UnitBytes))
	d.UnitSHA256 = hex.EncodeToString(sum[:])
	component := onboardingFixtureELF(183)
	sum = sha256.Sum256(component)
	d.ExecutableSHA256 = hex.EncodeToString(sum[:])
	for _, name := range onboardingSupportedBinaries(count) {
		d.Components = append(d.Components, onboardingInstalledComponent{Name: name, SHA256: d.ExecutableSHA256, Bytes: int64(len(component))})
	}
	sort.Slice(d.Components, func(i, j int) bool { return d.Components[i].Name < d.Components[j].Name })
	summary := d.Summary()
	plan := onboardingPlan{Candidate: onboardingCandidate{CandidateID: strings.Repeat("a", 32), Address: "192.0.2.8", Port: 22}, Review: onboardingReviewTarget{Action: "upgrade", ExistingInstallation: &summary, StartupLifetime: "session", Platform: "linux", Arch: "arm64", Status: "ready"}, Info: info, Artifact: pkg.source, PackageFile: pkg.file, ArchiveRoot: pkg.archiveRoot, ArchiveBytes: pkg.bytes, Username: "approved-user", ExistingInstallation: &d}
	plan.Review.CandidateID = plan.Candidate.CandidateID
	plan.Receipt, _ = onboardingInstallPaths(info, pkg, id)
	plan.Receipt.ManifestSHA256, _ = onboardingPackageManifestHash(pkg)
	plan.Receipt.StartupLifetime = "session"
	plan.Receipt.NodeID = d.NodeID
	if !validOnboardingUpgradePlan(plan) {
		t.Fatal("invalid fixture plan")
	}
	return plan, id
}

type onboardingUpgradeFake struct {
	t                                       *testing.T
	plan                                    onboardingPlan
	phase                                   string
	staged, oldRunning, newRunning, newUnit bool
	calls                                   []string
	fail                                    string
	changeOld                               bool
}

func TestOnboardingUpgradeRollbackUsesRetainedProductOwner(t *testing.T) {
	if !strings.Contains(
		onboardingUpgradeServiceScript,
		"owner=existing['executable'] if h['phase']=='rollback' else tui",
	) || !strings.Contains(
		onboardingUpgradeServiceScript,
		"product([owner,'--headless-service','upgrade'",
	) {
		t.Fatal("rollback is not bound to the exact retained predecessor owner")
	}
}

func (f *onboardingUpgradeFake) run(ctx context.Context, command string, input io.Reader) ([]byte, error) {
	var data []byte
	if input != nil {
		data, _ = io.ReadAll(input)
	}
	if command == onboardingPython(onboardingReceiveScript) {
		f.calls = append(f.calls, "stage")
		if f.phase != "staging" || !f.oldRunning {
			f.t.Fatal("staging followed downtime or preceded durable intent")
		}
		parts := bytes.SplitN(data, []byte{'\n'}, 2)
		var h struct {
			Upgrade onboardingExistingInstallation `json:"upgrade"`
		}
		remote := onboardingUpgradeTransferDescriptor(*f.plan.ExistingInstallation)
		if len(parts) != 2 || json.Unmarshal(parts[0], &h) != nil || !reflect.DeepEqual(h.Upgrade, remote) {
			f.t.Fatal("staging did not bind the exact old installation")
		}
		f.staged = true
		receipt := f.plan.Receipt
		receipt.Installed, receipt.Recoverable = true, true
		return onboardingMarshal(receipt), nil
	}
	if command == onboardingPython(onboardingUpgradeInspectScript) {
		f.calls = append(f.calls, "inspect-old")
		old := *f.plan.ExistingInstallation
		if f.changeOld {
			old.ProcessStartTicks = "6666"
		}
		return onboardingMarshal(old), nil
	}
	if command == onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeServiceScript) {
		var h struct {
			Phase    string                         `json:"phase"`
			Existing onboardingExistingInstallation `json:"existing"`
		}
		if json.Unmarshal(data, &h) != nil || !reflect.DeepEqual(h.Existing, *f.plan.ExistingInstallation) {
			f.t.Fatal("lifecycle did not bind old ownership")
		}
		f.calls = append(f.calls, h.Phase)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 398*time.Second {
			f.t.Fatal("upgrade wrapper cannot accommodate the product drain")
		}
		states := map[string]string{"stop": "stopped", "install": "installed", "rollback": "rolled-back"}
		switch h.Phase {
		case "stop":
			if f.phase != "stopping" || !f.staged {
				f.t.Fatal("stop escaped staging/journal admission")
			}
			f.oldRunning = false
		case "install":
			if f.phase != "installing-unit" || f.oldRunning {
				f.t.Fatal("new unit replaced a live old unit")
			}
			f.newUnit = true
		case "rollback":
			if f.phase != "rolling-back" {
				f.t.Fatal("rollback preceded its durable intent")
			}
			f.newRunning = false
			f.newUnit = false
			f.oldRunning = true
		default:
			f.t.Fatal("unknown lifecycle phase")
		}
		if f.fail == h.Phase {
			f.fail = ""
			return nil, context.DeadlineExceeded
		}
		return onboardingMarshal(map[string]any{"unit": onboardingUpgradeUnit, "operation": "upgrade", "phase": h.Phase, "state": states[h.Phase], "cleanupConfirmed": true}), nil
	}
	if command == onboardingPython(onboardingOwnedScript+onboardingServiceScript) {
		var h struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(data, &h)
		if h.Action != "start" || f.phase != "starting" || !f.newUnit {
			f.t.Fatal("unexpected or premature service action")
		}
		f.calls = append(f.calls, "start")
		f.newRunning = true
		return onboardingMarshal(map[string]any{"unit": onboardingUpgradeUnit, "operation": "start", "state": "started", "requestedLifetime": "session", "effectiveLifetime": "session"}), nil
	}
	if command == onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeVerifyScript) {
		f.calls = append(f.calls, "verify-identity")
		if !f.newRunning {
			f.t.Fatal("identity confirmation preceded successor start")
		}
		d := f.plan.ExistingInstallation
		return onboardingMarshal(map[string]string{"nodeId": d.NodeID, "clusterId": d.ClusterID, "certFingerprint": d.CertFingerprint}), nil
	}
	if command == onboardingPython(onboardingOwnedScript+onboardingUpgradeCleanStageScript) {
		f.calls = append(f.calls, "clean-stage")
		return []byte(`{"stagingCleaned":true}`), nil
	}
	f.t.Fatal("unexpected command: upgrade must not pair, mint identity, uninstall or run arbitrary code")
	return nil, errors.New("unexpected command")
}

func TestOnboardingUpgradeTransferDescriptorOmitsOnlyControllerLegacyEvidence(t *testing.T) {
	plan, _ := onboardingUpgradeFixture(t, 15)
	original := *plan.ExistingInstallation
	original.LegacyPackageStatus = onboardingLegacyPackageAdmitted
	original.LegacyPackage = &onboardingLegacyPackageReceipt{Package: onboardingLegacyPackageName}
	remote := onboardingUpgradeTransferDescriptor(original)
	if remote.LegacyPackageStatus != "" || remote.LegacyPackage != nil {
		t.Fatal("controller-only legacy package evidence crossed the peer comparison boundary")
	}
	if original.LegacyPackageStatus != onboardingLegacyPackageAdmitted || original.LegacyPackage == nil {
		t.Fatal("transfer projection mutated the retained reviewed descriptor")
	}
	remote.NodeID = "changed-core-field"
	expected := original
	expected.LegacyPackageStatus, expected.LegacyPackage = "", nil
	expected.NodeID = "changed-core-field"
	if !reflect.DeepEqual(remote, expected) {
		t.Fatal("transfer projection removed or changed a remotely verifiable core field")
	}
}

func TestOnboardingUpgradeStageProjectsAdmittedLegacyPackageEvidence(t *testing.T) {
	plan, id := onboardingUpgradeFixture(t, 15)
	legacy, _ := onboardingLegacyPackageFixture()
	legacy.Rollback.Path = path.Join(plan.Info.Home, "Downloads", "nvpair_0.2.24_arm64.deb")
	legacy.Rollback.UID, legacy.Rollback.GID = plan.Info.UID, plan.Info.UID
	plan.ExistingInstallation.LegacyPackageStatus = onboardingLegacyPackageAdmitted
	plan.ExistingInstallation.LegacyPackage = &legacy
	summary := plan.ExistingInstallation.Summary()
	plan.Review.ExistingInstallation = &summary
	if !validOnboardingUpgradePlan(plan) {
		t.Fatal("admitted legacy-package fixture plan is invalid")
	}
	fake := &onboardingUpgradeFake{t: t, plan: plan, phase: "staging", oldRunning: true}
	before, _ := json.Marshal(plan.ExistingInstallation)
	if _, err := stageOnboardingWithRunner(context.Background(), fake, plan.Info, onboardingPackage{source: plan.Artifact, file: plan.PackageFile, archiveRoot: plan.ArchiveRoot, bytes: plan.ArchiveBytes}, id, plan.Review.StartupLifetime, plan.ExistingInstallation, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(plan.ExistingInstallation)
	if len(fake.calls) != 1 || !bytes.Equal(before, after) || plan.ExistingInstallation.LegacyPackageStatus != onboardingLegacyPackageAdmitted || plan.ExistingInstallation.LegacyPackage == nil {
		t.Fatal("staging lost the full controller-side legacy package binding")
	}
}

func TestOnboardingUpgradeStagesBeforeDowntimeAndRetainsIdentity(t *testing.T) {
	for _, count := range []int{13, 15} {
		t.Run(strconvCount(count), func(t *testing.T) {
			plan, id := onboardingUpgradeFixture(t, count)
			fake := &onboardingUpgradeFake{t: t, plan: plan, oldRunning: true}
			before, _ := json.Marshal(plan.ExistingInstallation)
			phases := []string{}
			receipt, phase, err := advanceOnboardingUpgrade(context.Background(), fake, plan, id, func(next string, r onboardingInstallReceipt) error {
				fake.phase = next
				phases = append(phases, next)
				return nil
			})
			if err != nil || phase != "identity-verified" || !receipt.ServiceStarted || receipt.NodeID != plan.ExistingInstallation.NodeID {
				t.Fatalf("upgrade: %s %+v %v", phase, receipt, err)
			}
			want := []string{"stage", "inspect-old", "stop", "install", "start", "verify-identity"}
			if !reflect.DeepEqual(fake.calls, want) {
				t.Fatalf("effects=%v", fake.calls)
			}
			after, _ := json.Marshal(plan.ExistingInstallation)
			if !bytes.Equal(before, after) {
				t.Fatal("reviewed identity/model ownership was rewritten")
			}
			if len(phases) != 9 {
				t.Fatalf("missing durable phase: %v", phases)
			}
		})
	}
}
func strconvCount(n int) string {
	if n == 13 {
		return "old13"
	}
	return "old15"
}

func TestOnboardingUpgradeRefusesChangedOwnerBeforeStop(t *testing.T) {
	plan, id := onboardingUpgradeFixture(t, 13)
	fake := &onboardingUpgradeFake{t: t, plan: plan, oldRunning: true, changeOld: true}
	_, phase, err := advanceOnboardingUpgrade(context.Background(), fake, plan, id, func(next string, _ onboardingInstallReceipt) error { fake.phase = next; return nil })
	if err == nil || phase != "staged" || !fake.oldRunning || !reflect.DeepEqual(fake.calls, []string{"stage", "inspect-old"}) {
		t.Fatalf("changed owner reached downtime: %v %s %v", fake.calls, phase, err)
	}
	foreign := *plan.ExistingInstallation
	foreign.UnitBytes += "# foreign directive\n"
	sum := sha256.Sum256([]byte(foreign.UnitBytes))
	foreign.UnitSHA256 = hex.EncodeToString(sum[:])
	if validateOnboardingExisting(foreign, plan.Info) == nil {
		t.Fatal("foreign unit was accepted merely because its checksum was consistent")
	}
	foreign = *plan.ExistingInstallation
	foreign.LauncherPresent = true
	foreign.LauncherBody = onboardingUpgradeLauncher(foreign.Executable, false) + "echo unexpected\n"
	sum = sha256.Sum256([]byte(foreign.LauncherBody))
	foreign.LauncherSHA256 = hex.EncodeToString(sum[:])
	if validateOnboardingExisting(foreign, plan.Info) == nil {
		t.Fatal("unknown launcher was accepted")
	}
}

func TestOnboardingUpgradeReviewRejectsAliasesForOneEnrolledPeer(t *testing.T) {
	plan, _ := onboardingUpgradeFixture(t, 13)
	f := newOnboardingControllerFixture(t, 2)
	old := *plan.ExistingInstallation
	meshDir := t.TempDir()
	clustertrusttest.Join(t, meshDir, old.ClusterID, onboardingFixtureController, old.NodeID)
	f.s.m.mesh = clustertrust.Open(meshDir)
	pin, _ := f.s.m.mesh.PinSHA256(old.NodeID)
	old.CertFingerprint = "sha256:" + pin
	f.s.testCluster = func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		switch method {
		case "cluster:get-node-id":
			return onboardingMarshal(map[string]string{"nodeUuid": onboardingFixtureController, "clusterId": old.ClusterID}), nil
		case "nodes:get-initial":
			return onboardingMarshal(map[string]any{"nodes": []map[string]string{{"nodeUuid": old.NodeID, "state": "member"}}}), nil
		}
		t.Fatal("duplicate-peer review attempted a cluster mutation")
		return nil, errors.New("unexpected mutation")
	}
	for _, id := range f.ids {
		f.s.targets[id].candidate.HostKeyTrusted = true
	}
	f.s.dial = func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
		return &onboardingSSH{testRun: func(_ context.Context, command string, _ io.Reader) ([]byte, error) {
			if command == onboardingPython(onboardingInspectScript) {
				info := f.info
				info.ExistingPAIR = true
				return onboardingMarshal(info), nil
			}
			if command == onboardingPython(onboardingUpgradeInspectScript) {
				return onboardingMarshal(old), nil
			}
			t.Fatal("duplicate-peer review reached an effect")
			return nil, errors.New("unexpected effect")
		}}, nil
	}
	review, err := f.s.inspect(context.Background(), onboardingInspectRequest{CandidateIDs: f.ids, ArtifactID: f.pkg.source.ArtifactID})
	if err != nil || review.CanApprove || len(review.Targets) != 2 || review.Targets[0].Status != "blocked" || review.Targets[1].Status != "blocked" {
		t.Fatalf("aliases were not blocked in the real review path: %+v %v", review, err)
	}
	if _, err := f.s.approve(context.Background(), review.ReviewID); err == nil {
		t.Fatal("blocked aliases reached approval")
	}
}

func TestOnboardingEnrolledPeerUpgradeUsesActualReviewAndOperationPath(t *testing.T) {
	onboardingActualUpgrade(t, false)
}

func TestOnboardingUpgradeRollbackReconnectsAfterClosedSSH(t *testing.T) {
	onboardingActualUpgrade(t, true)
}

func onboardingActualUpgrade(t *testing.T, closeFirstClient bool) {
	plan, _ := onboardingUpgradeFixture(t, 13)
	f := newOnboardingControllerFixture(t, 1)
	old := *plan.ExistingInstallation
	meshDir := t.TempDir()
	clustertrusttest.Join(t, meshDir, old.ClusterID, onboardingFixtureController, old.NodeID)
	f.s.m.mesh = clustertrust.Open(meshDir)
	pin, _ := f.s.m.mesh.PinSHA256(old.NodeID)
	old.CertFingerprint = "sha256:" + pin
	id := f.ids[0]
	f.s.targets[id].candidate.HostKeyTrusted = true
	f.s.targets[id].expiresAt = time.Now().Add(30 * time.Minute)
	f.s.testCluster = func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		switch method {
		case "cluster:get-node-id":
			return onboardingMarshal(map[string]string{"nodeUuid": onboardingFixtureController, "clusterId": old.ClusterID}), nil
		case "nodes:get-initial":
			return onboardingMarshal(map[string]any{"nodes": []map[string]string{{"nodeUuid": old.NodeID, "state": "member"}}}), nil
		}
		t.Fatal("enrolled peer upgrade attempted re-pairing")
		return nil, errors.New("unexpected cluster mutation")
	}
	fake := &onboardingUpgradeFake{t: t, oldRunning: true}
	if closeFirstClient {
		fake.fail = "stop"
	}
	dials, closedCalls := 0, 0
	f.s.dial = func(_ context.Context, candidate onboardingCandidate, access onboardingAccess) (*onboardingSSH, error) {
		dials++
		if !candidate.HostKeyTrusted || candidate.HostKeySHA256 != f.s.targets[id].candidate.HostKeySHA256 || access.user != "approved-user" {
			t.Fatal("recovery changed the reviewed SSH host or account")
		}
		closed := false
		return &onboardingSSH{testRun: func(ctx context.Context, command string, input io.Reader) ([]byte, error) {
			if closed {
				closedCalls++
				return nil, errors.New("this SSH connection is permanently closed")
			}
			if command == onboardingPython(onboardingInspectScript) {
				info := f.info
				info.ExistingPAIR = true
				return onboardingMarshal(info), nil
			}
			if command == onboardingPython(onboardingUpgradeInspectScript) {
				return onboardingMarshal(old), nil
			}
			f.s.mu.Lock()
			for _, run := range f.s.operations {
				fake.plan = run.Plans[id]
				fake.phase = fake.plan.UpgradePhase
			}
			f.s.mu.Unlock()
			if command == onboardingPython(onboardingOwnedScript+onboardingControlScript) {
				var request struct {
					Method string `json:"method"`
				}
				raw, _ := io.ReadAll(input)
				_ = json.Unmarshal(raw, &request)
				if !fake.newRunning {
					t.Fatal("membership verification preceded successor start")
				}
				switch request.Method {
				case "cluster:get-node-id":
					return onboardingMarshal(map[string]any{"result": map[string]string{"nodeUuid": old.NodeID, "clusterId": old.ClusterID, "certFingerprint": old.CertFingerprint}}), nil
				case "nodes:get-initial":
					return onboardingMarshal(map[string]any{"result": map[string]any{"nodes": []map[string]string{{"nodeUuid": onboardingFixtureController, "state": "member"}}}}), nil
				}
				t.Fatal("upgrade dispatched a pairing mutation")
			}
			if command == onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeRetireScript) {
				if fake.phase != "retiring" || !fake.newRunning {
					t.Fatal("retirement preceded verified successor")
				}
				fake.calls = append(fake.calls, "retire-old-cli")
				return onboardingMarshal(map[string]any{"retired": true, "retiredEntries": 14, "launcher": "absent", "scope": "bound-cli-only"}), nil
			}
			if command == onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingRetentionScript) {
				fake.calls = append(fake.calls, "retain-bundles")
				return onboardingMarshal(map[string]any{"owner": onboardingRetentionOwner, "activeDigest": fake.plan.Receipt.ArtifactSHA256, "operation": path.Base(fake.plan.Receipt.StagePath), "generation": 1, "migrated": true}), nil
			}
			if command == onboardingPython(onboardingOwnedScript+onboardingFinishScript) {
				fake.calls = append(fake.calls, "finish")
				return []byte(`{"stagingCleaned":true}`), nil
			}
			body, err := fake.run(ctx, command, input)
			if closeFirstClient && errors.Is(err, context.DeadlineExceeded) {
				closed = true
			}
			return body, err
		}}, nil
	}
	review, err := f.s.inspect(context.Background(), onboardingInspectRequest{CandidateIDs: f.ids, ArtifactID: f.pkg.source.ArtifactID})
	if err != nil || !review.CanApprove || review.Targets[0].Action != "upgrade" || review.Targets[0].ExistingInstallation.NodeID != old.NodeID || review.Targets[0].Artifact.SourceFingerprint == "" {
		t.Fatalf("upgrade review: %+v %v", review, err)
	}
	op, err := f.s.approve(context.Background(), review.ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		value, err := f.s.operationRequest(context.Background(), "engine:onboarding-status", onboardingOperationRequest{OperationID: op.OperationID})
		if err != nil {
			t.Fatal(err)
		}
		current := value.(onboardingOperation)
		if current.State != "running" {
			wantState := "completed"
			if closeFirstClient {
				wantState = "failed"
			}
			if current.State != wantState || current.Targets[0].NodeID != old.NodeID || !closeFirstClient && current.Targets[0].CanCancel || !current.Targets[0].CleanupConfirmed {
				t.Fatalf("upgrade operation: %+v effects=%v", current, fake.calls)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture upgrade did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if closeFirstClient {
		if dials != 3 || closedCalls != 0 || !fake.oldRunning || fake.newRunning || fake.calls[len(fake.calls)-2] != "rollback" || fake.calls[len(fake.calls)-1] != "clean-stage" {
			t.Fatalf("rollback reused closed transport or changed ownership: dials=%d closedCalls=%d effects=%v", dials, closedCalls, fake.calls)
		}
		return
	}
	if !fake.newRunning || fake.oldRunning || fake.calls[len(fake.calls)-3] != "retire-old-cli" || fake.calls[len(fake.calls)-2] != "retain-bundles" || fake.calls[len(fake.calls)-1] != "finish" {
		t.Fatalf("replacement/retirement sequence=%v", fake.calls)
	}
}

func TestOnboardingRetirementBindsRenamedAndDeletedProcessAliases(t *testing.T) {
	plan, id := onboardingUpgradeFixture(t, 13)
	racingExecutable := path.Join(plan.ExistingInstallation.Bundle, "nvpair-tui") + ".pair-retired-" + id + " (deleted)"
	fake := &onboardingInstallFake{reply: func(_ int, call onboardingInstallCall) ([]byte, error) {
		var body struct {
			Existing             onboardingExistingInstallation `json:"existing"`
			RetiredProcessSuffix string                         `json:"retiredProcessSuffix"`
		}
		if json.Unmarshal(call.input, &body) != nil {
			t.Fatal("invalid retirement request")
		}
		for _, component := range body.Existing.Components {
			tracked := path.Join(body.Existing.Bundle, component.Name) + body.RetiredProcessSuffix
			if tracked == strings.TrimSuffix(racingExecutable, " (deleted)") {
				return nil, errors.New("still-running bound old executable")
			}
		}
		return onboardingMarshal(map[string]any{"retired": true, "retiredEntries": 14, "launcher": "absent", "scope": "bound-cli-only"}), nil
	}}
	if err := retireOnboardingUpgrade(context.Background(), fake, plan.Receipt, *plan.ExistingInstallation); err == nil {
		t.Fatal("a racing old launch disappeared when its inode acquired the retirement/deleted pathname")
	}
}

func TestOnboardingServiceWrappersCoverOwnedDrainWithoutExtendingStart(t *testing.T) {
	for _, action := range []string{"start", "stop", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			fake := &onboardingBudgetRunner{t: t, action: action}
			if err := runOnboardingService(context.Background(), fake, onboardingInstallReceipt{}, action, "session"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type onboardingBudgetRunner struct {
	t      *testing.T
	action string
}

func (f *onboardingBudgetRunner) run(ctx context.Context, _ string, _ io.Reader) ([]byte, error) {
	deadline, ok := ctx.Deadline()
	want := 35 * time.Second
	if f.action != "start" {
		want = 275 * time.Second
	}
	if !ok || time.Until(deadline) < want-time.Second || time.Until(deadline) > want {
		f.t.Fatal("service wrapper does not match the bounded lifecycle budget")
	}
	state := map[string]string{"start": "started", "stop": "stopped", "uninstall": "not-installed"}[f.action]
	return onboardingMarshal(map[string]any{"unit": onboardingUpgradeUnit, "operation": f.action, "state": state, "requestedLifetime": "session", "effectiveLifetime": "session"}), nil
}

func TestOnboardingUpgradeLostStopReplyResumesWithoutRetransfer(t *testing.T) {
	plan, id := onboardingUpgradeFixture(t, 13)
	fake := &onboardingUpgradeFake{t: t, plan: plan, oldRunning: true, fail: "stop"}
	save := func(next string, r onboardingInstallReceipt) error {
		fake.phase = next
		plan.UpgradePhase = next
		plan.Receipt = r
		return nil
	}
	_, phase, err := advanceOnboardingUpgrade(context.Background(), fake, plan, id, save)
	if err == nil || phase != "stopping" || fake.oldRunning {
		t.Fatal("lost stop reply was not retained as an uncertain phase")
	}
	_, phase, err = advanceOnboardingUpgrade(context.Background(), fake, plan, id, save)
	if err != nil || phase != "identity-verified" {
		t.Fatalf("retry: %s %v", phase, err)
	}
	if !reflect.DeepEqual(fake.calls, []string{"stage", "inspect-old", "stop", "stop", "install", "start", "verify-identity"}) {
		t.Fatalf("retry repeated unrelated effects: %v", fake.calls)
	}
}

func TestOnboardingUpgradeFailureRollsBackOnlyBoundOldOwner(t *testing.T) {
	plan, id := onboardingUpgradeFixture(t, 13)
	fake := &onboardingUpgradeFake{t: t, plan: plan, oldRunning: true, fail: "install"}
	save := func(next string, r onboardingInstallReceipt) error {
		fake.phase = next
		plan.UpgradePhase = next
		plan.Receipt = r
		return nil
	}
	receipt, phase, err := advanceOnboardingUpgrade(context.Background(), fake, plan, id, save)
	if err == nil || !upgradeNeedsRollback(phase) {
		t.Fatal("uncertain replacement lost rollback ownership")
	}
	receipt, err = rollbackOnboardingUpgrade(context.Background(), fake, *plan.ExistingInstallation, receipt, save)
	if err != nil || !receipt.CleanupConfirmed || !fake.oldRunning || fake.newUnit || fake.newRunning || plan.UpgradePhase != "rolled-back" {
		t.Fatalf("rollback: %+v %v", receipt, err)
	}
	if fake.calls[len(fake.calls)-2] != "rollback" || fake.calls[len(fake.calls)-1] != "clean-stage" {
		t.Fatal("rollback did not settle the original owner first")
	}
}

func TestOnboardingUpgradeCheckpointFailureAndOldIncomingBundleHaveNoDowntime(t *testing.T) {
	plan, id := onboardingUpgradeFixture(t, 13)
	fake := &onboardingUpgradeFake{t: t, plan: plan, oldRunning: true}
	_, _, err := advanceOnboardingUpgrade(context.Background(), fake, plan, id, func(next string, _ onboardingInstallReceipt) error {
		if next == "stopping" {
			return errors.New("journal unavailable")
		}
		fake.phase = next
		return nil
	})
	if err == nil || !fake.oldRunning || !reflect.DeepEqual(fake.calls, []string{"stage", "inspect-old"}) {
		t.Fatal("journal failure admitted stop")
	}
	entries := onboardingFixtureEntries(183)
	filtered := entries[:0]
	for _, entry := range entries {
		if !strings.HasSuffix(entry.name, "/nvpair-proxy") {
			filtered = append(filtered, entry)
		}
	}
	file, artifact := writeOnboardingFixture(t, filtered)
	oldPkg := onboardingPackage{source: onboardingArtifactSource{onboardingArtifact: artifact, File: file}, file: file, archiveRoot: "pair-test"}
	before := len(fake.calls)
	if _, err = stageOnboardingWithRunner(context.Background(), fake, plan.Info, oldPkg, id, "session", plan.ExistingInstallation, nil); err == nil || len(fake.calls) != before {
		t.Fatal("incomplete unified inventory weakened the strict successor catalog")
	}
	if validateFreshOnboardingPlatform(plan.Info) == nil {
		t.Fatal("upgrade enabled replacing identities through fresh enrollment")
	}
}

func TestOnboardingRetirementCommitSurvivesPartialFailureAndCancel(t *testing.T) {
	plan, id := onboardingUpgradeFixture(t, 13)
	candidate := plan.Candidate.CandidateID
	e := NewExecutor(nil, NewReporter(nil), nil, t.TempDir())
	m := &Manager{exec: e}
	s := &onboardingService{m: m, ctx: context.Background(), operations: map[string]*onboardingRun{}, targets: map[string]*onboardingPrivateTarget{}}
	run := &onboardingRun{Public: onboardingOperation{OperationID: id, Revision: 1, State: "running", Targets: []onboardingTargetState{{CandidateID: candidate, Stage: "verifying", CanCancel: true}}}, Plans: map[string]onboardingPlan{candidate: plan}, cancelTarget: map[string]bool{}, targetCancels: map[string]context.CancelFunc{}, Deadline: time.Now().Add(time.Minute).UnixMilli()}
	s.operations[id] = run
	s.active = id
	if err := s.upgradeCheckpoint(run, candidate, "retiring", plan.Receipt); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(t.TempDir(), "old-worker")
	if err := os.WriteFile(oldFile, []byte("bound old fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &onboardingInstallFake{reply: func(_ int, call onboardingInstallCall) ([]byte, error) {
		if !strings.Contains(call.command, "retiredEntries") {
			t.Fatal("unexpected lifecycle during retirement")
		}
		if err := os.Remove(oldFile); err != nil {
			t.Fatal(err)
		}
		return nil, io.ErrUnexpectedEOF
	}}
	if retireOnboardingUpgrade(context.Background(), fake, plan.Receipt, *plan.ExistingInstallation) == nil {
		t.Fatal("partial retirement became success")
	}
	if err := s.updateTarget(run, candidate, "verification-failed", "retirement interrupted", &plan.Receipt, plan.ExistingInstallation.NodeID); err != nil {
		t.Fatal(err)
	}
	if run.Public.Targets[0].CanCancel {
		t.Fatal("partial retirement advertised rollback/cancel")
	}
	for _, selected := range []string{candidate, ""} {
		if _, err := s.operationRequest(context.Background(), "engine:onboarding-cancel", onboardingOperationRequest{OperationID: id, CandidateID: selected}); err == nil {
			t.Fatal("cancel was admitted after retirement began")
		}
	}
	other := strings.Repeat("c", 32)
	fresh := plan
	fresh.Candidate.CandidateID, fresh.Review.CandidateID = other, other
	fresh.Review.Action, fresh.Review.ExistingInstallation = "", nil
	fresh.ExistingInstallation, fresh.UpgradePhase, fresh.Info.ExistingPAIR = nil, "", false
	run.Plans[other] = fresh
	run.Public.Targets = append(run.Public.Targets, onboardingTargetState{CandidateID: other, Stage: "access-authorized", CanCancel: true})
	if _, err := s.operationRequest(context.Background(), "engine:onboarding-cancel", onboardingOperationRequest{OperationID: id}); err != nil || !run.cancelTarget[other] || run.cancelTarget[candidate] {
		t.Fatal("batch cancellation touched committed retirement or failed to cancel the other target")
	}
	restarted := &onboardingService{m: m, ctx: context.Background(), operations: map[string]*onboardingRun{}, targets: map[string]*onboardingPrivateTarget{}}
	restarted.loadOperations()
	recovered := restarted.operations[id]
	if recovered == nil || restarted.recoveryRequired || recovered.Plans[candidate].UpgradePhase != "retiring" || recovered.Public.Targets[0].CanCancel || !recovered.Public.Targets[0].CanRetry {
		t.Fatal("restart lost committed retirement disposition")
	}
	if len(fake.calls) != 1 {
		t.Fatal("cancel/restart dispatched cleanup, uninstall or rollback")
	}
}
