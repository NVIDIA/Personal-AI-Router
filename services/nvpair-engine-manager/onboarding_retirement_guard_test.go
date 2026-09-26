// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Fixtures execute the actual Python guard/proof logic against private temporary
// files. Kernel visibility and busy outcomes are substituted; no old ELF, peer,
// systemd service, or installed product is executed or changed.
func runOnboardingRetirementPythonFixture(t *testing.T, program string) map[string]json.RawMessage {
	t.Helper()
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("the required Python fixture interpreter is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	script := filepath.Join(root, "fixture.py")
	if err := os.WriteFile(script, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, python, "-I", "-B", script)
	cmd.Env = append(os.Environ(), "PAIR_RETIREMENT_TEST_ROOT="+root)
	stdout := &onboardingBoundedOutput{max: 128 << 10}
	stderr := &onboardingBoundedOutput{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(string(stderr.Bytes()))
		if len(message) > 4096 {
			message = message[len(message)-4096:]
		}
		t.Fatalf("retirement Python fixture failed: %v: %s", err, message)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("retirement fixture returned invalid result: %v", err)
	}
	return result
}

func TestOnboardingRetirementExecutableGuardAndProof(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the retirement worker executes on Linux; Windows short-path aliases invalidate this Python path-custody fixture")
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "onboarding_retirement_guard_fixture.py"))
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	for _, name := range []string{
		"legacy-thirteen", "current-fifteen", "lost-reply-and-cleanup", "busy", "permission", "hardlink", "redirection", "hardlink-after-unlink",
		"inode-race", "hash-race", "mode-race", "owner-race", "identity-race",
		"missing-legacy-elf", "missing-legacy-manifest", "duplicate-alias", "partial-valid", "partial-alias", "pending-present", "pending-missing",
		"partial-missing-proof", "partial-tampered-binding", "partial-tampered-inode",
		"partial-replaced-inode", "partial-proof-mode", "partial-wrong-stage-owner", "partial-prefix-tamper", "partial-pending-tamper", "partial-publication-uncertain",
		"ancestry-private-anchor", "ancestry-initial-alias", "ancestry-pending-present", "ancestry-pending-missing",
		"ancestry-writable-before-anchor", "ancestry-foreign-descendant", "ancestry-redirect", "ancestry-stage-mode", "ancestry-proof-mode",
		"managed-inspect-next-upgrade", "managed-inspect-unanchored-parent", "managed-inspect-writable-file", "managed-inspect-foreign-owner", "managed-inspect-redirect",
	} {
		t.Run(name, func(t *testing.T) {
			program := string(fixture) + "\nexec(" + quote(onboardingUpgradeInspectPrelude) + ")\nready_fixture()\n" +
				"case=" + quote(name) + "\nbody=" + quote(onboardingUpgradeRetireScript) + "\nfinish=" + quote(onboardingFinishScript) +
				"\nprint(json.dumps(run_case(case,body,finish)))\n"
			result := runOnboardingRetirementPythonFixture(t, program)
			if string(result["passed"]) != "true" || string(result["remainingGuards"]) != "0" || string(result["oldELFWrites"]) != "0" {
				t.Fatalf("fixture did not finish with all guards released: %s", onboardingMarshal(result))
			}
		})
	}
}

func TestOnboardingRetirementBusyResultIsAllowlisted(t *testing.T) {
	plan, _ := onboardingUpgradeFixture(t, 13)
	for _, tc := range []struct {
		name string
		body string
		err  error
		busy bool
	}{
		{"busy", `{"retired":false,"retiredEntries":0,"launcher":"unconfirmed","scope":"bound-cli-only","failureCode":"old-executable-busy"}`, nil, true},
		{"unknown-code", `{"retired":false,"retiredEntries":0,"launcher":"unconfirmed","scope":"bound-cli-only","failureCode":"raw-peer-path"}`, nil, false},
		{"transport-error", `{"retired":false,"scope":"bound-cli-only","failureCode":"old-executable-busy"}`, errors.New("closed transport"), false},
		{"foreign-scope", `{"retired":false,"scope":"whole-host","failureCode":"old-executable-busy"}`, nil, false},
		{"success-with-code", `{"retired":true,"retiredEntries":14,"launcher":"absent","scope":"bound-cli-only","failureCode":"old-executable-busy"}`, nil, false},
		{"proof", `{"retired":false,"retiredEntries":0,"launcher":"unconfirmed","scope":"bound-cli-only","failureCode":"retirement-proof-unconfirmed"}`, nil, false},
		{"proof-wrong-count", `{"retired":false,"retiredEntries":1,"launcher":"unconfirmed","scope":"bound-cli-only","failureCode":"retirement-proof-unconfirmed"}`, nil, false},
		{"proof-wrong-launcher", `{"retired":false,"retiredEntries":0,"launcher":"absent","scope":"bound-cli-only","failureCode":"retirement-proof-unconfirmed"}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &onboardingInstallFake{reply: func(_ int, call onboardingInstallCall) ([]byte, error) {
				var request map[string]json.RawMessage
				if err := json.Unmarshal(call.input, &request); err != nil || len(request) != 3 || request["receipt"] == nil || request["existing"] == nil || request["retiredProcessSuffix"] == nil {
					t.Fatal("retirement required fields outside the existing committed-operation contract")
				}
				return []byte(tc.body), tc.err
			}}
			err := retireOnboardingUpgrade(context.Background(), fake, plan.Receipt, *plan.ExistingInstallation)
			if err == nil || errors.Is(err, errOnboardingRetirementBusy) != tc.busy || errors.Is(err, errOnboardingRetirementProof) != (tc.name == "proof") {
				t.Fatalf("unexpected busy classification: %v", err)
			}
		})
	}
}
