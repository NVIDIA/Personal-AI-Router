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
	"os/exec"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func managedOnboardingPredecessor(t *testing.T) (onboardingPlan, string) {
	t.Helper()
	plan, operation := onboardingUpgradeFixture(t, 15)
	digest := strings.Repeat("9", 64)
	old := plan.ExistingInstallation
	old.Bundle = path.Join(old.Home, ".local", "share", "Nvidia Corporation", "Personal AI Router", "bundles", digest, "bin")
	old.Executable, old.Broker = path.Join(old.Bundle, "nvpair-tui"), path.Join(old.Bundle, "nvpair-ui-broker")
	old.UnitBytes = onboardingUpgradeUnitText(old.Executable, old.Broker, old.ConfigHome, 145)
	sum := sha256.Sum256([]byte(old.UnitBytes))
	old.UnitSHA256 = hex.EncodeToString(sum[:])
	old.Retention = &onboardingRetentionReview{
		Owner:                "absent",
		ActiveDigest:         digest,
		HeldPruneDigests:     []string{},
		Generation:           0,
		NextHeldPruneDigests: []string{},
		NextGeneration:       1,
	}
	plan.Review.ExistingInstallation = func() *onboardingInstallationSummary { value := old.Summary(); return &value }()
	return plan, operation
}

func TestOnboardingApproveBindsManagedRetentionSummaryByValue(t *testing.T) {
	fixture := func(t *testing.T) (*onboardingService, string) {
		t.Helper()
		plan, _ := managedOnboardingPredecessor(t)
		id := plan.Candidate.CandidateID
		plan.Candidate.AccessID = "fixture-access"
		plan.Candidate.AccessLabel = "approved-user (password)"
		plan.Candidate.AccessAvailable = true
		plan.Candidate.HostKeySHA256 = "SHA256:fixture-managed-peer"

		row := plan.Review
		row.Label = "fixture managed peer"
		row.Address, row.Port = plan.Candidate.Address, plan.Candidate.Port
		row.AccessID, row.AccessLabel = plan.Candidate.AccessID, plan.Candidate.AccessLabel
		row.HostKeySHA256 = plan.Candidate.HostKeySHA256
		summary := plan.ExistingInstallation.Summary()
		row.ExistingInstallation = &summary
		fresh := plan.ExistingInstallation.Summary()
		if summary.Retention == nil || summary.Retention == fresh.Retention || !reflect.DeepEqual(summary, fresh) {
			t.Fatal("fixture must carry value-equal summaries with distinct retention pointers")
		}

		reviewID := newOpID()
		s := NewManager(nil, newTestExecutor(t, testEngineManifest(fakeEngineBin)), nil).onboarding
		s.testSave = func(*onboardingRun) error { return nil }
		s.testCluster = func(_ context.Context, method string, _ any) (json.RawMessage, error) {
			if method != "cluster:get-node-id" {
				return nil, errors.New("unexpected fixture cluster request")
			}
			return onboardingMarshal(onboardingNodeIdentity{NodeUUID: onboardingFixtureController, ClusterID: plan.ExistingInstallation.ClusterID}), nil
		}
		s.dial = func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
			return nil, errors.New("fixture stops after approval")
		}
		s.targets[id] = &onboardingPrivateTarget{
			accessGeneration: "fixture-generation",
			candidate:        plan.Candidate,
			access:           onboardingAccess{user: plan.Username, password: "fixture-password"},
			lifetime:         row.StartupLifetime,
		}
		s.reviews[reviewID] = onboardingReviewBinding{
			review: onboardingReview{
				ReviewID: reviewID, ControllerNodeID: onboardingFixtureController,
				TargetClusterID: plan.ExistingInstallation.ClusterID,
				ExpiresAt:       time.Now().Add(time.Minute).UnixMilli(),
				CanApprove:      true, Targets: []onboardingReviewTarget{row},
			},
			info: map[string]onboardingPlatformInfo{id: plan.Info},
			packages: map[string]onboardingPackage{id: {
				source: plan.Artifact, file: plan.PackageFile,
				archiveRoot: plan.ArchiveRoot, bytes: plan.ArchiveBytes,
			}},
			upgrades: map[string]onboardingExistingInstallation{id: *plan.ExistingInstallation},
		}
		return s, reviewID
	}

	t.Run("equal nested retention", func(t *testing.T) {
		s, reviewID := fixture(t)
		op, err := s.approve(context.Background(), reviewID)
		if err != nil {
			t.Fatal(err)
		}
		terminal := waitOnboardingTerminal(t, s, op.OperationID)
		if terminal.State != "failed" || terminal.Targets[0].Stage != "verification-failed" {
			t.Fatalf("approved fixture did not stop at its fake dial: %+v", terminal)
		}
	})

	t.Run("changed retention", func(t *testing.T) {
		s, reviewID := fixture(t)
		binding := s.reviews[reviewID]
		binding.review.Targets[0].ExistingInstallation.Retention.NextGeneration++
		s.reviews[reviewID] = binding
		if _, err := s.approve(context.Background(), reviewID); err == nil || !strings.Contains(err.Error(), "summary changed") {
			t.Fatalf("changed retention was not rejected: %v", err)
		}
		if s.active != "" || len(s.operations) != 0 {
			t.Fatal("changed retention published an onboarding operation")
		}
	})
}

func TestOnboardingManagedBundleDigestIsExact(t *testing.T) {
	plan, _ := managedOnboardingPredecessor(t)
	digest, ok := onboardingManagedBundleDigest(*plan.ExistingInstallation)
	if !ok || digest != strings.Repeat("9", 64) {
		t.Fatalf("managed predecessor was not recognized: %q %v", digest, ok)
	}
	for _, mutate := range []func(*onboardingExistingInstallation){
		func(old *onboardingExistingInstallation) {
			old.Bundle = path.Join(path.Dir(path.Dir(old.Bundle)), "not-a-digest", "bin")
		},
		func(old *onboardingExistingInstallation) {
			old.Bundle = path.Join(path.Dir(old.Bundle), "nested", "bin")
		},
		func(old *onboardingExistingInstallation) {
			old.Bundle = strings.Replace(old.Bundle, "/bundles/", "/foreign/", 1)
		},
	} {
		near := *plan.ExistingInstallation
		mutate(&near)
		if _, ok := onboardingManagedBundleDigest(near); ok {
			t.Fatalf("near-miss bundle was classified as managed: %s", near.Bundle)
		}
	}
}

func TestOnboardingUpgradeTransferDescriptorRetainsReviewedRetention(t *testing.T) {
	plan, _ := managedOnboardingPredecessor(t)
	existing := *plan.ExistingInstallation
	remote := onboardingUpgradeTransferDescriptor(existing)
	if !reflect.DeepEqual(remote.Retention, existing.Retention) {
		t.Fatal("reviewed managed retention was omitted from the receiver comparison")
	}
	if remote.LegacyPackageStatus != "" || remote.LegacyPackage != nil {
		t.Fatal("controller-only legacy package evidence crossed the receiver boundary")
	}
	remote.LegacyPackageStatus = existing.LegacyPackageStatus
	remote.LegacyPackage = existing.LegacyPackage
	if !reflect.DeepEqual(remote, existing) {
		t.Fatal("receiver comparison descriptor lost ordinary installation identity")
	}
}

func TestOnboardingManagedRetentionStagingReinspectsFullDescriptor(t *testing.T) {
	plan, operation := managedOnboardingPredecessor(t)
	check := "if inspect_reviewed_upgrade()!=h['upgrade']:"
	effect := "os.mkdir(stage,0o700)"
	if checkAt, effectAt := strings.Index(onboardingReceiveScript, check), strings.Index(onboardingReceiveScript, effect); checkAt < 0 || effectAt < 0 || checkAt >= effectAt {
		t.Fatal("full managed retention is not revalidated before the first staging effect")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("Python is required for the embedded receiver comparison regression")
	}
	observed := onboardingUpgradeTransferDescriptor(*plan.ExistingInstallation)
	for _, tc := range []struct {
		name   string
		change func(*onboardingExistingInstallation)
		equal  bool
	}{
		{name: "value-equal", change: func(*onboardingExistingInstallation) {}, equal: true},
		{name: "changed-generation", change: func(value *onboardingExistingInstallation) {
			value.Retention.NextGeneration++
		}},
		{name: "changed-held-digests", change: func(value *onboardingExistingInstallation) {
			value.Retention.NextHeldPruneDigests = append(
				value.Retention.NextHeldPruneDigests,
				strings.Repeat("8", 64),
			)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observedJSON, _ := json.Marshal(observed)
			var reviewed onboardingExistingInstallation
			if err := json.Unmarshal(observedJSON, &reviewed); err != nil {
				t.Fatal(err)
			}
			tc.change(&reviewed)
			reviewedJSON, _ := json.Marshal(reviewed)
			harness := "import json\n" +
				"observed=json.loads(" + strconv.Quote(string(observedJSON)) + ")\n" +
				"reviewed=json.loads(" + strconv.Quote(string(reviewedJSON)) + ")\n" +
				"def inspect_existing():\n value=dict(observed); value.pop('retention',None); return value\n" +
				"def inspect_retention(_value): return observed['retention']\n" +
				onboardingReceiverUpgradeInspectScript +
				"\nassert (inspect_reviewed_upgrade()==reviewed) is " +
				map[bool]string{true: "True", false: "False"}[tc.equal] + "\n"
			if output, err := exec.Command(python, "-I", "-B", "-c", harness).CombinedOutput(); err != nil {
				t.Fatalf("receiver retention comparison: %s %v", output, err)
			}
		})
	}
	fake := &onboardingUpgradeFake{t: t, plan: plan, phase: "staging", oldRunning: true}
	before, _ := json.Marshal(plan.ExistingInstallation)
	pkg := onboardingPackage{
		source: plan.Artifact, file: plan.PackageFile,
		archiveRoot: plan.ArchiveRoot, bytes: plan.ArchiveBytes,
	}
	if _, err := stageOnboardingWithRunner(
		context.Background(),
		fake,
		plan.Info,
		pkg,
		operation,
		plan.Review.StartupLifetime,
		plan.ExistingInstallation,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(plan.ExistingInstallation)
	if !fake.staged || !bytes.Equal(before, after) {
		t.Fatal("managed-retention staging lost or changed its reviewed predecessor")
	}
}

func TestOnboardingManagedRetirementRetainsRollbackBundle(t *testing.T) {
	plan, _ := managedOnboardingPredecessor(t)
	digest, _ := onboardingManagedBundleDigest(*plan.ExistingInstallation)
	fake := &onboardingInstallFake{reply: func(_ int, _ onboardingInstallCall) ([]byte, error) {
		return onboardingMarshal(map[string]any{"retired": true, "retiredEntries": 0, "retainedRollback": digest, "launcher": "absent", "scope": "managed-bundle-retained"}), nil
	}}
	if err := retireOnboardingUpgrade(context.Background(), fake, plan.Receipt, *plan.ExistingInstallation); err != nil {
		t.Fatal(err)
	}
	fake.reply = func(_ int, _ onboardingInstallCall) ([]byte, error) {
		return onboardingMarshal(map[string]any{"retired": true, "retiredEntries": 16, "launcher": "absent", "scope": "bound-cli-only"}), nil
	}
	if err := retireOnboardingUpgrade(context.Background(), fake, plan.Receipt, *plan.ExistingInstallation); err == nil {
		t.Fatal("managed predecessor was accepted as deleted legacy payload")
	}
}

func TestOnboardingRetentionAcknowledgesMigrationAndOrderedHeldHistory(t *testing.T) {
	plan, operation := managedOnboardingPredecessor(t)
	rollback, _ := onboardingManagedBundleDigest(*plan.ExistingInstallation)
	command := onboardingPython(onboardingUpgradeInspectPrelude + onboardingOwnedScript + onboardingUpgradeIdentityPrelude + onboardingRetentionScript)
	for _, tc := range []struct {
		name      string
		retention onboardingRetentionReview
	}{
		{name: "unpublished-migration", retention: *plan.ExistingInstallation.Retention},
		{name: "v1-held-migration", retention: onboardingRetentionReview{
			Owner: onboardingLegacyRetentionOwner, ActiveDigest: rollback, RollbackDigest: strings.Repeat("8", 64), HeldPruneDigests: []string{strings.Repeat("7", 64)}, Operation: strings.Repeat("6", 32), Generation: 2, RegistrySHA256: strings.Repeat("5", 64), NextHeldPruneDigests: []string{strings.Repeat("7", 64), strings.Repeat("8", 64)}, NextGeneration: 3,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := plan
			old := *plan.ExistingInstallation
			old.Retention = &tc.retention
			current.ExistingInstallation = &old
			summary := old.Summary()
			current.Review.ExistingInstallation = &summary
			fake := &onboardingInstallFake{reply: func(_ int, call onboardingInstallCall) ([]byte, error) {
				if call.command != command {
					t.Fatal("retention used an unreviewed remote program")
				}
				var request struct {
					Receipt  onboardingInstallReceipt       `json:"receipt"`
					Existing onboardingExistingInstallation `json:"existing"`
				}
				if json.Unmarshal(call.input, &request) != nil || request.Receipt != current.Receipt || !reflect.DeepEqual(request.Existing, old) {
					t.Fatal("retention lost the committed receipt or predecessor binding")
				}
				return onboardingMarshal(map[string]any{"owner": onboardingRetentionOwner, "activeDigest": current.Receipt.ArtifactSHA256, "rollbackDigest": rollback, "operation": operation, "generation": tc.retention.NextGeneration, "heldPruneDigests": tc.retention.NextHeldPruneDigests, "migrated": true}), nil
			}}
			if err := recordOnboardingBundleRetention(context.Background(), fake, current.Receipt, old); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOnboardingAbsentRetentionReviewKeepsExactPublicationKeys(t *testing.T) {
	plan, _ := managedOnboardingPredecessor(t)
	raw := onboardingMarshal(*plan.ExistingInstallation)
	var decoded map[string]json.RawMessage
	if json.Unmarshal(raw, &decoded) != nil {
		t.Fatal("managed predecessor did not marshal")
	}
	var retention map[string]json.RawMessage
	if json.Unmarshal(decoded["retention"], &retention) != nil {
		t.Fatal("managed predecessor lost its retention review")
	}
	want := []string{"activeDigest", "generation", "heldPruneDigests", "nextGeneration", "nextHeldPruneDigests", "operation", "owner", "registrySha256", "rollbackDigest"}
	got := make([]string, 0, len(retention))
	for key := range retention {
		got = append(got, key)
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, want) || string(retention["rollbackDigest"]) != `""` || string(retention["operation"]) != `""` || string(retention["registrySha256"]) != `""` {
		t.Fatalf("absent registry review does not match the fixed publication schema: keys=%v body=%s", got, raw)
	}
}

func TestOnboardingRetentionFailsClosedOnNearMisses(t *testing.T) {
	plan, operation := managedOnboardingPredecessor(t)
	rollback, _ := onboardingManagedBundleDigest(*plan.ExistingInstallation)
	valid := onboardingRetentionRegistry{Owner: onboardingRetentionOwner, ActiveDigest: plan.Receipt.ArtifactSHA256, RollbackDigest: rollback, HeldPruneDigests: []string{}, Operation: operation, Generation: 1}
	for _, tc := range []struct {
		name   string
		mutate func(*onboardingRetentionRegistry)
	}{
		{"foreign-owner", func(v *onboardingRetentionRegistry) { v.Owner = "foreign" }},
		{"wrong-active", func(v *onboardingRetentionRegistry) { v.ActiveDigest = strings.Repeat("7", 64) }},
		{"wrong-rollback", func(v *onboardingRetentionRegistry) { v.RollbackDigest = strings.Repeat("6", 64) }},
		{"wrong-operation", func(v *onboardingRetentionRegistry) { v.Operation = strings.Repeat("5", 32) }},
		{"zero-generation", func(v *onboardingRetentionRegistry) { v.Generation = 0 }},
		{"duplicate-held", func(v *onboardingRetentionRegistry) {
			v.HeldPruneDigests = []string{strings.Repeat("4", 64), strings.Repeat("4", 64)}
		}},
		{"active-held", func(v *onboardingRetentionRegistry) { v.HeldPruneDigests = []string{v.ActiveDigest} }},
		{"overflow-held", func(v *onboardingRetentionRegistry) {
			v.HeldPruneDigests = []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := valid
			tc.mutate(&observed)
			fake := &onboardingInstallFake{reply: func(_ int, _ onboardingInstallCall) ([]byte, error) { return onboardingMarshal(observed), nil }}
			if err := recordOnboardingBundleRetention(context.Background(), fake, plan.Receipt, *plan.ExistingInstallation); err == nil {
				t.Fatal("near-miss retention acknowledgement was accepted")
			}
		})
	}
	fake := &onboardingInstallFake{reply: func(_ int, _ onboardingInstallCall) ([]byte, error) { return nil, errors.New("closed transport") }}
	if err := recordOnboardingBundleRetention(context.Background(), fake, plan.Receipt, *plan.ExistingInstallation); err == nil {
		t.Fatal("lost atomic publication reply became success")
	}
}

func TestOnboardingRetentionScriptPublishesOnlyAndNeverDeletesBundles(t *testing.T) {
	for _, required := range []string{"renameat2", "heldPruneDigests", "RETENTION_LIMIT=4", "retention_residue_action(old_raw,pending_raw,encoded,reviewed)", "retention_review_value(displaced,upgrade_hash(pending_raw))", "observed!=reviewed", "next_held=next_held[-RETENTION_LIMIT:]", "set(os.listdir(folder))!=allowed", "successor process does not execute the active bundle"} {
		if !strings.Contains(onboardingRetentionScript, required) {
			t.Fatalf("retention script lost guard %q", required)
		}
	}
	if strings.Contains(onboardingRetentionScript, "rmtree") || strings.Contains(onboardingRetentionScript, "unlink(folder") {
		t.Fatal("held retention slice contains live bundle deletion")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err == nil {
		command := exec.Command(python, "-I", "-B", "-c", "import sys; compile(sys.stdin.read(), '<retention-publication>', 'exec')")
		command.Stdin = strings.NewReader(onboardingUpgradeInspectPrelude + onboardingOwnedScript + onboardingUpgradeIdentityPrelude + onboardingRetentionScript)
		if output, compileErr := command.CombinedOutput(); compileErr != nil {
			t.Fatalf("retention publication does not compile: %v: %s", compileErr, output)
		}
	}
}

func TestOnboardingRetentionEmbeddedModelMigratesOrdersAndRejectsTamper(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("Python interpreter unavailable for embedded retention model")
	}
	harness := "import hashlib,json,re,tempfile,pathlib\ndef upgrade_hash(data): return hashlib.sha256(data).hexdigest()\n" + onboardingRetentionInspectScript + `
def outcome(value):
 try: return retention_decode(json.dumps(value,sort_keys=True,separators=(',',':')).encode())
 except RuntimeError: return None
active='5'*64; rollback='7'*64; pending='d'*64; op='3'*32
legacy={'owner':RETENTION_V1,'activeDigest':active,'rollbackDigest':rollback,'pendingPruneDigest':pending,'pruneState':'held','operation':op,'generation':2}
raw=json.dumps(legacy,sort_keys=True,separators=(',',':')).encode(); decoded=retention_decode(raw); reviewed=retention_review_value(decoded,hashlib.sha256(raw).hexdigest())
full=['1'*64,'2'*64,'3'*64,'4'*64]; full_current={'owner':RETENTION_V2,'activeDigest':active,'rollbackDigest':rollback,'heldPruneDigests':full,'operation':op,'generation':5}; full_review=retention_review_value(full_current,'8'*64)
tampered=dict(legacy); tampered['operation']='4'*32; tampered_raw=json.dumps(tampered,sort_keys=True,separators=(',',':')).encode()
desired={'owner':RETENTION_V2,'activeDigest':'a'*64,'rollbackDigest':active,'heldPruneDigests':[pending,rollback],'operation':'b'*32,'generation':3}
encoded=json.dumps(desired,sort_keys=True,separators=(',',':')).encode()
unmanaged={'owner':RETENTION_V2,'activeDigest':'a'*64,'rollbackDigest':'','heldPruneDigests':[],'operation':'b'*32,'generation':1}
unmanaged_encoded=json.dumps(unmanaged,sort_keys=True,separators=(',',':')).encode()
def rejected(call):
 try: call(); return False
 except RuntimeError: return True
root=pathlib.Path(tempfile.mkdtemp()); bundle_files=[]
for name,body in ((active,b'active-bytes'),(rollback,b'rollback-bytes'),(pending,b'held-bytes')):
 folder=root/name; folder.mkdir(); item=folder/'payload'; item.write_bytes(body); bundle_files.append((item,hashlib.sha256(body).hexdigest()))
beforeAction=retention_residue_action(raw,encoded,encoded,reviewed)
afterAction=retention_residue_action(encoded,raw,encoded,reviewed)
answers={
 'migratedHeld':decoded['heldPruneDigests'],
 'nextHeld':reviewed['nextHeldPruneDigests'],
 'capacityRotated':full_review['nextHeldPruneDigests']==full[1:]+[rollback] and full_review['nextGeneration']==6,
 'exactCrashBinding':retention_review_value(retention_decode(raw),hashlib.sha256(raw).hexdigest())==reviewed,
 'tamperedCrashRejected':retention_review_value(retention_decode(tampered_raw),hashlib.sha256(tampered_raw).hexdigest())!=reviewed,
 'beforeExchange':beforeAction=='exchange',
 'afterExchange':afterAction=='cleanup-displaced',
 'wrongDisplacedRejected':rejected(lambda: retention_residue_action(encoded,tampered_raw,encoded,reviewed)),
 'bundleBytesUnchanged':all(hashlib.sha256(item.read_bytes()).hexdigest()==digest for item,digest in bundle_files),
 'unmanagedFirstPublicationExact':retention_decode(unmanaged_encoded)==unmanaged,
 'duplicateRejected':outcome({'owner':RETENTION_V2,'activeDigest':active,'rollbackDigest':'','heldPruneDigests':[pending,pending],'operation':op,'generation':2}) is None,
 'activeOverlapRejected':outcome({'owner':RETENTION_V2,'activeDigest':active,'rollbackDigest':'','heldPruneDigests':[active],'operation':op,'generation':2}) is None,
 'successorOverlapRejected':outcome({'owner':RETENTION_V2,'activeDigest':pending,'rollbackDigest':active,'heldPruneDigests':[pending,rollback],'operation':op,'generation':3}) is None,
 'overflowRejected':outcome({'owner':RETENTION_V2,'activeDigest':active,'rollbackDigest':'','heldPruneDigests':['1'*64,'2'*64,'3'*64,'4'*64,'6'*64],'operation':op,'generation':2}) is None}
print(json.dumps(answers,sort_keys=True))
`
	command := exec.Command(python, "-I", "-B", "-c", "import sys; exec(sys.stdin.read())")
	command.Stdin = strings.NewReader(harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("embedded retention model failed: %v: %s", err, output)
	}
	var got struct {
		MigratedHeld                   []string `json:"migratedHeld"`
		NextHeld                       []string `json:"nextHeld"`
		CapacityRotated                bool     `json:"capacityRotated"`
		ExactCrashBinding              bool     `json:"exactCrashBinding"`
		TamperedCrashRejected          bool     `json:"tamperedCrashRejected"`
		BeforeExchange                 bool     `json:"beforeExchange"`
		AfterExchange                  bool     `json:"afterExchange"`
		WrongDisplacedRejected         bool     `json:"wrongDisplacedRejected"`
		BundleBytesUnchanged           bool     `json:"bundleBytesUnchanged"`
		UnmanagedFirstPublicationExact bool     `json:"unmanagedFirstPublicationExact"`
		DuplicateRejected              bool     `json:"duplicateRejected"`
		ActiveOverlapRejected          bool     `json:"activeOverlapRejected"`
		SuccessorOverlapRejected       bool     `json:"successorOverlapRejected"`
		OverflowRejected               bool     `json:"overflowRejected"`
	}
	if json.Unmarshal(output, &got) != nil || !reflect.DeepEqual(got.MigratedHeld, []string{strings.Repeat("d", 64)}) || !reflect.DeepEqual(got.NextHeld, []string{strings.Repeat("d", 64), strings.Repeat("7", 64)}) || !got.CapacityRotated || !got.ExactCrashBinding || !got.TamperedCrashRejected || !got.BeforeExchange || !got.AfterExchange || !got.WrongDisplacedRejected || !got.BundleBytesUnchanged || !got.UnmanagedFirstPublicationExact || !got.DuplicateRejected || !got.ActiveOverlapRejected || !got.SuccessorOverlapRejected || !got.OverflowRejected {
		t.Fatalf("embedded retention transition accepted a near miss: %s", output)
	}
}

func TestOnboardingRetentionReviewRollsGenerationFiveAcrossTwoAndThreeNodeUpdates(t *testing.T) {
	held := []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)}
	active, rollback := strings.Repeat("9", 64), strings.Repeat("8", 64)
	firstNext := []string{held[1], held[2], held[3], rollback}
	secondActive := strings.Repeat("a", 64)
	secondNext := []string{held[2], held[3], rollback, active}
	for _, participantCount := range []int{2, 3} {
		t.Run(strconv.Itoa(participantCount), func(t *testing.T) {
			for participant := 0; participant < participantCount; participant++ {
				plan, _ := managedOnboardingPredecessor(t)
				old := *plan.ExistingInstallation
				old.Retention = &onboardingRetentionReview{
					Owner: onboardingRetentionOwner, ActiveDigest: active, RollbackDigest: rollback,
					HeldPruneDigests: append([]string(nil), held...), Operation: strings.Repeat("6", 32),
					Generation: 5, RegistrySHA256: strings.Repeat("5", 64),
					NextHeldPruneDigests: append([]string(nil), firstNext...), NextGeneration: 6,
				}
				if err := validateOnboardingExisting(old, plan.Info); err != nil {
					t.Fatalf("participant %d generation-five review blocked: %v", participant, err)
				}
				if got := nextOnboardingHeldDigests(active, rollback, held); !reflect.DeepEqual(got, firstNext) {
					t.Fatalf("participant %d first rotation=%v", participant, got)
				}
				wrongOrder := old
				wrongRetention := *old.Retention
				wrongRetention.NextHeldPruneDigests = append([]string(nil), held...)
				wrongOrder.Retention = &wrongRetention
				if validateOnboardingExisting(wrongOrder, plan.Info) == nil {
					t.Fatal("review accepted a transition that discarded the current rollback")
				}
				overflow := old
				overflowRetention := *old.Retention
				overflowRetention.HeldPruneDigests = append(append([]string(nil), held...), strings.Repeat("b", 64))
				overflow.Retention = &overflowRetention
				if validateOnboardingExisting(overflow, plan.Info) == nil {
					t.Fatal("over-limit retained registry became a rolling-window input")
				}

				old.Bundle = path.Join(old.Home, ".local", "share", "Nvidia Corporation", "Personal AI Router", "bundles", secondActive, "bin")
				old.Executable, old.Broker = path.Join(old.Bundle, "nvpair-tui"), path.Join(old.Bundle, "nvpair-ui-broker")
				old.UnitBytes = onboardingUpgradeUnitText(old.Executable, old.Broker, old.ConfigHome, 145)
				sum := sha256.Sum256([]byte(old.UnitBytes))
				old.UnitSHA256 = hex.EncodeToString(sum[:])
				old.Retention = &onboardingRetentionReview{
					Owner: onboardingRetentionOwner, ActiveDigest: secondActive, RollbackDigest: active,
					HeldPruneDigests: append([]string(nil), firstNext...), Operation: strings.Repeat("7", 32),
					Generation: 6, RegistrySHA256: strings.Repeat("6", 64),
					NextHeldPruneDigests: append([]string(nil), secondNext...), NextGeneration: 7,
				}
				if err := validateOnboardingExisting(old, plan.Info); err != nil {
					t.Fatalf("participant %d repeated review blocked: %v", participant, err)
				}
				if got := nextOnboardingHeldDigests(secondActive, active, firstNext); !reflect.DeepEqual(got, secondNext) {
					t.Fatalf("participant %d repeated rotation=%v", participant, got)
				}
			}
		})
	}
}
