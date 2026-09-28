// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/noderec"
)

func TestDiagnosticLocalInspectionPreservesBinaryStdin(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Skip("existing Python interpreter unavailable")
	}
	// Protocol-only fixture, never the native inspector or a system tool probe.
	script := diagnosticInspectionProgram([]byte(`{"nodeId":"fixture-node"}`), "import json,sys\nprint(json.loads(sys.stdin.buffer.read())['nodeId'])")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, python, "-I", "-c", script).Output()
	if err != nil || strings.TrimSpace(string(output)) != "fixture-node" {
		t.Fatalf("local input protocol: %s %v", output, err)
	}
}

func inspectionFixture(t *testing.T, self string) (*diagnosticService, diagnosticInspectionRequest) {
	t.Helper()
	d := diagnosticTestService(t)
	dir := t.TempDir()
	clustertrusttest.Join(t, dir, "cluster", self, "node-a", "node-b")
	d.m.mesh = clustertrust.Open(dir)
	nodes := []noderec.DirectoryNode{}
	for i, id := range []string{"node-a", "node-b"} {
		nodes = append(nodes, noderec.DirectoryNode{HostUUID: id, ClusterUUID: id, IP: fmt.Sprintf("192.0.2.%d", i+1), Services: map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceEngineControl: {Port: 14323}}})
	}
	d.m.peers.set(nodes)
	body, _ := json.Marshal([]string{"node-a", "node-a", "node-b", "node-b"})
	digest := sha256.Sum256(body)
	r := diagnosticInspectionRequest{diagnosticSetupRequest: diagnosticSetupRequest{GroupID: fmt.Sprintf("pair-recipe/nccl-%x", digest[:16]), Members: []diagnosticSetupMember{{NodeID: "node-a", Principal: "node-a"}, {NodeID: "node-b", Principal: "node-b"}}}}
	inventory, err := d.inspectionTargets(r.diagnosticSetupRequest)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range inventory.Targets {
		if row.Local {
			continue
		}
		target := d.m.onboarding.targets[row.Candidate.CandidateID]
		target.candidate.AccessAvailable = true
		target.candidate.AccessID = newOpID()
		target.candidate.HostKeySHA256 = "SHA256:fixture-public-key"
		target.candidate.HostKeyTrusted = true
		target.accessGeneration = newOpID()
		target.expiresAt = time.Now().Add(time.Minute)
		target.access = onboardingAccess{user: "fixture", password: "volatile-ssh-fixture"}
	}
	return d, r
}

func inspectionReply(node string) []byte {
	body, _ := json.Marshal(map[string]any{"schemaVersion": 1, "action": "inspect", "state": "inspected", "effectsApplied": false, "executable": false,
		"identity":     map[string]any{"nodeId": node, "principal": node, "uid": 1000, "home": "/home/fixture"},
		"observations": map[string]any{"cuda": map[string]any{"path": "/usr/local/cuda-13.0/bin/nvcc", "version": "13.0.88"}, "mpi": map[string]any{"packages": []any{map[string]any{"name": "libopenmpi-dev", "status": "missing"}}}}, "errors": []any{}})
	return body
}

func TestDiagnosticInspectionUsesVolatilePinnedAccessAndReturnsObservedFacts(t *testing.T) {
	d, r := inspectionFixture(t, "controller")
	calls := 0
	d.m.onboarding.dial = func(ctx context.Context, c onboardingCandidate, a onboardingAccess) (*onboardingSSH, error) {
		calls++
		if !c.HostKeyTrusted || a.password != "volatile-ssh-fixture" {
			t.Fatal("existing access owner bypassed")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 35*time.Second {
			t.Fatal("unbounded native read")
		}
		return &onboardingSSH{testRun: func(ctx context.Context, command string, input io.Reader) ([]byte, error) {
			if command != "/usr/bin/python3 -I -c "+onboardingQuote(diagnosticInspectPython) {
				t.Fatal("not the fixed inspection program")
			}
			var request map[string]string
			if json.NewDecoder(input).Decode(&request) != nil || len(request) != 4 || request["action"] != "inspect" {
				t.Fatal("unexpected native request")
			}
			return inspectionReply(request["nodeId"]), nil
		}}, nil
	}
	result, err := d.inspectParticipants(context.Background(), r)
	if err != nil || calls != 2 || result.CanProvision || result.ObservedAt == 0 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
	for _, target := range result.Targets {
		if target.State != "inspected" || !bytes.Contains(target.Facts, []byte("/usr/local/cuda-13.0/bin/nvcc")) {
			t.Fatal("observed toolkit lost")
		}
	}
	public, _ := json.Marshal(result)
	if bytes.Contains(public, []byte("volatile-ssh-fixture")) || len(d.operations) != 0 || d.reservation != nil {
		t.Fatal("inspection leaked access or created execution state")
	}
}

func TestDiagnosticInspectionRejectsMissingDeniedChangedAndMismatchedAccess(t *testing.T) {
	for _, failure := range []string{"missing", "denied", "fingerprint", "identity", "generation"} {
		t.Run(failure, func(t *testing.T) {
			d, r := inspectionFixture(t, "controller")
			for _, target := range d.m.onboarding.targets {
				if failure == "missing" {
					target.candidate.AccessAvailable = false
				}
				if failure == "fingerprint" {
					target.candidate.HostKeyTrusted = false
				}
			}
			d.m.onboarding.dial = func(_ context.Context, c onboardingCandidate, _ onboardingAccess) (*onboardingSSH, error) {
				if failure == "denied" {
					return nil, errors.New("device account SSH authentication failed")
				}
				return &onboardingSSH{testRun: func(_ context.Context, _ string, input io.Reader) ([]byte, error) {
					var request map[string]string
					_ = json.NewDecoder(input).Decode(&request)
					if failure == "identity" {
						return inspectionReply("other-node"), nil
					}
					if failure == "generation" {
						d.m.onboarding.targets[c.CandidateID].accessGeneration = newOpID()
					}
					return inspectionReply(request["nodeId"]), nil
				}}, nil
			}
			result, err := d.inspectParticipants(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range result.Targets {
				if target.State == "inspected" || len(target.Facts) != 0 || target.Reason == "" {
					t.Fatalf("failure was hidden: %+v", target)
				}
			}
		})
	}
}

func TestDiagnosticInspectionLocalOwnerNeedsNoSSHToSelfAndCancelledReadDoesNotDial(t *testing.T) {
	d, r := inspectionFixture(t, "node-a")
	inventory, err := d.inspectionTargets(r.diagnosticSetupRequest)
	if err != nil || !inventory.Targets[0].Local || inventory.Targets[0].Candidate != nil || inventory.Targets[0].State != "not-inspected" {
		t.Fatalf("local owner manufactured access: %+v %v", inventory, err)
	}
	d.m.onboarding.dial = func(context.Context, onboardingCandidate, onboardingAccess) (*onboardingSSH, error) {
		t.Fatal("cancelled inspection dialled")
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.inspectParticipants(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	row := diagnosticInspectionTarget{NodeID: "node-a", Principal: "node-a"}
	applyDiagnosticInspection(&row, []byte(strings.Replace(string(inspectionReply("node-a")), `"executable":false`, `"executable":true`, 1)), nil)
	if row.State != "blocked" || len(row.Facts) != 0 {
		t.Fatal("inspection claimed execution permission")
	}
}
