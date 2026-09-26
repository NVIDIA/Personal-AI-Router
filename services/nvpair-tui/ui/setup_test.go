// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func setupTestSelection() cableprobe.ReviewRequest {
	return cableprobe.ReviewRequest{
		NodeIDs: []string{"node-a", "node-b"},
		Ports: []cableprobe.PortRef{
			{NodeID: "node-a", SwitchID: "switch-a", PortName: "p0"},
			{NodeID: "node-b", SwitchID: "switch-b", PortName: "p0"},
		},
	}
}

func setupTestCableReview() cableprobe.Review {
	return cableprobe.Review{
		ReviewID: "cable-review-fixture", OwnerNodeID: "node-a", Available: true, RemainingMs: 25_000,
		Targets: []cableprobe.Target{
			{NodeID: "node-a", Principal: "principal-a", RawPrivilege: "approval-needed", Ports: []cableprobe.Port{{SwitchID: "switch-a", PortName: "p0"}}},
			{NodeID: "node-b", Principal: "principal-b", RawPrivilege: "approval-needed", Ports: []cableprobe.Port{{SwitchID: "switch-b", PortName: "p0"}}},
		},
	}
}

func setupTestCableRun() cableprobe.Run {
	review := setupTestCableReview()
	return cableprobe.Run{
		RunID: strings.Repeat("a", 32), ReviewID: review.ReviewID, OwnerNodeID: review.OwnerNodeID,
		Revision: 2, State: "failed", Targets: review.Targets, Result: "incomplete", Directness: "unverified",
		CleanupConfirmed: false, Message: "cleanup unconfirmed",
	}
}

func setupTestFabricReview() setupFabricReview {
	return setupFabricReview{
		SchemaVersion: 1, ReviewID: strings.Repeat("b", 32), OwnerNodeID: "node-a", RecipeID: "fixture-recipe",
		Persistence: "until-reboot", State: "ready", Executable: true, RemainingMs: 20_000,
		Targets: []setupFabricTarget{
			{NodeID: "node-a", Principal: "principal-a", SwitchID: "switch-a", PortName: "p0"},
			{NodeID: "node-b", Principal: "principal-b", SwitchID: "switch-b", PortName: "p0"},
		},
	}
}

func TestSetupViewKeepsServingAndProxyOwnershipSurfaces(t *testing.T) {
	titles := []string{}
	for _, view := range defaultViews(nil) {
		titles = append(titles, view.Title())
	}
	joined := strings.Join(titles, ",")
	for _, want := range []string{"Proxies", "Engines", "Serving", "Setup"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("default tabs %q lost %s", joined, want)
		}
	}
}

func TestSetupSecretsAreMaskedClearedAndPrivateCallbackIgnored(t *testing.T) {
	v := newSetupView(nil)
	v.SetSize(120, 30)
	v.candidates = []setupCandidate{{
		CandidateID:    "candidate-a",
		Label:          "A",
		Address:        "192.0.2.10",
		Port:           22,
		BootstrapState: "ssh-ready",
	}}
	v.selected["candidate-a"] = true
	v.renderCandidates()
	v.beginAccess("cable")
	v.input.SetValue("operator")
	v.submitInput()
	v.input.SetValue("password")
	v.submitInput()
	v.input.SetValue("account-secret")
	v.submitInput()
	if v.input.EchoMode != textinput.EchoPassword || strings.Contains(v.View(), "account-secret") {
		t.Fatal("credential input was not masked")
	}
	v.Update(NotificationMsg{Msg: &rpc.Message{Method: "engine:onboarding-cluster-call", Params: json.RawMessage(`{"password":"private-callback-secret"}`)}})
	if strings.Contains(v.View(), "private-callback-secret") {
		t.Fatal("private onboarding callback reached the Setup view")
	}
	v.input.SetValue("admin-secret")
	cmd := v.submitInput()
	if cmd == nil || v.CapturingInput() || v.input.Value() != "" || len(v.accessDraft.Password) != 0 || len(v.accessDraft.ElevationPassword) != 0 {
		t.Fatalf("submitted secrets remained in view state: cmd=%v input=%v", cmd != nil, v.CapturingInput())
	}
}

func TestSetupAccessCommandZerosMutableSecrets(t *testing.T) {
	password := transientSecret([]byte("account-secret"))
	elevation := transientSecret([]byte("admin-secret"))
	request, result := runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return setupAccessCmd(client, setupAccessRequest{
			Purpose: "cable", CandidateIDs: []string{"candidate-a"}, Username: "operator", Auth: "password",
			Password: password, ElevationPassword: elevation, StartupLifetime: "session",
		})
	}, map[string]any{"candidates": []any{}})
	if request.Method != "engine:onboarding-access" || !strings.Contains(string(request.Params), "account-secret") || !strings.Contains(string(request.Params), "admin-secret") {
		t.Fatalf("access request = %s %s", request.Method, request.Params)
	}
	if msg, ok := result.(setupAccessMsg); !ok || msg.err != nil {
		t.Fatalf("access result = %#v", result)
	}
	for _, secret := range []transientSecret{password, elevation} {
		for _, value := range secret {
			if value != 0 {
				t.Fatal("mutable access secret was not zeroed after RPC")
			}
		}
	}
}

func TestSetupSelectionAndReviewsStayExactlyBound(t *testing.T) {
	selection, err := parseCableSelection("node-a/switch-a/p0,node-b/switch-b/p0")
	if err != nil || !reflect.DeepEqual(selection, setupTestSelection()) {
		t.Fatalf("selection = %+v, err=%v", selection, err)
	}
	for _, value := range []string{"node-a/switch-a/p0", "node-a/switch-a/p0,node-a/switch-a/p0", "node-a/switch-a/p0,node-b"} {
		if _, err := parseCableSelection(value); err == nil {
			t.Fatalf("invalid cable selection %q accepted", value)
		}
	}
	review := setupTestCableReview()
	if err := validateCableReview(review, selection); err != nil {
		t.Fatalf("valid cable review rejected: %v", err)
	}
	review.Targets[1].Ports[0].PortName = "p1"
	if err := validateCableReview(review, selection); err == nil {
		t.Fatal("cable review drift was accepted")
	}
	fabric := setupTestFabricReview()
	if err := validateFabricReview(fabric, selection); err != nil {
		t.Fatalf("valid fabric review rejected: %v", err)
	}
	fabric.Targets = fabric.Targets[:1]
	if err := validateFabricReview(fabric, selection); err == nil {
		t.Fatal("executable fabric review omitted a selected participant")
	}
}

func TestSetupBackgroundPollCannotReleasePendingEffectOrUncertainty(t *testing.T) {
	v := newSetupView(nil)
	v.pending = "setup approval"
	v.uncertainSetupID = "new-review"
	v.Update(setupOperationMsg{action: "setup status", operation: &setupOperation{OperationID: "old-operation", ReviewID: "old-review", StartedAt: 1}})
	if v.pending != "setup approval" || v.uncertainSetupID != "new-review" || v.operation != nil {
		t.Fatalf("stale setup poll released pending effect: pending=%q uncertain=%q operation=%v", v.pending, v.uncertainSetupID, v.operation != nil)
	}
	v.pending = "cable start"
	v.uncertainCableID = "new-cable-review"
	v.Update(setupCableRunMsg{action: "cable status", run: &cableprobe.Run{RunID: "old-run", ReviewID: "old-review", StartedAt: 1}})
	if v.pending != "cable start" || v.uncertainCableID != "new-cable-review" {
		t.Fatalf("stale cable poll released pending effect: pending=%q uncertain=%q", v.pending, v.uncertainCableID)
	}
	v.pending = "fabric approval"
	v.Update(setupFabricOperationMsg{action: "fabric status", operation: &setupFabricOperation{OperationID: "fabric-op"}})
	if v.pending != "fabric approval" {
		t.Fatalf("fabric poll released pending effect: %q", v.pending)
	}
}

func TestSetupViewRendersExactHoldsAndBindings(t *testing.T) {
	run := setupTestCableRun()
	run.Edges = []cableprobe.Edge{{
		Left:  cableprobe.PortRef{NodeID: "node-a", SwitchID: "switch-a", PortName: "p0"},
		Right: cableprobe.PortRef{NodeID: "node-b", SwitchID: "switch-b", PortName: "p0"}, AgeMs: 42, Fresh: true,
	}}
	run.FreshnessRemainingMs = 1900
	run.CleanupRecovery = &cableprobe.CleanupRecovery{AttemptID: "attempt-exact", ReviewID: "cleanup-review-exact", State: "verifying", Code: "verifying"}
	v := newSetupView(nil)
	v.SetSize(160, 45)
	v.loadedCandidates = true
	v.loadedRetained = true
	v.retained = cableprobe.RetainedRuns{OwnerNodeID: "node-a", Held: true, Runs: []cableprobe.RetainedRun{{RunID: run.RunID, ReviewID: run.ReviewID, State: run.State}}}
	v.cableRun = &run
	fabric := setupFabricOperation{
		SchemaVersion: 1, OperationID: strings.Repeat("b", 32), ReviewID: strings.Repeat("b", 32), RecipeID: "fixture-recipe", State: "active",
		Targets:      setupTestFabricReview().Targets,
		CandidateIPs: []setupFabricCandidateIP{{NodeID: "node-a", PeerNodeID: "node-b", PeerPrincipal: "principal-b", Address: "172.31.0.1", PeerAddress: "172.31.0.2", InterfaceName: "enp1s0", InterfaceIndex: 7, SwitchID: "switch-a", PortName: "p0", RDMADevice: "mlx5_0", RDMAPort: 1, GIDIndex: 3, GIDType: "RoCE v2"}},
	}
	v.fabricOperation = &fabric
	view := v.View()
	for _, want := range []string{run.RunID, "Cable hold=yes", "switch-a/p0 <-> node-b/switch-b/p0", "lease=1900ms", "attempt-exact", fabric.OperationID, "principal-b", "mlx5_0:1", "gid=3/RoCE v2"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing exact binding %q:\n%s", want, view)
		}
	}
}

func TestSetupFabricRendersRoutedRingProofsWithoutRDMA(t *testing.T) {
	v := newSetupView(nil)
	v.fabricOperation = &setupFabricOperation{
		SchemaVersion: 1, OperationID: strings.Repeat("c", 32), ReviewID: strings.Repeat("c", 32), RecipeID: "spark-three-node-ring-routed-v2", State: "active",
		Targets: []setupFabricTarget{{NodeID: "node-a", Principal: "principal-a", AdvertisedAddress: "10.253.0.0", Interfaces: []setupFabricInterface{
			{Name: "enp1s0f0np0", Index: 7, Address: "10.253.0.0/31", Routes: []setupFabricRoute{{Destination: "10.253.0.4/32", Gateway: "10.253.0.1"}}},
		}}},
		CandidateIPs: []setupFabricCandidateIP{{NodeID: "node-a", PeerNodeID: "node-b", PeerPrincipal: "principal-b", Address: "10.253.0.0", PeerAddress: "10.253.0.4", InterfaceName: "enp1s0f0np0", InterfaceIndex: 7, SwitchID: "switch-a", PortName: "p0", Gateway: "10.253.0.1"}},
	}
	var b strings.Builder
	v.renderFabric(&b)
	view := b.String()
	for _, want := range []string{"advertised=10.253.0.0", "route=10.253.0.4/32 via 10.253.0.1", "routed node-a(10.253.0.0) -> node-b(10.253.0.4) via gateway 10.253.0.1 on enp1s0f0np0#7"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing routed ring detail %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "rdma=:0") || strings.Contains(view, "gid=0/") {
		t.Fatalf("routed proof rendered as an RDMA lane:\n%s", view)
	}
}

func TestSetupRPCJourneyUsesOnlyPublicOwnerMethods(t *testing.T) {
	candidate := setupCandidate{CandidateID: "candidate-a", Label: "A", Address: "192.0.2.10", Port: 22}
	request, _ := runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCandidatesCmd(client) }, map[string]any{"candidates": []setupCandidate{candidate}, "artifacts": []setupArtifact{}})
	assertSetupMethod(t, request, "engine:onboarding-candidates")
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupAddTargetCmd(client, candidate.Address, 22) }, candidate)
	assertSetupMethod(t, request, "engine:onboarding-add-target")

	review := setupReview{ReviewID: strings.Repeat("c", 32), ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), ControllerNodeID: "controller", Targets: []setupReviewTarget{{CandidateID: candidate.CandidateID}}, CanApprove: true}
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return setupInspectCmd(client, []string{candidate.CandidateID}, "artifact-a", nil)
	}, review)
	assertSetupMethod(t, request, "engine:onboarding-inspect")
	operation := setupOperation{OperationID: strings.Repeat("d", 32), ReviewID: review.ReviewID, Revision: 1, State: "running", Targets: []setupTargetState{{CandidateID: candidate.CandidateID, Stage: "installing"}}}
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupApproveCmd(client, review) }, operation)
	assertSetupMethod(t, request, "engine:onboarding-approve")
	for _, method := range []string{"engine:onboarding-status", "engine:onboarding-cancel", "engine:onboarding-retry"} {
		request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd {
			return setupOperationCmd(client, method, "fixture", operation.OperationID)
		}, operation)
		assertSetupMethod(t, request, method)
	}

	selection := setupTestSelection()
	cableReview := setupTestCableReview()
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCableReviewCmd(client, selection, nil) }, cableReview)
	assertSetupMethod(t, request, "engine:cable-review")
	cableRun := setupTestCableRun()
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCableStartCmd(client, cableReview) }, cableRun)
	assertSetupMethod(t, request, "engine:cable-start")
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCableStatusCmd(client, cableRun.RunID) }, cableRun)
	assertSetupMethod(t, request, "engine:cable-status")
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCableCancelCmd(client, cableRun.RunID, false) }, cableRun)
	assertSetupMethod(t, request, "engine:cable-cancel")
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCableRetainedCmd(client) }, cableprobe.RetainedRuns{OwnerNodeID: "node-a", Held: true})
	assertSetupMethod(t, request, "engine:cable-retained-runs")

	cleanup := cableprobe.CleanupReview{ReviewID: "cable-cleanup-review-fixture", RunID: cableRun.RunID, Available: true, RemainingMs: 20_000}
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCleanupReviewCmd(client, cableRun.RunID, nil) }, cleanup)
	assertSetupMethod(t, request, "engine:cable-cleanup-review")
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCleanupVerifyCmd(client, cleanup) }, cableprobe.CleanupVerifyResult{Disposition: "accepted", ReviewID: cleanup.ReviewID, Run: cableRun})
	assertSetupMethod(t, request, "engine:cable-cleanup-verify")
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupCableCancelCmd(client, cableRun.RunID, true) }, cableRun)
	assertSetupMethod(t, request, "engine:cable-cleanup-cancel")

	fabricReview := setupTestFabricReview()
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupFabricReviewCmd(client, selection, nil) }, fabricReview)
	assertSetupMethod(t, request, "engine:fabric-review")
	fabricOperation := setupFabricOperation{SchemaVersion: 1, OperationID: fabricReview.ReviewID, ReviewID: fabricReview.ReviewID, State: "applying"}
	request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd { return setupFabricApproveCmd(client, fabricReview) }, fabricOperation)
	assertSetupMethod(t, request, "engine:fabric-approve")
	for _, entry := range []struct{ method, action string }{{"engine:fabric-status", "status"}, {"engine:fabric-cancel", "cancel"}, {"engine:fabric-recover", "recover"}} {
		request, _ = runTUICommand(t, func(client *rpc.Client) tea.Cmd {
			return setupFabricOperationCmd(client, entry.method, entry.action, fabricOperation.OperationID)
		}, fabricOperation)
		assertSetupMethod(t, request, entry.method)
	}
}

func assertSetupMethod(t *testing.T, request *rpc.Message, want string) {
	t.Helper()
	if request.Method != want {
		t.Fatalf("method = %q, want %q", request.Method, want)
	}
	if strings.Contains(request.Method, "onboarding-cluster") {
		t.Fatalf("private callback method exposed: %s", request.Method)
	}
}
