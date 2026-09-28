// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"
)

func onboardingLegacyPackageFixture() (onboardingLegacyPackageReceipt, onboardingPlatformInfo) {
	info := onboardingPlatformInfo{UID: 1000, Home: "/home/operator", OS: "Linux", Arch: "aarch64", ExistingPAIR: true, UserRuntime: true}
	file := func(name string, uid int, mode uint32, size int64) onboardingLegacyFileIdentity {
		return onboardingLegacyFileIdentity{Path: name, SHA256: strings.Repeat("a", 64), Bytes: size, Device: 8, Inode: uint64(len(name) + 100), UID: uid, GID: uid, Mode: mode, Links: 1}
	}
	return onboardingLegacyPackageReceipt{
		Package: onboardingLegacyPackageName, Status: "install ok installed", Version: "0.2.24~private.1", Architecture: "arm64",
		PackageRecordSHA256: strings.Repeat("b", 64), PackageFilesSHA256: strings.Repeat("c", 64), PackageInfoSHA256: strings.Repeat("d", 64),
		App: file(onboardingLegacyPackageApp, 0, 0755, 1024), Launcher: file(onboardingLegacyPackageLauncher, 0, 0755, 128), DesktopEntry: file(onboardingLegacyPackageDesktopEntry, 0, 0644, 256), Processes: []onboardingLegacyPackageProcess{}, Rollback: file(path.Join(info.Home, "Downloads", "nvpair_0.2.24_arm64.deb"), info.UID, 0644, 4096),
	}, info
}

func TestOnboardingLegacyPackageReceiptValidation(t *testing.T) {
	receipt, info := onboardingLegacyPackageFixture()
	if err := validateOnboardingLegacyPackage(onboardingLegacyPackageAdmitted, &receipt, info); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*onboardingLegacyPackageReceipt){
		"symlink-shaped inode": func(value *onboardingLegacyPackageReceipt) { value.App.Inode = 0 },
		"foreign app":          func(value *onboardingLegacyPackageReceipt) { value.App.UID = 2000 },
		"shared launcher":      func(value *onboardingLegacyPackageReceipt) { value.Launcher.Links = 2 },
		"writable desktop":     func(value *onboardingLegacyPackageReceipt) { value.DesktopEntry.Mode = 0664 },
		"changed rollback":     func(value *onboardingLegacyPackageReceipt) { value.Rollback.SHA256 = "changed" },
		"missing rollback":     func(value *onboardingLegacyPackageReceipt) { value.Rollback.Bytes = 0 },
		"running process": func(value *onboardingLegacyPackageReceipt) {
			value.Processes = []onboardingLegacyPackageProcess{{PID: 42}}
		},
		"foreign rollback":   func(value *onboardingLegacyPackageReceipt) { value.Rollback.UID = 2000 },
		"unbounded rollback": func(value *onboardingLegacyPackageReceipt) { value.Rollback.Path = "/tmp/nvpair.deb" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			mutate(&changed)
			if validateOnboardingLegacyPackage(onboardingLegacyPackageAdmitted, &changed, info) == nil {
				t.Fatal("unsafe legacy package receipt was admitted")
			}
		})
	}
	for _, status := range []string{"", onboardingLegacyPackageAbsent, onboardingLegacyPackageNotApplicable, onboardingLegacyPackageNoRollback, onboardingLegacyPackageNoElevation} {
		if err := validateOnboardingLegacyPackage(status, nil, info); err != nil {
			t.Fatalf("non-effect disposition %q was rejected: %v", status, err)
		}
	}
	if validateOnboardingLegacyPackage(onboardingLegacyPackageAdmitted, nil, info) == nil || validateOnboardingLegacyPackage(onboardingLegacyPackageAbsent, &receipt, info) == nil {
		t.Fatal("legacy package admission disagreed with its effect receipt")
	}
}

func TestOnboardingLegacyPackageRecordParsingFailsClosed(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("Python interpreter unavailable for embedded parser regression")
	}
	harness := "import json,re\n" + onboardingLegacyPackageRecordParserScript + `
def state(code,record,version=None,architecture=None):
 try: return legacy_package_record_state(code,record,version,architecture)[0]
 except RuntimeError: return 'error'
version='0.2.24~private.1'; installed=b'nvpair\tinstall ok installed\t0.2.24~private.1\tarm64\n'; config=b'nvpair\tdeinstall ok config-files\t0.2.24~private.1\tarm64\n'
answers={
 'missing':state(1,b'',version,'arm64'),
 'config_files':state(0,config,version,'arm64'),
 'installed':state(0,installed,version,'arm64'),
 'exit_two_empty':state(2,b'',version,'arm64'),
 'zero_malformed':state(0,b'nvpair\tinstall ok installed\n',version,'arm64'),
 'foreign_version':state(0,b'nvpair\tinstall ok installed\t9.9.9\tarm64\n',version,'arm64'),
 'foreign_architecture':state(0,b'nvpair\tinstall ok installed\t0.2.24~private.1\tamd64\n',version,'arm64'),
 'foreign_package':state(0,b'nvpair:foreign\tinstall ok installed\t0.2.24~private.1\tarm64\n',version,'arm64'),
 'foreign_status':state(0,b'nvpair\tunpack ok unpacked\t0.2.24~private.1\tarm64\n',version,'arm64')}
print(json.dumps(answers,sort_keys=True))
`
	command := exec.Command(python, "-c", "import sys; exec(sys.stdin.read())")
	command.Stdin = strings.NewReader(harness)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("embedded dpkg record parser failed: %v: %s", err, output)
	}
	var got map[string]string
	if json.Unmarshal(output, &got) != nil {
		t.Fatalf("invalid parser fixture result: %s", output)
	}
	want := map[string]string{
		"missing": "absent", "config_files": "absent", "installed": "installed",
		"exit_two_empty": "error", "zero_malformed": "error", "foreign_version": "error",
		"foreign_architecture": "error", "foreign_package": "error", "foreign_status": "error",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dpkg record truth table is not closed: got %v want %v", got, want)
	}
}

func TestOnboardingLegacyPackageReviewSummaryAndAdmission(t *testing.T) {
	receipt, _ := onboardingLegacyPackageFixture()
	existing := onboardingExistingInstallation{LegacyPackageStatus: onboardingLegacyPackageAdmitted, LegacyPackage: &receipt}
	summary := existing.Summary()
	if summary.LegacyPackageDisposition != onboardingLegacyPackageAdmitted || summary.LegacyPackageStatus != receipt.Status || summary.LegacyPackage != "nvpair" || summary.LegacyPackageVersion != receipt.Version || summary.LegacyPackageArchitecture != receipt.Architecture || summary.LegacyAppSHA256 != receipt.App.SHA256 || summary.LegacyLauncherSHA256 != receipt.Launcher.SHA256 || summary.LegacyRollbackSHA256 != receipt.Rollback.SHA256 || summary.LegacyRollbackBytes != receipt.Rollback.Bytes {
		t.Fatalf("review summary lost exact package identity: %+v", summary)
	}
	onboardingLegacyPackageAdmission(&existing, false)
	if existing.LegacyPackage != nil || existing.LegacyPackageStatus != onboardingLegacyPackageNoElevation {
		t.Fatalf("package effect was retained without administrator admission: %+v", existing)
	}
	if !strings.Contains(onboardingLegacyPackageDisposition(existing), "legacy Debian desktop package retained because administrator access was not admitted") {
		t.Fatal("unadmitted package changed the bound-CLI-only completion path")
	}
}

func TestOnboardingLegacyPackageInvocationKeepsSecretOffCommandAndJournal(t *testing.T) {
	plan, _ := onboardingUpgradeFixture(t, 13)
	receipt, _ := onboardingLegacyPackageFixture()
	plan.ExistingInstallation.LegacyPackageStatus = onboardingLegacyPackageAdmitted
	plan.ExistingInstallation.LegacyPackage = &receipt
	secret := "synthetic-administrator-input"
	command, input, err := onboardingLegacyPackageInvocation("remove", plan.Receipt, *plan.ExistingInstallation, strings.Repeat("e", 64), secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(command, secret) || !strings.HasPrefix(command, "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c ") {
		t.Fatal("administrator secret escaped the fixed stdin-only route")
	}
	if string(input) != secret+"\n" {
		t.Fatal("administrator input framing changed")
	}
	var header struct {
		Action   string                         `json:"action"`
		Receipt  onboardingInstallReceipt       `json:"receipt"`
		Existing onboardingExistingInstallation `json:"existing"`
	}
	token := command[strings.LastIndex(command, " ")+1:]
	encoded := strings.TrimSuffix(strings.TrimPrefix(token, "'"), "'")
	payload, decodeErr := base64.StdEncoding.DecodeString(encoded)
	if decodeErr != nil || json.Unmarshal(payload, &header) != nil || header.Action != "remove" || header.Receipt != plan.Receipt || !reflect.DeepEqual(header.Existing, *plan.ExistingInstallation) {
		t.Fatal("fixed package action lost its reviewed binding")
	}
	if _, _, err := onboardingLegacyPackageInvocation("remove", plan.Receipt, *plan.ExistingInstallation, strings.Repeat("e", 64), "line\nbreak"); err == nil {
		t.Fatal("multiline administrator input was accepted")
	}
}

func TestOnboardingLegacyPackageArgumentWorksWithPromptAndNOPASSWD(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("Python interpreter unavailable for sudo framing regression")
	}
	payload := []byte(`{"action":"remove","review":"nonsecret"}`)
	encoded := base64.StdEncoding.EncodeToString(payload)
	script := "import base64,binascii,sys\n" + onboardingLegacyPackageArgumentScript + "\nsys.stdout.buffer.write(legacy_package_request_argument())\n"
	for _, test := range []struct {
		name  string
		stdin string
	}{
		{"prompt consumed password", ""},
		{"cached NOPASSWD left password unread", "synthetic-administrator-input\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(python, "-c", script, encoded)
			command.Stdin = strings.NewReader(test.stdin)
			output, err := command.CombinedOutput()
			if err != nil || !bytes.Equal(output, payload) {
				t.Fatalf("argv request changed with sudo stdin behavior: %v %q", err, output)
			}
		})
	}
	if strings.Contains(onboardingLegacyPackageRootScript, "sys.stdin.buffer.read") {
		t.Fatal("root child still conditionally consumes sudo password stdin")
	}
}

func testOnboardingLegacyAction(t *testing.T, command string, input io.Reader, password string) string {
	t.Helper()
	rawInput, _ := io.ReadAll(input)
	if string(rawInput) != password+"\n" {
		t.Fatal("root action did not keep password-only stdin")
	}
	token := command[strings.LastIndex(command, " ")+1:]
	encoded := strings.TrimSuffix(strings.TrimPrefix(token, "'"), "'")
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal("invalid root action argument", err)
	}
	var request struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(payload, &request) != nil {
		t.Fatal("invalid root action request")
	}
	return request.Action
}

func TestOnboardingLegacyPackageRemovalImmediatelyRevalidatesMembership(t *testing.T) {
	plan, _ := onboardingUpgradeFixture(t, 13)
	legacy, _ := onboardingLegacyPackageFixture()
	plan.ExistingInstallation.LegacyPackageStatus, plan.ExistingInstallation.LegacyPackage = onboardingLegacyPackageAdmitted, &legacy
	old := *plan.ExistingInstallation
	f := newOnboardingControllerFixture(t, 1)
	f.s.testCluster = func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		switch method {
		case "cluster:get-node-id":
			return onboardingMarshal(map[string]string{"nodeUuid": onboardingFixtureController, "clusterId": old.ClusterID}), nil
		case "nodes:get-initial":
			return onboardingMarshal(map[string]any{"nodes": []map[string]string{{"nodeUuid": old.NodeID, "state": "member"}}}), nil
		}
		t.Fatal("unexpected local membership method", method)
		return nil, nil
	}
	events := []string{}
	client := &onboardingSSH{testRun: func(_ context.Context, command string, input io.Reader) ([]byte, error) {
		if strings.HasPrefix(command, "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c ") {
			action := testOnboardingLegacyAction(t, command, input, "synthetic-admin")
			if action != "remove" && action != "commit" {
				t.Fatal("root action was not exact or stdin-bound")
			}
			events = append(events, action)
			state := "retired"
			if action == "commit" {
				state = "committed"
			}
			return onboardingMarshal(onboardingLegacyPackageResult{Retired: true, SuccessorStarted: true, State: state}), nil
		}
		if command == onboardingPython(onboardingOwnedScript+onboardingControlScript) {
			var request struct {
				Method string `json:"method"`
			}
			raw, _ := io.ReadAll(input)
			_ = json.Unmarshal(raw, &request)
			events = append(events, request.Method)
			switch request.Method {
			case "cluster:get-node-id":
				return onboardingMarshal(map[string]any{"result": map[string]string{"nodeUuid": old.NodeID, "clusterId": old.ClusterID, "certFingerprint": old.CertFingerprint}}), nil
			case "nodes:get-initial":
				return onboardingMarshal(map[string]any{"result": map[string]any{"nodes": []map[string]string{{"nodeUuid": onboardingFixtureController, "state": "member"}}}}), nil
			}
		}
		if command == onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeVerifyScript) {
			events = append(events, "identity")
			return onboardingMarshal(map[string]string{"nodeId": old.NodeID, "clusterId": old.ClusterID, "certFingerprint": old.CertFingerprint}), nil
		}
		t.Fatal("unexpected retirement command")
		return nil, nil
	}}
	if err := f.s.retireOnboardingLegacyPackage(context.Background(), client, plan.Receipt, old, onboardingAccess{elevationPassword: "synthetic-admin"}, onboardingFixtureController); err != nil {
		t.Fatal(err)
	}
	want := []string{"remove", "cluster:get-node-id", "nodes:get-initial", "identity", "commit"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("package retirement was not followed immediately by reciprocal proof: got %v want %v", events, want)
	}
}

func TestOnboardingLegacyPackageRollbackUsesFreshFiniteContext(t *testing.T) {
	for _, mode := range []string{"cancelled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			plan, _ := onboardingUpgradeFixture(t, 13)
			legacy, _ := onboardingLegacyPackageFixture()
			plan.ExistingInstallation.LegacyPackageStatus, plan.ExistingInstallation.LegacyPackage = onboardingLegacyPackageAdmitted, &legacy
			old := *plan.ExistingInstallation
			f := newOnboardingControllerFixture(t, 1)
			var ctx context.Context
			var cancel context.CancelFunc
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			f.s.testCluster = func(call context.Context, method string, _ any) (json.RawMessage, error) {
				if err := call.Err(); err != nil {
					return nil, err
				}
				switch method {
				case "cluster:get-node-id":
					return onboardingMarshal(map[string]string{"nodeUuid": onboardingFixtureController, "clusterId": old.ClusterID}), nil
				case "nodes:get-initial":
					return onboardingMarshal(map[string]any{"nodes": []map[string]string{{"nodeUuid": old.NodeID, "state": "member"}}}), nil
				}
				return nil, errors.New("unexpected local membership method")
			}
			events := []string{}
			client := &onboardingSSH{testRun: func(call context.Context, command string, input io.Reader) ([]byte, error) {
				if strings.HasPrefix(command, "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c ") {
					action := testOnboardingLegacyAction(t, command, input, "synthetic-admin")
					events = append(events, action)
					switch action {
					case "remove":
						if mode == "deadline" {
							<-call.Done()
						} else {
							cancel()
						}
						return onboardingMarshal(onboardingLegacyPackageResult{Retired: true, SuccessorStarted: true, State: "retired"}), nil
					case "rollback":
						if call.Err() != nil {
							t.Fatal("rollback inherited the canceled or expired request")
						}
						if _, ok := call.Deadline(); !ok {
							t.Fatal("rollback recovery context is not finite")
						}
						return onboardingMarshal(onboardingLegacyPackageResult{RollbackConfirmed: true, SuccessorStarted: true, State: "rolled-back", FailureCode: "post-removal-revalidation-failed"}), nil
					}
				}
				if err := call.Err(); err != nil {
					return nil, err
				}
				if command == onboardingPython(onboardingOwnedScript+onboardingControlScript) {
					var request struct {
						Method string `json:"method"`
					}
					raw, _ := io.ReadAll(input)
					_ = json.Unmarshal(raw, &request)
					events = append(events, request.Method)
					switch request.Method {
					case "cluster:get-node-id":
						return onboardingMarshal(map[string]any{"result": map[string]string{"nodeUuid": old.NodeID, "clusterId": old.ClusterID, "certFingerprint": old.CertFingerprint}}), nil
					case "nodes:get-initial":
						return onboardingMarshal(map[string]any{"result": map[string]any{"nodes": []map[string]string{{"nodeUuid": onboardingFixtureController, "state": "member"}}}}), nil
					}
				}
				if command == onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeVerifyScript) {
					events = append(events, "identity")
					return onboardingMarshal(map[string]string{"nodeId": old.NodeID, "clusterId": old.ClusterID, "certFingerprint": old.CertFingerprint}), nil
				}
				return nil, errors.New("unexpected recovery command")
			}}
			err := f.s.retireOnboardingLegacyPackage(ctx, client, plan.Receipt, old, onboardingAccess{elevationPassword: "synthetic-admin"}, onboardingFixtureController)
			if err == nil || !strings.Contains(err.Error(), "restored") {
				t.Fatalf("post-remove proof failure did not return confirmed rollback: %v", err)
			}
			want := []string{"remove", "rollback", "cluster:get-node-id", "nodes:get-initial", "identity"}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("fresh rollback sequence changed: got %v want %v", events, want)
			}
		})
	}
}

func TestOnboardingLegacyPackageResultIsFailClosed(t *testing.T) {
	if err := validateOnboardingLegacyPackageResult("remove", onboardingLegacyPackageResult{Retired: true, SuccessorStarted: true, State: "retired"}); err != nil {
		t.Fatal(err)
	}
	rolledBack := onboardingLegacyPackageResult{RollbackConfirmed: true, SuccessorStarted: true, State: "rolled-back", FailureCode: "package-action-failed"}
	if err := validateOnboardingLegacyPackageResult("remove", rolledBack); err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatal("confirmed rollback was mistaken for retirement success")
	}
	if err := validateOnboardingLegacyPackageResult("rollback", rolledBack); err != nil {
		t.Fatal("exact confirmed rollback was not accepted", err)
	}
	for _, reconciled := range []onboardingLegacyPackageResult{
		{SuccessorStarted: true, State: "ready"},
		{Retired: true, SuccessorStarted: true, State: "retired"},
		rolledBack,
	} {
		if err := validateOnboardingLegacyPackageResult("reconcile", reconciled); err != nil {
			t.Fatal("safe retained reconciliation was not accepted", err)
		}
	}
	if err := validateOnboardingLegacyPackageResult("commit", onboardingLegacyPackageResult{Retired: true, SuccessorStarted: true, State: "committed"}); err != nil {
		t.Fatal("validated retirement commit was not accepted", err)
	}
	for _, result := range []onboardingLegacyPackageResult{
		{},
		{State: "retired", SuccessorStarted: true},
		{State: "rolled-back", RollbackConfirmed: true, FailureCode: "failed"},
		{State: "rolled-back", SuccessorStarted: true, FailureCode: "failed"},
	} {
		if validateOnboardingLegacyPackageResult("remove", result) == nil {
			t.Fatal("ambiguous package result was accepted")
		}
	}
}

func TestOnboardingLegacyPackageScriptsAreFixedAndCompile(t *testing.T) {
	for _, required := range []string{
		"['/usr/bin/dpkg','--remove','nvpair']",
		"['/usr/bin/dpkg','--install',archive]",
		"nvidia-pair-legacy-package-retirement-v1",
		".nvpair-rollback-",
		"stop_successor()",
		"start_successor()",
		"seal_proof('intent'",
		"seal_proof('retired'",
		"seal_proof('rolled-back'",
		"seal_proof('commit-intent'",
		"seal_proof('committed'",
		"legacy_package_record_state(code,record,legacy['version'],legacy['architecture'])",
		"state,record=package_state()",
	} {
		if !strings.Contains(onboardingLegacyPackageRootScript, required) {
			t.Fatalf("root transaction lost fixed contract %q", required)
		}
	}
	for _, forbidden := range []string{"rm -rf", "shell=True", "os.kill(", "pkill", "killall", "apt-get", "shutil.rmtree"} {
		if strings.Contains(onboardingLegacyPackageRootScript, forbidden) {
			t.Fatalf("root transaction gained forbidden broad effect %q", forbidden)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("Python interpreter unavailable for source-only syntax check")
	}
	for name, script := range map[string]string{
		"inspection": onboardingUpgradeInspectScript,
		"root":       onboardingLegacyPackageRootScript,
	} {
		command := exec.Command(python, "-c", "import sys; compile(sys.stdin.read(), '<"+name+">', 'exec')")
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s script does not compile: %v: %s", name, err, output)
		}
	}
}

func TestOnboardingLegacyPackageCompletionCopyIsTruthful(t *testing.T) {
	plan, _ := onboardingUpgradeFixture(t, 13)
	if got := onboardingLegacyPackageCompletion(*plan.ExistingInstallation); !strings.Contains(got, "legacy CLI inventory retired") {
		t.Fatal(got)
	}
	managed, _ := managedOnboardingPredecessor(t)
	if got := onboardingLegacyPackageCompletion(*managed.ExistingInstallation); !strings.Contains(got, "prior managed bundle retained as the registered rollback") || strings.Contains(got, "old CLI payload retired") {
		t.Fatal(got)
	}
	receipt, _ := onboardingLegacyPackageFixture()
	plan.ExistingInstallation.LegacyPackageStatus, plan.ExistingInstallation.LegacyPackage = onboardingLegacyPackageAdmitted, &receipt
	if got := onboardingLegacyPackageCompletion(*plan.ExistingInstallation); !strings.Contains(got, "legacy Debian desktop package retired with an exact reviewed rollback archive") {
		t.Fatal(got)
	}
	plan.ExistingInstallation.LegacyPackageStatus, plan.ExistingInstallation.LegacyPackage = onboardingLegacyPackageNoRollback, nil
	if got := onboardingLegacyPackageCompletion(*plan.ExistingInstallation); !strings.Contains(got, "desktop package retained because no unique exact rollback archive was admitted") {
		t.Fatal(got)
	}
}
