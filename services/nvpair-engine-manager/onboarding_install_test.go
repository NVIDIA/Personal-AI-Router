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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type onboardingInstallCall struct {
	command string
	input   []byte
}
type onboardingInstallFake struct {
	calls []onboardingInstallCall
	reply func(int, onboardingInstallCall) ([]byte, error)
}

func (f *onboardingInstallFake) run(_ context.Context, command string, input io.Reader) ([]byte, error) {
	var data []byte
	if input != nil {
		data, _ = io.ReadAll(input)
	}
	call := onboardingInstallCall{command: command, input: data}
	f.calls = append(f.calls, call)
	return f.reply(len(f.calls), call)
}

func onboardingInstallFixture(t *testing.T) (onboardingPlatformInfo, onboardingPackage, string) {
	t.Helper()
	file, artifact := writeOnboardingFixture(t, onboardingFixtureEntries(183))
	pkg, err := prepareOnboardingPackage(context.Background(), t.TempDir(), onboardingArtifactSource{onboardingArtifact: artifact, File: file})
	if err != nil {
		t.Fatal(err)
	}
	return onboardingPlatformInfo{Home: "/home/approved-user", UID: 1000, OS: "Linux", Arch: "aarch64"}, pkg, strings.Repeat("b", 32)
}

func TestOnboardingInstallTransportAndFixedServiceSequence(t *testing.T) {
	info, pkg, id := onboardingInstallFixture(t)
	expected, err := onboardingInstallPaths(info, pkg, id)
	if err != nil {
		t.Fatal(err)
	}
	expected.StartupLifetime = "session"
	expected.ManifestSHA256, _ = onboardingPackageManifestHash(pkg)
	packageBytes, err := os.ReadFile(pkg.file)
	if err != nil {
		t.Fatal(err)
	}
	fake := &onboardingInstallFake{}
	fake.reply = func(index int, call onboardingInstallCall) ([]byte, error) {
		if index == 1 {
			if call.command != onboardingPython(onboardingReceiveScript) {
				t.Fatal("transfer was not the fixed receiver")
			}
			parts := bytes.SplitN(call.input, []byte("\n"), 2)
			if len(parts) != 2 || !bytes.Equal(parts[1], packageBytes) {
				t.Fatal("transfer did not consume the exact byte stream")
			}
			var header map[string]any
			if json.Unmarshal(parts[0], &header) != nil || header["operationId"] != id || header["sha256"] != pkg.source.SHA256 {
				t.Fatal("transfer identity was not bound")
			}
			receipt := expected
			receipt.Installed = true
			receipt.Recoverable = true
			return onboardingMarshal(receipt), nil
		}
		if index == 4 {
			if call.command != onboardingPython(onboardingOwnedScript+onboardingRecoveryScript) {
				t.Fatal("identity recovery escaped fixed command")
			}
			return []byte(`{"nodeId":"owned-unpaired-node","state":"installed-not-paired","recoverable":true}`), nil
		}
		if call.command != onboardingPython(onboardingOwnedScript+onboardingServiceScript) {
			t.Fatal("service action escaped fixed product command")
		}
		var body struct {
			Receipt  onboardingInstallReceipt `json:"receipt"`
			Action   string                   `json:"action"`
			Lifetime string                   `json:"lifetime"`
		}
		if json.Unmarshal(call.input, &body) != nil || body.Receipt.BundlePath != expected.BundlePath || body.Lifetime != "session" {
			t.Fatal("service action was not receipt-bound")
		}
		action, state := "install", "installed"
		if index == 3 {
			action, state = "start", "started"
		}
		if body.Action != action {
			t.Fatalf("action=%s", body.Action)
		}
		return onboardingMarshal(map[string]string{"unit": "nvidia-pair-headless.service", "operation": action, "state": state, "persistence": "user-session", "requestedLifetime": "session", "effectiveLifetime": "session"}), nil
	}
	var stages []string
	receipt, err := installOnboardingWithRunner(context.Background(), fake, info, pkg, id, "session", func(stage, message string) { stages = append(stages, stage) })
	if err != nil || !receipt.Installed || !receipt.ServiceInstalled || !receipt.ServiceStarted || receipt.NodeID != "owned-unpaired-node" || !receipt.Recoverable || len(fake.calls) != 4 || len(stages) != 3 {
		t.Fatalf("receipt=%+v calls=%d err=%v", receipt, len(fake.calls), err)
	}
}

func TestOnboardingInstallUncertainTransferNeverStartsService(t *testing.T) {
	info, pkg, id := onboardingInstallFixture(t)
	fake := &onboardingInstallFake{reply: func(int, onboardingInstallCall) ([]byte, error) { return nil, errors.New("interrupted") }}
	receipt, err := installOnboardingWithRunner(context.Background(), fake, info, pkg, id, "persistent", nil)
	if err == nil || receipt.Installed || receipt.CleanupConfirmed || len(fake.calls) != 1 {
		t.Fatalf("uncertain transfer=%+v %v", receipt, err)
	}
}

func TestOnboardingInstallRejectsMismatchedActivationReceipt(t *testing.T) {
	info, pkg, id := onboardingInstallFixture(t)
	fake := &onboardingInstallFake{reply: func(int, onboardingInstallCall) ([]byte, error) {
		return onboardingMarshal(onboardingInstallReceipt{Installed: true, BundlePath: "/another/target"}), nil
	}}
	if _, err := installOnboardingWithRunner(context.Background(), fake, info, pkg, id, "persistent", nil); err == nil || len(fake.calls) != 1 {
		t.Fatal("mismatched target continued into service actions")
	}
}

func TestOnboardingServiceRejectsLifetimeFallbackBeforeReportingStarted(t *testing.T) {
	fake := &onboardingInstallFake{reply: func(int, onboardingInstallCall) ([]byte, error) {
		return []byte(`{"unit":"nvidia-pair-headless.service","operation":"start","state":"started","persistence":"user-session","requestedLifetime":"persistent","effectiveLifetime":"session"}`), nil
	}}
	if err := runOnboardingService(context.Background(), fake, onboardingInstallReceipt{}, "start", "persistent"); err == nil {
		t.Fatal("silent session-only fallback accepted")
	}
	if err := runOnboardingService(context.Background(), fake, onboardingInstallReceipt{}, "start; other", "session"); err == nil || len(fake.calls) != 1 {
		t.Fatal("free-form service action reached transport")
	}
}

func TestOnboardingCleanupUsesOnlyOwnedReceiptAndRetainsBundles(t *testing.T) {
	info, pkg, id := onboardingInstallFixture(t)
	receipt, _ := onboardingInstallPaths(info, pkg, id)
	receipt.StartupLifetime = "persistent"
	receipt.ManifestSHA256, _ = onboardingPackageManifestHash(pkg)
	fake := &onboardingInstallFake{reply: func(_ int, call onboardingInstallCall) ([]byte, error) {
		if call.command != onboardingPython(onboardingOwnedScript+onboardingCleanupScript) {
			t.Fatal("cleanup was not fixed product code")
		}
		var body struct {
			Receipt onboardingInstallReceipt `json:"receipt"`
		}
		if json.Unmarshal(call.input, &body) != nil || body.Receipt.StartupLifetime != "persistent" {
			t.Fatal("cleanup lost the approved persistent lifetime")
		}
		return []byte(`{"cleanupConfirmed":true,"bundleRetained":true}`), nil
	}}
	if clean, err := cleanupOnboardingWithRunner(context.Background(), fake, receipt); err != nil || !clean {
		t.Fatalf("cleanup=%v %v", clean, err)
	}
	receipt.StagePath = "/"
	if _, err := cleanupOnboardingWithRunner(context.Background(), fake, receipt); err == nil || len(fake.calls) != 1 {
		t.Fatal("invalid cleanup target reached transport")
	}
}

// These tests execute the actual installed-file verifier with private fixture
// files. Only the account-home lookup is substituted; no SSH, PAIR service,
// systemd, GPU or executable from the fixture is invoked.
func onboardingPythonVerifierFixture(t *testing.T) (onboardingInstallReceipt, string) {
	t.Helper()
	home := t.TempDir()
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	op := strings.Repeat("b", 32)
	root := filepath.Join(home, ".local", "share", "Nvidia Corporation", "Personal AI Router")
	bundle := filepath.Join(root, "bundles", digest)
	if err := os.MkdirAll(filepath.Join(bundle, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	var manifest []byte
	for _, entry := range onboardingFixtureEntries(183) {
		name := filepath.Base(entry.name)
		if err := os.WriteFile(filepath.Join(bundle, "bin", name), entry.body, 0755); err != nil {
			t.Fatal(err)
		}
		if name == "manifest.json" {
			manifest = entry.body
		}
	}
	sum := sha256.Sum256(manifest)
	receipt := onboardingInstallReceipt{StagePath: filepath.Join(root, ".onboarding", op), BundlePath: bundle, TUIPath: filepath.Join(bundle, "bin", "nvpair-tui"), ArtifactSHA256: digest, ManifestSHA256: hex.EncodeToString(sum[:]), StartupLifetime: "persistent", Installed: true}
	marker := map[string]any{"owner": "nvidia-pair-onboarding-v1", "operationId": op, "sha256": digest, "manifestSha256": receipt.ManifestSHA256, "startupLifetime": receipt.StartupLifetime}
	if err := os.WriteFile(filepath.Join(bundle, ".pair-onboarding.json"), onboardingMarshal(marker), 0600); err != nil {
		t.Fatal(err)
	}
	return receipt, home
}

func executeOnboardingPythonFixture(t *testing.T, home string, receipt onboardingInstallReceipt, tail string) ([]byte, error) {
	t.Helper()
	python := os.Getenv("NVPAIR_ONBOARDING_TEST_PYTHON")
	if python == "" {
		t.Skip("set NVPAIR_ONBOARDING_TEST_PYTHON to an existing Python3 for executable verifier checks")
	}
	bootstrap := `import os,sys,types
home=os.environ['PAIR_FIXTURE_HOME']
sys.modules['pwd']=types.SimpleNamespace(getpwuid=lambda uid:types.SimpleNamespace(pw_dir=home))
if not hasattr(os,'geteuid'): os.geteuid=lambda:0
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", bootstrap+onboardingOwnedScript+tail)
	cmd.Env = append(os.Environ(), "PAIR_FIXTURE_HOME="+home)
	cmd.Stdin = bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt}))
	return cmd.CombinedOutput()
}

func TestOnboardingInstalledPythonVerifierRejectsChildAndManifestMutation(t *testing.T) {
	for _, mutation := range []string{"none", "child", "manifest-and-tui"} {
		t.Run(mutation, func(t *testing.T) {
			receipt, home := onboardingPythonVerifierFixture(t)
			if mutation == "child" {
				if err := os.WriteFile(filepath.Join(receipt.BundlePath, "bin", "nvpair-ui-broker"), []byte("changed child"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if mutation == "manifest-and-tui" {
				body := []byte("changed tui")
				if err := os.WriteFile(receipt.TUIPath, body, 0755); err != nil {
					t.Fatal(err)
				}
				manifestPath := filepath.Join(receipt.BundlePath, "bin", "manifest.json")
				raw, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				var manifest map[string]any
				if json.Unmarshal(raw, &manifest) != nil {
					t.Fatal("invalid fixture manifest")
				}
				sum := sha256.Sum256(body)
				for _, entry := range manifest["files"].([]any) {
					file := entry.(map[string]any)
					if file["fileName"] == "nvpair-tui" {
						file["sha256"] = hex.EncodeToString(sum[:])
						file["size"] = len(body)
					}
				}
				if err := os.WriteFile(manifestPath, onboardingMarshal(manifest), 0644); err != nil {
					t.Fatal(err)
				}
			}
			output, err := executeOnboardingPythonFixture(t, home, receipt, "\nverify_tui()\nprint('verified')\n")
			if mutation == "none" && (err != nil || !strings.Contains(string(output), "verified")) {
				t.Fatalf("valid fixture failed: %s %v", output, err)
			}
			if mutation != "none" && err == nil {
				t.Fatal("mutated installed bundle was admitted")
			}
		})
	}
}

func TestOnboardingMissingBundleCannotConfirmServiceCleanup(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".local", "share", "Nvidia Corporation", "Personal AI Router")
			digest := strings.Repeat("a", 64)
			bundle := filepath.Join(root, "bundles", digest)
			receipt := onboardingInstallReceipt{StagePath: filepath.Join(root, ".onboarding", strings.Repeat("b", 32)), BundlePath: bundle, TUIPath: filepath.Join(bundle, "bin", "nvpair-tui"), ArtifactSHA256: digest, ManifestSHA256: strings.Repeat("c", 64), StartupLifetime: "persistent", Installed: true, ServiceStarted: started}
			output, err := executeOnboardingPythonFixture(t, home, receipt, onboardingCleanupScript)
			if err == nil || strings.Contains(string(output), `"cleanupConfirmed": true`) {
				t.Fatalf("absent bundle falsely clean: %s %v", output, err)
			}
		})
	}
}

func TestOnboardingFinishOnlyUsesStagingCleanupAfterPairing(t *testing.T) {
	info, pkg, id := onboardingInstallFixture(t)
	receipt, _ := onboardingInstallPaths(info, pkg, id)
	receipt.StartupLifetime = "persistent"
	receipt.ManifestSHA256, _ = onboardingPackageManifestHash(pkg)
	receipt.Installed = true
	receipt.ServiceStarted = true
	fake := &onboardingInstallFake{reply: func(_ int, call onboardingInstallCall) ([]byte, error) {
		if call.command != onboardingPython(onboardingOwnedScript+onboardingFinishScript) {
			t.Fatal("completion used cancellation/service teardown path")
		}
		return []byte(`{"stagingCleaned":true}`), nil
	}}
	if clean, err := finishOnboardingWithRunner(context.Background(), fake, receipt); err != nil || !clean {
		t.Fatalf("finish=%v %v", clean, err)
	}
	if strings.Contains(onboardingFinishScript, "uninstall") || strings.Contains(onboardingFinishScript, "admission.json") {
		t.Fatal("completion changes service or membership")
	}
}

func TestOnboardingResumeRejectsChangedOriginalBinding(t *testing.T) {
	info, pkg, id := onboardingInstallFixture(t)
	receipt, _ := onboardingInstallPaths(info, pkg, id)
	receipt.ManifestSHA256 = strings.Repeat("0", 64)
	receipt.StartupLifetime = "persistent"
	fake := &onboardingInstallFake{reply: func(int, onboardingInstallCall) ([]byte, error) {
		t.Fatal("changed artifact reached target")
		return nil, nil
	}}
	if _, err := resumeOnboardingWithRunner(context.Background(), fake, info, pkg, receipt, nil); err == nil {
		t.Fatal("changed original manifest accepted")
	}
}

func TestOnboardingPythonRecoveryPreservesExistingIdentityAndRefusesMissingMetadata(t *testing.T) {
	receipt, home := onboardingPythonVerifierFixture(t)
	receipt.NodeID = "previously-confirmed-node"
	cluster := filepath.Join(home, ".config", "Nvidia Corporation", "Personal AI Router", "cluster")
	if err := os.MkdirAll(cluster, 0700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(cluster, "identity.json")
	if err := os.WriteFile(identity, []byte(`{"node_uuid":"previously-confirmed-node"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cluster, "node.crt"), []byte("-----BEGIN CERTIFICATE-----\nAQID\n-----END CERTIFICATE-----\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(cluster, "node.key")
	canary := []byte("test-only-key-preservation-canary")
	if err := os.WriteFile(key, canary, 0600); err != nil {
		t.Fatal(err)
	}
	marker := map[string]any{"owner": "nvidia-pair-onboarding-v1", "operationId": filepath.Base(receipt.StagePath), "sha256": receipt.ArtifactSHA256, "manifestSha256": receipt.ManifestSHA256, "startupLifetime": receipt.StartupLifetime}
	fingerprint := sha256.Sum256([]byte{1, 2, 3})
	state := map[string]any{"owner": marker, "nodeId": receipt.NodeID, "certFingerprint": "sha256:" + hex.EncodeToString(fingerprint[:]), "stage": "installed-not-paired"}
	if err := os.WriteFile(filepath.Join(receipt.BundlePath, ".pair-onboarding-state.json"), onboardingMarshal(state), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := executeOnboardingPythonFixture(t, home, receipt, "\nverify_tui()\nprint('verified')\n"); err != nil {
		t.Fatalf("existing public binding did not verify: %s %v", output, err)
	}
	if err := os.Remove(identity); err != nil {
		t.Fatal(err)
	}
	if _, err := executeOnboardingPythonFixture(t, home, receipt, "\nverify_tui()\n"); err == nil {
		t.Fatal("missing identity metadata was accepted before a restart")
	}
	data, err := os.ReadFile(key)
	if err != nil || !bytes.Equal(data, canary) {
		t.Fatal("identity-preservation fixture was modified")
	}
}

func TestOnboardingFailedResumePreservesPriorEffectsForMissingBundleCleanup(t *testing.T) {
	info, pkg, id := onboardingInstallFixture(t)
	prior, _ := onboardingInstallPaths(info, pkg, id)
	prior.ManifestSHA256, _ = onboardingPackageManifestHash(pkg)
	prior.StartupLifetime = "persistent"
	prior.NodeID = "original-owned-node"
	prior.Installed = true
	prior.ServiceInstalled = true
	prior.ServiceStarted = true
	prior.Recoverable = true
	prior.CleanupConfirmed = true
	fake := &onboardingInstallFake{reply: func(index int, call onboardingInstallCall) ([]byte, error) {
		if index == 1 {
			return []byte(`{"resumable":true}`), nil
		}
		return nil, errors.New("transfer acknowledgement lost")
	}}
	resumed, err := resumeOnboardingWithRunner(context.Background(), fake, info, pkg, prior, nil)
	if err == nil || !resumed.Installed || !resumed.ServiceInstalled || !resumed.ServiceStarted || !resumed.Recoverable || resumed.CleanupConfirmed || resumed.NodeID != prior.NodeID || resumed.ManifestSHA256 != prior.ManifestSHA256 || resumed.StartupLifetime != prior.StartupLifetime || resumed.BundlePath != prior.BundlePath {
		t.Fatalf("failed resume weakened ownership receipt: %+v %v", resumed, err)
	}
	// Run the real cleanup predicate with an absent bundle under a private
	// fixture home. The preserved possible effects must prevent a false clean.
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", "Nvidia Corporation", "Personal AI Router")
	resumed.StagePath = filepath.Join(root, ".onboarding", id)
	resumed.BundlePath = filepath.Join(root, "bundles", resumed.ArtifactSHA256)
	resumed.TUIPath = filepath.Join(resumed.BundlePath, "bin", "nvpair-tui")
	if output, err := executeOnboardingPythonFixture(t, home, resumed, onboardingCleanupScript); err == nil || strings.Contains(string(output), `"cleanupConfirmed": true`) {
		t.Fatalf("failed-resume cleanup was falsely confirmed: %s %v", output, err)
	}
}
