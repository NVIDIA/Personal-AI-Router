// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"nvpair-shared/cableprobe"
)

func TestFabricInspectionReviewShowsFixedFailurePerTarget(t *testing.T) {
	for _, mode := range []string{"typed-error", "bound-result", "untrusted-code", "configuration-blocked"} {
		t.Run(mode, func(t *testing.T) {
			s, _, controls := fabricRefusalFixture(t)
			want := "network-manager-device-unavailable"
			if mode == "untrusted-code" {
				want = "native-inspect-failed"
			} else if mode == "configuration-blocked" {
				want = "configuration-blocked"
			}
			s.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
				if request.Method != "inspect" || request.SelectedPortPauseApproved {
					t.Error("inspection acquired mutation permission")
				}
				result := fabricWorkerResult{Protocol: fabricWorkerProtocol, Method: "inspect", OperationID: request.OperationID,
					NodeID: request.Target.NodeID, Principal: request.Target.Principal}
				if mode == "configuration-blocked" {
					result.Facts = fabricNativeFacts{Digest: strings.Repeat("a", 64), Blockers: []string{"private-detail-do-not-export"}}
					return result, nil
				}
				if mode == "bound-result" {
					result.FailureCode = want
					return result, nil
				}
				code := want
				if mode == "untrusted-code" {
					code = "private-detail-do-not-export"
				}
				return result, &fabricInspectError{Code: code, cause: errors.New("private-detail-do-not-export")}
			}
			review, err := s.review(context.Background(), cableProductReviewRequest{ReviewRequest: cableTestSelection()}, true)
			if err != nil || review.Executable || len(review.Blockers) != 2 || review.EffectsApplied || controls.Load() != 0 || len(s.runs) != 0 {
				t.Fatalf("inspection failure did not remain read-only and blocked: %+v %v", review, err)
			}
			for i, blocker := range review.Blockers {
				if !strings.Contains(blocker, review.Targets[i].NodeID) || !strings.Contains(blocker, "("+want+")") || strings.Contains(blocker, "private-detail") {
					t.Fatalf("missing target/fixed code or leaked private cause: %s", blocker)
				}
			}
		})
	}
}

func TestFabricApplyConfigurationBlockerKeepsTargetAndHidesPrivateDetail(t *testing.T) {
	s, run := fabricServiceFixture(t)
	s.control = func(context.Context, fabricTarget, fabricControlRequest) (fabricControlResult, error) {
		return fabricControlResult{}, nil
	}
	s.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if request.Method != "inspect" {
			t.Fatalf("configuration blocker reached %s", request.Method)
		}
		return fabricWorkerResult{Protocol: fabricWorkerProtocol, Method: request.Method, OperationID: request.OperationID,
			NodeID: request.Target.NodeID, Principal: request.Target.Principal,
			Facts: fabricNativeFacts{Digest: strings.Repeat("a", 64), Blockers: []string{"private-detail-do-not-export"}}}, nil
	}

	s.execute(context.Background(), run)
	wantNode := run.Public.Targets[0].NodeID
	if run.Public.State != "failed" || !run.Public.CleanupConfirmed || run.Public.EffectsApplied || run.Public.EffectsUnconfirmed ||
		run.Public.Failure == nil || run.Public.Failure.NodeID != wantNode || run.Public.Failure.Phase != "inspect" || run.Public.Failure.Code != "configuration-blocked" || !validFabricRecord(*run) {
		t.Fatalf("configuration blocker lost exact public attribution: %+v", run.Public)
	}
	if !strings.Contains(run.Public.Message, wantNode) || strings.Contains(run.Public.Message, "private-detail") {
		t.Fatalf("configuration blocker leaked detail or lost its node: %q", run.Public.Message)
	}
}

func generatedReviewDefault(iface fabricInterface) fabricGeneratedDefault {
	on := true
	return fabricGeneratedDefault{SchemaVersion: 1, InterfaceName: iface.Name, Index: iface.Index,
		Owner: ":1.2", BusID: strings.Repeat("a", 32), DevicePath: "/org/freedesktop/NetworkManager/Devices/2",
		PermanentMAC: iface.MAC, OriginalAutoconnect: &on, UUID: "11111111-2222-3333-4444-555555555555",
		Name: "Generated default", SettingsPath: "/org/freedesktop/NetworkManager/Settings/2",
		ProfileDigest: strings.Repeat("b", 64), ActivePath: "/org/freedesktop/NetworkManager/ActiveConnection/4"}
}

func TestFabricGeneratedRebindRequiresExactStableProfileAndApproval(t *testing.T) {
	iface := fabricNativeTestInterface()
	reviewed := generatedReviewDefault(iface)
	current := reviewed
	current.ActivePath = "/org/freedesktop/NetworkManager/ActiveConnection/99"
	facts := func(value *fabricGeneratedDefault) fabricNativeFacts {
		out := fabricNativeFacts{Digest: strings.Repeat("a", 64), Blockers: []string{}}
		if value != nil {
			out.GeneratedDefaults = []fabricGeneratedDefault{*value}
		}
		return out
	}
	target := func() fabricTarget {
		copy := iface
		copy.GeneratedDefault = &reviewed
		return fabricTarget{Interfaces: []fabricInterface{copy}}
	}

	got := target()
	changed, err := rebindFabricGeneratedDefaults(&got, facts(&current), true)
	if err != nil || !changed || got.Interfaces[0].GeneratedDefault.ActivePath != current.ActivePath {
		t.Fatalf("stable generated activation was not rebound: changed=%v err=%v", changed, err)
	}
	withoutApproval := target()
	if _, err := rebindFabricGeneratedDefaults(&withoutApproval, facts(&current), false); err == nil {
		t.Fatal("generated activation rebound without selected-port approval")
	}

	mutations := map[string]func(*fabricGeneratedDefault){
		"interface name":       func(d *fabricGeneratedDefault) { d.InterfaceName += "-other" },
		"interface index":      func(d *fabricGeneratedDefault) { d.Index++ },
		"permanent MAC":        func(d *fabricGeneratedDefault) { d.PermanentMAC = "02:00:00:00:00:99" },
		"device path":          func(d *fabricGeneratedDefault) { d.DevicePath = fabricNMRoot + "/Devices/99" },
		"settings path":        func(d *fabricGeneratedDefault) { d.SettingsPath = fabricNMRoot + "/Settings/99" },
		"profile UUID":         func(d *fabricGeneratedDefault) { d.UUID = "00000000-0000-4000-8000-000000000099" },
		"profile digest":       func(d *fabricGeneratedDefault) { d.ProfileDigest = strings.Repeat("c", 64) },
		"daemon owner":         func(d *fabricGeneratedDefault) { d.Owner = ":1.99" },
		"system bus":           func(d *fabricGeneratedDefault) { d.BusID = strings.Repeat("d", 32) },
		"profile name":         func(d *fabricGeneratedDefault) { d.Name += " replaced" },
		"original autoconnect": func(d *fabricGeneratedDefault) { value := !*d.OriginalAutoconnect; d.OriginalAutoconnect = &value },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := current
			mutate(&changed)
			candidate := target()
			if _, err := rebindFabricGeneratedDefaults(&candidate, facts(&changed), true); err == nil {
				t.Fatal("stable generated-profile mutation was admitted")
			}
		})
	}
	disappeared := target()
	if _, err := rebindFabricGeneratedDefaults(&disappeared, facts(nil), true); err == nil {
		t.Fatal("reviewed generated profile disappearance was admitted")
	}
}

func TestFabricLinkLocalReviewDoesNotInventNativePermission(t *testing.T) {
	facts := fabricServiceInventory("node-a")
	facts.Interfaces[0].Addresses = []string{"fe80::1/64"}
	forged := generatedReviewDefault(facts.Interfaces[0].fabricInterface)
	facts.Interfaces[0].GeneratedDefault = &forged
	target, err := fabricSelectedTarget(facts, cableprobe.PortRef{NodeID: "node-a", SwitchID: "switch-node-a", PortName: "p0"})
	if err != nil || target.Interfaces[0].GeneratedDefault != nil || !reflect.DeepEqual(target.Interfaces[0].Addresses, []string{"fe80::1/64"}) {
		t.Fatal("node inventory erased link-local facts or minted native permission")
	}
	for _, invalid := range [][]string{nil, {"192.0.2.1/30"}, {"fd00::1/64"}, {"fe80::1/128"}, {"fe80::1/64", "fe80::1/64"}, {"fe80::1"}} {
		if fabricLinkLocalBaseline(invalid) {
			t.Fatalf("unsupported address baseline accepted: %v", invalid)
		}
	}
}

func TestFabricNativeAddressMatchAdmitsOnlyConsentedLinkLocalChurn(t *testing.T) {
	target := fabricNativeTestInterface()
	generated := generatedReviewDefault(target)
	target.GeneratedDefault = &generated
	target.Addresses = []string{}
	actual := target
	actual.Addresses = []string{"fe80::1/64"}
	if !fabricNativeAddressesMatch(target, actual, false) {
		t.Fatal("consented generated-default link-local churn was rejected")
	}
	for _, addresses := range [][]string{{"192.0.2.1/24"}, {"fd00::1/64"}, {"fe80::1/128"}} {
		actual.Addresses = addresses
		if fabricNativeAddressesMatch(target, actual, false) {
			t.Fatalf("non-link-local address churn was admitted: %v", addresses)
		}
	}
	target.GeneratedDefault = nil
	actual.Addresses = []string{"fe80::1/64"}
	if fabricNativeAddressesMatch(target, actual, false) {
		t.Fatal("link-local churn was admitted without a generated-default binding")
	}
}

func TestFabricGeneratedInspectionIsExplicitAndApprovalIsSeparate(t *testing.T) {
	for _, mode := range []string{"missing-consent", "approved"} {
		t.Run(mode, func(t *testing.T) {
			s, _, controls := fabricRefusalFixture(t)
			inventory := s.inventory
			s.inventory = func(ctx context.Context, node string) (fabricInventory, error) {
				facts, err := inventory(ctx, node)
				facts.Interfaces[0].Addresses = []string{"fe80::1/64"}
				return facts, err
			}
			var inspections atomic.Int32
			s.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
				if request.Method != "inspect" || request.SelectedPortPauseApproved {
					t.Error("read-only inspection acquired mutation permission")
				}
				inspections.Add(1)
				return fabricWorkerResult{Protocol: fabricWorkerProtocol, Method: "inspect", OperationID: request.OperationID,
					NodeID: request.Target.NodeID, Principal: request.Target.Principal,
					Facts: fabricNativeFacts{Digest: strings.Repeat("a", 64), Routes: []string{}, Blockers: []string{},
						GeneratedDefaults: []fabricGeneratedDefault{generatedReviewDefault(request.Target.Interfaces[0])}}}, nil
			}
			request := cableProductReviewRequest{ReviewRequest: cableTestSelection()}
			candidate, err := s.review(context.Background(), request)
			if err != nil || candidate.Executable || !candidate.InspectionRequired || !candidate.InspectionAvailable || inspections.Load() != 0 || controls.Load() != 0 {
				t.Fatalf("ordinary review elevated or bypassed native proof: %+v %v", candidate, err)
			}
			review, err := s.review(context.Background(), request, true)
			if err != nil || !review.Executable || review.InspectionRequired || inspections.Load() != 2 || controls.Load() != 0 || !fabricTargetsNeedPause(review.Targets) {
				t.Fatalf("explicit inspection failed to bind a mutation-free review: %+v %v", review, err)
			}
			approved := mode == "approved"
			op, err := s.approve(context.Background(), review.ReviewID, true, approved)
			if !approved {
				assertFabricNotStarted(t, op, review, err)
				if controls.Load() != 0 {
					t.Fatal("missing selected-port consent reached reservation")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				r := s.runs[review.ReviewID]
				<-r.done // Fixture refuses reservation, so no address action runs.
				if !r.SelectedPortPauseApproved || !validFabricRecord(*r) || !r.Public.CleanupConfirmed {
					t.Fatal("approved pause scope or refusal cleanup was lost")
				}
			}
		})
	}
}

func TestFabricGeneratedBindingRejectsIncompleteOrForeignInspection(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "duplicate", "foreign-interface", "unknown-policy", "bad-digest", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			targets, _ := fabricServiceTargets(t)
			target := targets[0]
			target.Interfaces[0].Addresses = []string{"fe80::1/64"}
			facts := fabricNativeFacts{Digest: strings.Repeat("a", 64), GeneratedDefaults: []fabricGeneratedDefault{generatedReviewDefault(target.Interfaces[0])}}
			switch mode {
			case "missing":
				facts.GeneratedDefaults = nil
			case "duplicate":
				facts.GeneratedDefaults = append(facts.GeneratedDefaults, facts.GeneratedDefaults[0])
			case "foreign-interface":
				facts.GeneratedDefaults[0].Index = 999
			case "unknown-policy":
				facts.GeneratedDefaults[0].OriginalAutoconnect = nil
			case "bad-digest":
				facts.Digest = ""
			case "blocked":
				facts.Blockers = []string{"foreign route"}
			}
			err := bindFabricGeneratedDefaults(&target, facts)
			if (err == nil) != (mode == "valid") {
				t.Fatalf("wrong inspection binding result: %v", err)
			}
			if err != nil && target.Interfaces[0].GeneratedDefault != nil {
				t.Fatal("failed inspection published partial pause authority")
			}
		})
	}
}

func TestFabricWorkerRequiresGeneratedPauseConsent(t *testing.T) {
	for _, mode := range []string{"missing-consent", "missing-binding", "old-worker", "approved", "legacy-rollback"} {
		t.Run(mode, func(t *testing.T) {
			request, inventory := fabricDiagnosticWorkerFixture()
			request.Method = "apply"
			request.Target.Interfaces[0].Addresses = []string{"fe80::1/64"}
			inventory.Interfaces[0].Addresses = []string{"fe80::1/64"}
			descriptor := generatedReviewDefault(request.Target.Interfaces[0])
			request.Target.Interfaces[0].GeneratedDefault = &descriptor
			request.SelectedPortPauseApproved = mode != "missing-consent"
			if mode == "missing-binding" {
				request.Target.Interfaces[0].GeneratedDefault = nil
			}
			if mode == "old-worker" {
				request.Protocol = "pair-fabric-address/2"
			}
			if mode == "legacy-rollback" {
				request.Method = "rollback"
				request.Target.Interfaces[0].GeneratedDefault = nil
				request.Target.Interfaces[0].Addresses = []string{}
				request.SelectedPortPauseApproved = false
			}
			calls := []string{}
			var output bytes.Buffer
			err := runFabricAddressWorker(context.Background(), bytes.NewReader(fabricDiagnosticRequestBytes(t, request)), &output, request.Target.NodeID, fabricDiagnosticIO(request, inventory, &calls))
			valid := mode == "approved" || mode == "legacy-rollback"
			if (err == nil) != valid {
				t.Fatalf("worker pause boundary result: %v calls=%v", err, calls)
			}
			if !valid && len(calls) != 0 {
				t.Fatal("unapproved generated pause crossed worker admission")
			}
		})
	}
}

func TestFabricGeneratedRPCUsesSeparateInspectionAndPauseFlags(t *testing.T) {
	s, _, controls := fabricRefusalFixture(t)
	var out bytes.Buffer
	s.m.codec = NewCodec(&out)
	var inspections atomic.Int32
	s.worker = func(_ context.Context, _ cableLaunchPlan, request fabricWorkerRequest) (fabricWorkerResult, error) {
		if request.Method != "inspect" || request.SelectedPortPauseApproved {
			t.Error("inspection RPC granted a pause")
		}
		inspections.Add(1)
		return fabricWorkerResult{Protocol: fabricWorkerProtocol, Method: "inspect", OperationID: request.OperationID,
			NodeID: request.Target.NodeID, Principal: request.Target.Principal, Facts: fabricNativeFacts{Digest: strings.Repeat("a", 64),
				GeneratedDefaults: []fabricGeneratedDefault{generatedReviewDefault(request.Target.Interfaces[0])}}}, nil
	}
	selection := cableTestSelection()
	params, err := json.Marshal(map[string]any{"nodeIds": selection.NodeIDs, "ports": selection.Ports, "inspectSelectedProfiles": true})
	if err != nil {
		t.Fatal(err)
	}
	id := json.RawMessage("1")
	s.m.handleFabric(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:fabric-review", Params: params})
	var reply struct {
		Result fabricReview `json:"result"`
		Error  *RPCError    `json:"error"`
	}
	if json.Unmarshal(out.Bytes(), &reply) != nil || reply.Error != nil || !reply.Result.Executable || inspections.Load() != 2 || controls.Load() != 0 {
		t.Fatalf("explicit inspection flag lost at RPC: %s", out.Bytes())
	}
	out.Reset()
	params, err = json.Marshal(map[string]any{"reviewId": reply.Result.ReviewID, "administratorApproved": true, "selectedPortPauseApproved": true})
	if err != nil {
		t.Fatal(err)
	}
	s.m.handleFabric(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:fabric-approve", Params: params})
	s.mu.Lock()
	run := s.runs[reply.Result.ReviewID]
	s.mu.Unlock()
	if run == nil {
		t.Fatalf("pause flag did not reach approval: %s", out.Bytes())
	}
	<-run.done
	if !run.SelectedPortPauseApproved || !run.Public.CleanupConfirmed {
		t.Fatal("RPC lost recorded pause approval or bounded failure cleanup")
	}
}
