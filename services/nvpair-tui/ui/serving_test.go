// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func servingBool(value bool) *bool { return &value }
func servingInt(value int) *int    { return &value }

func runTUICommand(t *testing.T, build func(*rpc.Client) tea.Cmd, result any) (*rpc.Message, tea.Msg) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	client := rpc.NewClient(clientConn, clientConn)
	server := rpc.NewCodec(serverConn, serverConn)
	ctx, cancel := context.WithCancel(context.Background())
	go client.Run(ctx)
	t.Cleanup(func() {
		cancel()
		clientConn.Close()
		serverConn.Close()
	})
	resultCh := make(chan tea.Msg, 1)
	go func() { resultCh <- build(client)() }()
	request, err := server.Read()
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if err := server.Write(&rpc.Message{JSONRPC: "2.0", ID: request.ID, Result: raw}); err != nil {
		t.Fatalf("write response: %v", err)
	}
	select {
	case msg := <-resultCh:
		return request, msg
	case <-time.After(2 * time.Second):
		t.Fatal("TUI command did not finish")
		return nil, nil
	}
}

func servingTestPlan() servingGroupPlan {
	return servingGroupPlan{
		Coordinator: "node-a",
		Runtime:     "0.29.0",
		Model:       "example/model",
		Topology:    servingGroupTopology{TensorParallel: 2, PipelineParallel: 1, DataParallel: 1},
		Members:     []servingGroupMember{{NodeID: "node-a"}, {NodeID: "node-b"}},
	}
}

func servingQwenTestPlan() servingGroupPlan {
	return servingGroupPlan{
		Coordinator: "node-a", Model: servingGroupQwenModel, Runtime: servingGroupQwenRuntime,
		Topology: servingGroupTopology{TensorParallel: 2, PipelineParallel: 1, DataParallel: 1},
		Members:  []servingGroupMember{{NodeID: "node-a"}, {NodeID: "node-b"}},
		Transport: &servingGroupTransport{
			Mode: "host-buffer-roce", OperationID: strings.Repeat("9", 32), QualificationSHA256: strings.Repeat("a", 64),
			NetGDRLevel: servingInt(0), NetGDRC2C: servingInt(0), NetGDRRead: servingInt(0),
			NetPlugin: "none", EnvPlugin: "none", GINPlugin: "none",
			SubnetAwareRouting: servingBool(false), SubnetPrefixLength: servingInt(0), MergeNICs: servingBool(true), SocketPayloadFallback: servingBool(false),
		},
	}
}

func servingTestRun() servingGroupRun {
	return servingGroupRun{
		RunID:      "facfa61ad839f1108ea0972f3f389b3c",
		Generation: 13,
		PlanDigest: strings.Repeat("a", 64),
		State:      "cleanup-required",
		Plan:       servingTestPlan(),
		Ranks: []servingGroupRank{
			{NodeID: "node-a", Attempted: true},
			{NodeID: "node-b", Attempted: true},
		},
		Failure: "all owned ranks require cleanup",
	}
}

func servingTestReview() servingGroupReview {
	return servingGroupReview{
		ReviewID:          "abcfa61ad839f1108ea0972f3f389b3c",
		PlanDigest:        strings.Repeat("a", 64),
		Plan:              servingTestPlan(),
		ExpiresAt:         time.Now().Add(time.Minute).UnixMilli(),
		ActivationEnabled: servingBool(true),
	}
}

func servingTestCheck(review servingGroupReview) servingGroupCheck {
	participants := make([]servingGroupParticipant, len(review.Plan.Members))
	for i, member := range review.Plan.Members {
		participants[i] = servingGroupParticipant{
			Protocol: "pair-vllm-group/1", Action: "capability", NodeID: member.NodeID,
			Controller: "controller-principal", RunID: review.ReviewID, Generation: 1,
			PlanDigest: review.PlanDigest, Rank: i, State: "capable", Code: "ok",
			ActivationEnabled: servingBool(true), EffectsApplied: servingBool(false), CleanupConfirmed: servingBool(false),
		}
	}
	return servingGroupCheck{ReviewID: review.ReviewID, ActivationEnabled: servingBool(true), Participants: participants}
}

func TestServingGroupViewShowsHeldStatusAndCleanupActions(t *testing.T) {
	run := servingTestRun()
	v := newServingGroupView(nil)
	v.SetSize(100, 20)
	v.Update(servingGroupMsg{status: servingGroupStatus{
		ActivationEnabled: servingBool(true), Reserved: servingBool(true),
		Reason: "fresh native admission required", Run: &run,
	}})
	got := v.View()
	for _, want := range []string{"generation 13", "cleanup-required", "TP 2", "2 members", "fresh native admission required"} {
		if !strings.Contains(got, want) {
			t.Fatalf("view missing %q:\n%s", want, got)
		}
	}
	if got := len(v.Help()); got != 4 {
		t.Fatalf("held action count = %d, want refresh + stop/reconcile/cleanup", got)
	}
}

func TestServingGroupQwenTransportIsExactAndVisible(t *testing.T) {
	plan := servingQwenTestPlan()
	if err := validateServingGroupPlan(plan); err != nil {
		t.Fatal(err)
	}
	v := newServingGroupView(nil)
	v.SetSize(140, 30)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	v.review = &servingGroupReview{ReviewID: strings.Repeat("b", 32), PlanDigest: strings.Repeat("c", 64), Plan: plan, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), ActivationEnabled: servingBool(true)}
	want := "host-buffer RoCE · merged NICs on · socket payload fallback off"
	if got := v.View(); !strings.Contains(got, want) {
		t.Fatalf("visible review omitted %q:\n%s", want, got)
	}
	for name, change := range map[string]func(*servingGroupTransport){
		"missing subnet mode": func(value *servingGroupTransport) { value.SubnetAwareRouting = nil },
		"subnet routing":      func(value *servingGroupTransport) { value.SubnetAwareRouting = servingBool(true) },
		"subnet prefix":       func(value *servingGroupTransport) { value.SubnetPrefixLength = servingInt(31) },
		"unmerged NICs":       func(value *servingGroupTransport) { value.MergeNICs = servingBool(false) },
		"socket fallback":     func(value *servingGroupTransport) { value.SocketPayloadFallback = servingBool(true) },
	} {
		t.Run(name, func(t *testing.T) {
			plan := servingQwenTestPlan()
			change(plan.Transport)
			if err := validateServingGroupPlan(plan); err == nil {
				t.Fatal("changed Qwen transport was accepted")
			}
		})
	}
	ordinary := servingTestPlan()
	ordinary.Transport = servingQwenTestPlan().Transport
	if err := validateServingGroupPlan(ordinary); err == nil {
		t.Fatal("ordinary plan claimed the fixed Qwen transport")
	}
}

func TestServingGroupSocketBindingsAreExactAndVisible(t *testing.T) {
	socket := func(mode string) *servingGroupSocket {
		return &servingGroupSocket{Mode: mode, OperationID: strings.Repeat("7", 32), QualificationSHA256: strings.Repeat("8", 64)}
	}
	threeNode := func(tp, pp int) servingGroupPlan {
		plan := servingTestPlan()
		plan.Members = append(plan.Members, servingGroupMember{NodeID: "node-c"})
		plan.Topology = servingGroupTopology{TensorParallel: tp, PipelineParallel: pp, DataParallel: 1}
		return plan
	}
	direct := servingTestPlan()
	direct.DirectSocket = socket("qualified-direct-socket")
	pipelineRing, tensorRing := threeNode(1, 3), threeNode(3, 1)
	pipelineRing.RingSocket, tensorRing.RingSocket = socket("qualified-ring-socket"), socket("qualified-ring-socket")
	ringLabel := "NCCL Socket on the qualified routed ring · control on management · RDMA off"
	for _, tc := range []struct {
		plan servingGroupPlan
		want string
	}{
		{direct, "NCCL Socket on the qualified direct fabric lane · control on management · RDMA off"},
		{pipelineRing, ringLabel},
		{tensorRing, ringLabel},
	} {
		if err := validateServingGroupPlan(tc.plan); err != nil {
			t.Fatal(err)
		}
		v := newServingGroupView(nil)
		v.SetSize(140, 30)
		v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
		v.review = &servingGroupReview{ReviewID: strings.Repeat("b", 32), PlanDigest: strings.Repeat("c", 64), Plan: tc.plan, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), ActivationEnabled: servingBool(true)}
		if got := v.View(); !strings.Contains(got, tc.want) {
			t.Fatalf("visible review omitted %q:\n%s", tc.want, got)
		}
	}
	for name, plan := range map[string]func() servingGroupPlan{
		"ring on two nodes": func() servingGroupPlan {
			p := servingTestPlan()
			p.RingSocket = socket("qualified-ring-socket")
			return p
		},
		"direct on three nodes": func() servingGroupPlan {
			p := threeNode(3, 1)
			p.DirectSocket = socket("qualified-direct-socket")
			return p
		},
		"direct pipeline": func() servingGroupPlan {
			p := servingTestPlan()
			p.Topology = servingGroupTopology{TensorParallel: 1, PipelineParallel: 2, DataParallel: 1}
			p.DirectSocket = socket("qualified-direct-socket")
			return p
		},
		"both bindings": func() servingGroupPlan {
			p := threeNode(1, 3)
			p.RingSocket, p.DirectSocket = socket("qualified-ring-socket"), socket("qualified-direct-socket")
			return p
		},
		"direct mode on the ring": func() servingGroupPlan {
			p := threeNode(1, 3)
			p.RingSocket = socket("qualified-direct-socket")
			return p
		},
		"missing operation": func() servingGroupPlan {
			p := threeNode(1, 3)
			p.RingSocket = socket("qualified-ring-socket")
			p.RingSocket.OperationID = ""
			return p
		},
		"qwen profile": func() servingGroupPlan {
			p := servingQwenTestPlan()
			p.DirectSocket = socket("qualified-direct-socket")
			return p
		},
	} {
		if err := validateServingGroupPlan(plan()); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestServingGroupNeverAcceptsAThreeSparkQwenPlan(t *testing.T) {
	for name, topology := range map[string]servingGroupTopology{
		"pipeline":      {TensorParallel: 1, PipelineParallel: 3, DataParallel: 1},
		"data parallel": {TensorParallel: 1, PipelineParallel: 1, DataParallel: 3},
		"tensor":        {TensorParallel: 2, PipelineParallel: 1, DataParallel: 1},
	} {
		t.Run(name, func(t *testing.T) {
			plan := servingQwenTestPlan()
			plan.Topology = topology
			plan.Members = append(plan.Members, servingGroupMember{NodeID: "node-c"})
			if err := validateServingGroupPlan(plan); err == nil {
				t.Fatal("a three-Spark Qwen3.8 plan gained Review authority")
			}
			ranks := make([]servingGroupRank, len(plan.Members))
			for i, member := range plan.Members {
				ranks[i] = servingGroupRank{NodeID: member.NodeID, Attempted: true, Started: true, CleanupConfirmed: true}
			}
			run := servingGroupRun{
				RunID: strings.Repeat("6", 32), Generation: 60, PlanDigest: strings.Repeat("a", 64),
				Plan: plan, State: "failed", Ranks: ranks, CleanupConfirmed: true,
			}
			if err := validateServingGroupRun(run); err == nil {
				t.Fatal("a three-Spark Qwen3.8 run was accepted as history")
			}
		})
	}
}

func TestServingGroupHistoricalTransportIsCleanupReadbackOnly(t *testing.T) {
	plan := servingQwenTestPlan()
	plan.Transport.SubnetAwareRouting = nil
	plan.Transport.SubnetPrefixLength = nil
	plan.Transport.MergeNICs = nil
	ranks := make([]servingGroupRank, len(plan.Members))
	for i, member := range plan.Members {
		ranks[i] = servingGroupRank{NodeID: member.NodeID, Attempted: true, Started: true, CleanupConfirmed: true}
	}
	run := servingGroupRun{
		RunID: strings.Repeat("5", 32), Generation: 55, PlanDigest: strings.Repeat("a", 64),
		Plan: plan, State: "failed", Ranks: ranks, CleanupConfirmed: true,
	}
	status := servingGroupStatus{ActivationEnabled: servingBool(false), Reserved: servingBool(false), Run: &run}
	if err := validateServingGroupStatus(status); err != nil {
		t.Fatalf("cleanup-confirmed generation 55 was hidden: %v", err)
	}
	if err := validateServingGroupPlan(plan); err == nil {
		t.Fatal("historical transport gained fresh Review authority")
	}
	active := run
	active.Ranks = append([]servingGroupRank(nil), run.Ranks...)
	active.State, active.CleanupConfirmed = "starting", false
	for i := range active.Ranks {
		active.Ranks[i].CleanupConfirmed = false
	}
	if err := validateServingGroupRun(active); err == nil {
		t.Fatal("historical transport gained active-run authority")
	}
	v := newServingGroupView(nil)
	v.SetSize(140, 30)
	v.Update(servingGroupMsg{status: status})
	if got := v.View(); !strings.Contains(got, "historical peer-subnet policy not recorded") || strings.Contains(got, "subnet-aware routing /31") {
		t.Fatalf("historical transport was misrepresented:\n%s", got)
	}
}

func TestServingGroupViewMissingJournalOffersReviewOnlyWhenActivationAvailable(t *testing.T) {
	v := newServingGroupView(nil)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	if got := v.View(); !strings.Contains(got, "No retained serving group") {
		t.Fatalf("inactive view = %q", got)
	}
	if got := len(v.Help()); got != 2 {
		t.Fatalf("inactive action count = %d, want refresh + review", got)
	}
}

func TestServingGroupDecodeFailsClosedWithoutAuthorityFields(t *testing.T) {
	if err := validateServingGroupStatus(servingGroupStatus{}); err == nil {
		t.Fatal("missing authority fields were treated as inactive")
	}
}

func TestServingGroupStartingRunDoesNotInventCleanup(t *testing.T) {
	run := servingTestRun()
	run.State, run.Failure = "starting", ""
	for i := range run.Ranks {
		run.Ranks[i] = servingGroupRank{NodeID: run.Plan.Members[i].NodeID}
	}
	status := servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(true), Run: &run}
	if err := validateServingGroupStatus(status); err != nil {
		t.Fatalf("valid unattempted starting run rejected: %v", err)
	}
	run.CleanupConfirmed = true
	if err := validateServingGroupRun(run); err == nil {
		t.Fatal("live run claimed terminal cleanup")
	}
}

func TestServingGroupReviewAndCheckStayBoundToSelection(t *testing.T) {
	review := servingTestReview()
	selection := servingGroupSelection{NodeIDs: []string{"node-a", "node-b"}, Model: "example/model", Parallelism: "tensor"}
	if err := validateServingGroupReview(review, selection); err != nil {
		t.Fatalf("valid review rejected: %v", err)
	}
	check := servingTestCheck(review)
	if err := validateServingGroupCheck(check, review); err != nil {
		t.Fatalf("valid check rejected: %v", err)
	}
	check.Participants[1].NodeID = "node-c"
	if err := validateServingGroupCheck(check, review); err == nil {
		t.Fatal("participant drift was accepted")
	}
	review.Plan.Members[1].NodeID = "node-c"
	if err := validateServingGroupReview(review, selection); err == nil {
		t.Fatal("review selection drift was accepted")
	}
}

func TestServingGroupNodeSelectionIsOrderedAndDistinct(t *testing.T) {
	got, err := parseServingGroupNodes("node-a, node-b,node-c")
	if err != nil || strings.Join(got, ",") != "node-a,node-b,node-c" {
		t.Fatalf("parsed nodes = %v, err=%v", got, err)
	}
	for _, input := range []string{"node-a", "node-a,node-a", "node a,node-b", "node-a,node-b,node-c,node-d"} {
		if _, err := parseServingGroupNodes(input); err == nil {
			t.Fatalf("invalid node selection %q accepted", input)
		}
	}
}

func TestServingGroupRequiresFreshCheckBeforeStart(t *testing.T) {
	v := newServingGroupView(nil)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	review := servingTestReview()
	v.Update(servingGroupActionMsg{action: "review", review: &review})
	if v.canStart() {
		t.Fatal("unchecked review could Start")
	}
	check := servingTestCheck(review)
	v.Update(servingGroupActionMsg{action: "check", check: &check})
	if !v.canStart() {
		t.Fatal("fresh admitted check did not enable Start")
	}
	review.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if v.canStart() {
		t.Fatal("expired review could Start")
	}
}

func TestServingGroupStartConsumesReviewBeforeSending(t *testing.T) {
	v := newServingGroupView(nil)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	review := servingTestReview()
	check := servingTestCheck(review)
	v.review, v.check = &review, &check
	if cmd := v.submitStart(nil); cmd == nil {
		t.Fatal("admitted Start did not dispatch")
	}
	if v.review != nil || v.check != nil || !v.uncertainStart || v.pending != "start" || v.CapturingInput() {
		t.Fatalf("Start was not consumed before send: review=%v check=%v uncertain=%v pending=%q input=%v", v.review != nil, v.check != nil, v.uncertainStart, v.pending, v.CapturingInput())
	}
}

func TestServingGroupStartPromptsForEveryReviewedParticipantAndMasksSecrets(t *testing.T) {
	v := newServingGroupView(nil)
	v.SetSize(120, 30)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	review := servingTestReview()
	check := servingTestCheck(review)
	v.review, v.check = &review, &check

	if cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}}); cmd == nil {
		t.Fatal("Start did not open administrator access input")
	}
	if v.inputMode != servingGroupInputElevation || v.input.EchoMode != textinput.EchoPassword || !reflect.DeepEqual(v.elevationNodes, []string{"node-a", "node-b"}) {
		t.Fatalf("Start elevation prompt = mode=%d echo=%d nodes=%v", v.inputMode, v.input.EchoMode, v.elevationNodes)
	}

	// Blank is an explicit passwordless-sudo choice for the first reviewed node.
	v.input.SetValue("")
	if cmd := v.submitInput(); cmd == nil || len(v.elevationDraft) != 1 || !v.elevationDraft[0].NonInteractive {
		t.Fatalf("blank administrator choice = cmd=%v draft=%+v", cmd != nil, v.elevationDraft)
	}
	v.input.SetValue("one-use-secret")
	if strings.Contains(v.View(), "one-use-secret") {
		t.Fatal("administrator secret was rendered")
	}
	v.input.SetValue("")
	if cmd := v.submitInput(); cmd == nil {
		t.Fatal("final participant did not stage Start")
	}
	if v.CapturingInput() || v.input.Value() != "" || v.review != nil || v.check != nil || len(v.elevationDraft) != 0 {
		t.Fatalf("submitted Start retained prompt state: input=%v review=%v check=%v draft=%d", v.CapturingInput(), v.review != nil, v.check != nil, len(v.elevationDraft))
	}
	if strings.Contains(v.View(), "one-use-secret") {
		t.Fatal("submitted administrator secret leaked into view history")
	}
}

func TestServingGroupElevationCancellationZerosDraftAndSendsNothing(t *testing.T) {
	v := newServingGroupView(nil)
	v.SetSize(120, 30)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	review := servingTestReview()
	check := servingTestCheck(review)
	v.review, v.check = &review, &check
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	v.input.SetValue("cancelled-secret")
	v.submitInput()
	secret := v.elevationDraft[0].ElevationPassword

	if cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Fatal("cancel dispatched an RPC command")
	}
	if v.CapturingInput() || v.input.Value() != "" || len(v.elevationDraft) != 0 || v.review == nil || v.check == nil {
		t.Fatalf("cancel state = input=%v value=%q draft=%d review=%v check=%v", v.CapturingInput(), v.input.Value(), len(v.elevationDraft), v.review != nil, v.check != nil)
	}
	for _, value := range secret {
		if value != 0 {
			t.Fatal("cancelled administrator secret was not zeroed")
		}
	}
	if strings.Contains(v.View(), "cancelled-secret") {
		t.Fatal("cancelled administrator secret leaked into view history")
	}
}

func TestServingGroupStateChangeDiscardsPromptedSecret(t *testing.T) {
	v := newServingGroupView(nil)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	review := servingTestReview()
	check := servingTestCheck(review)
	v.review, v.check = &review, &check
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	v.input.SetValue("discarded-secret")
	v.submitInput()
	secret := v.elevationDraft[0].ElevationPassword
	run := servingTestRun()
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(true), Run: &run}})

	if v.CapturingInput() || len(v.elevationDraft) != 0 || !strings.Contains(v.actionStatus, "discarded") {
		t.Fatalf("state change retained administrator input: input=%v draft=%d status=%q", v.CapturingInput(), len(v.elevationDraft), v.actionStatus)
	}
	for _, value := range secret {
		if value != 0 {
			t.Fatal("state change did not zero administrator input")
		}
	}
}

func TestServingGroupExpiredReviewClearsAccessWithoutStart(t *testing.T) {
	v := newServingGroupView(nil)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	review := servingTestReview()
	review.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	check := servingTestCheck(review)
	v.review, v.check = &review, &check
	password := transientSecret([]byte("expired-secret"))
	elevation := []servingGroupElevation{{NodeID: "node-a", ElevationPassword: password}}

	if cmd := v.submitStart(elevation); cmd == nil {
		t.Fatal("expired review did not request a status refresh")
	}
	if v.pending == "start" || v.uncertainStart || !strings.Contains(v.actionStatus, "expired") {
		t.Fatalf("expired review staged Start: pending=%q uncertain=%v status=%q", v.pending, v.uncertainStart, v.actionStatus)
	}
	for _, value := range password {
		if value != 0 {
			t.Fatal("expired review did not zero administrator input")
		}
	}
}

func TestServingGroupStartPayloadIsTypedAndSecretsAreOneUse(t *testing.T) {
	review := servingTestReview()
	run := servingTestRun()
	run.State, run.Failure = "starting", ""
	password := transientSecret([]byte("administrator-secret"))
	elevation := []servingGroupElevation{
		{NodeID: "node-a", ElevationPassword: password},
		{NodeID: "node-b", NonInteractive: true},
	}
	request, result := runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupStartCmd(client, review, "", elevation)
	}, run)
	var params struct {
		ReviewID  string `json:"reviewId"`
		Elevation []struct {
			NodeID            string `json:"nodeId"`
			ElevationPassword string `json:"elevationPassword"`
			NonInteractive    bool   `json:"nonInteractive"`
		} `json:"elevation"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil || params.ReviewID != review.ReviewID || len(params.Elevation) != 2 {
		t.Fatalf("Start payload shape is invalid: %v", err)
	}
	if params.Elevation[0].ElevationPassword != "administrator-secret" || params.Elevation[0].NonInteractive || params.Elevation[1].ElevationPassword != "" || !params.Elevation[1].NonInteractive {
		t.Fatal("Start payload did not preserve the two exact administrator choices")
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.run == nil {
		t.Fatalf("Start result = %#v", result)
	}
	for _, value := range password {
		if value != 0 {
			t.Fatal("administrator secret was not zeroed after Start RPC")
		}
	}
	if elevation[0].ElevationPassword != nil {
		t.Fatal("Start RPC retained its administrator secret slice")
	}
}

func TestServingGroupStartWithoutElevationKeepsBackendNoninteractiveFallback(t *testing.T) {
	review := servingTestReview()
	run := servingTestRun()
	run.State, run.Failure = "starting", ""
	request, result := runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupStartCmd(client, review, "", nil)
	}, run)
	if strings.Contains(string(request.Params), "elevation") {
		t.Fatal("empty administrator selection changed the backend noninteractive fallback")
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.run == nil {
		t.Fatalf("Start result = %#v", result)
	}
}

func TestServingGroupStartClearsSecretWhenRPCFails(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client := rpc.NewClient(clientConn, clientConn)
	ctx, cancel := context.WithCancel(context.Background())
	go client.Run(ctx)
	serverConn.Close()
	t.Cleanup(func() {
		cancel()
		clientConn.Close()
	})

	password := transientSecret([]byte("failed-rpc-secret"))
	result := servingGroupStartCmd(
		client,
		servingTestReview(),
		"",
		[]servingGroupElevation{{NodeID: "node-a", ElevationPassword: password}},
	)()
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err == nil {
		t.Fatalf("failed Start result = %#v", result)
	}
	for _, value := range password {
		if value != 0 {
			t.Fatal("administrator secret survived failed Start RPC")
		}
	}
}

func TestServingGroupReconcilePromptsOnlyUnresolvedParticipants(t *testing.T) {
	run := servingTestRun()
	run.Ranks[0].CleanupConfirmed = true
	v := newServingGroupView(nil)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(true), Run: &run}})

	if cmd := v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}}); cmd == nil {
		t.Fatal("Reconcile did not open administrator access input")
	}
	if !reflect.DeepEqual(v.elevationNodes, []string{"node-b"}) || v.elevationAction != "reconcile" {
		t.Fatalf("Reconcile elevation nodes = %v action=%q", v.elevationNodes, v.elevationAction)
	}
}

func TestServingGroupReconcilePayloadIsExactAndCleared(t *testing.T) {
	run := servingTestRun()
	status := servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(true), Run: &run}
	password := transientSecret([]byte("reconcile-secret"))
	elevation := []servingGroupElevation{{NodeID: "node-b", ElevationPassword: password}}
	request, result := runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupOperationCmd(client, "engine:vllm-group-reconcile", "reconcile", run, elevation)
	}, status)
	if request.Method != "engine:vllm-group-reconcile" || !strings.Contains(string(request.Params), `"nodeId":"node-b"`) || !strings.Contains(string(request.Params), "reconcile-secret") {
		t.Fatal("Reconcile request did not carry the exact unresolved participant access")
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.status == nil {
		t.Fatalf("Reconcile result = %#v", result)
	}
	for _, value := range password {
		if value != 0 {
			t.Fatal("administrator secret was not zeroed after Reconcile RPC")
		}
	}
}

func TestServingGroupUncertainStartClearsOnlyForMatchingFreshStatus(t *testing.T) {
	v := newServingGroupView(nil)
	v.uncertainStart = true
	v.pendingDigest = strings.Repeat("a", 64)
	v.previousRunID = strings.Repeat("b", 32)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	if !v.uncertainStart || !strings.Contains(v.actionStatus, "will not be resent") {
		t.Fatal("missing retained run cleared ambiguous Start")
	}
	run := servingTestRun()
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(true), Run: &run}})
	if v.uncertainStart || !strings.Contains(v.actionStatus, "reconciled") {
		t.Fatal("matching retained run did not reconcile Start")
	}
}

func TestServingGroupCleanupRefusalRefreshesWithoutClaimingSuccess(t *testing.T) {
	v := newServingGroupView(nil)
	v.loaded = true
	v.pending = "cleanup"
	cmd := v.Update(servingGroupActionMsg{action: "cleanup", err: errors.New("native cleanup is not admitted")})
	if cmd == nil || v.pending != "" || !strings.Contains(v.actionStatus, "not admitted") || strings.Contains(v.actionStatus, "confirmed") {
		t.Fatalf("cleanup refusal state = pending=%q status=%q cmd=%v", v.pending, v.actionStatus, cmd != nil)
	}
}

func TestServingGroupReviewInputStagesOneBoundRequest(t *testing.T) {
	v := newServingGroupView(nil)
	v.Update(servingGroupMsg{status: servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(false)}})
	v.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	if v.inputMode != servingGroupInputNodes {
		t.Fatal("review did not begin with ordered nodes")
	}
	v.input.SetValue("node-a,node-b")
	v.submitInput()
	v.input.SetValue("example/model")
	v.submitInput()
	v.input.SetValue("tensor")
	if cmd := v.submitInput(); cmd == nil || v.pending != "review" || v.CapturingInput() {
		t.Fatalf("review request was not staged once: pending=%q input=%v", v.pending, v.CapturingInput())
	}
}

func TestServingGroupRPCShapesStayExact(t *testing.T) {
	selection := servingGroupSelection{NodeIDs: []string{"node-a", "node-b"}, Model: "example/model", Parallelism: "tensor"}
	review := servingTestReview()
	request, result := runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupReviewCmd(client, selection)
	}, review)
	if request.Method != "engine:vllm-group-review" || strings.Contains(string(request.Params), "elevation") {
		t.Fatalf("review request changed proposal-only shape: %q", request.Method)
	}
	var reviewParams struct {
		Selection servingGroupSelection `json:"selection"`
	}
	if err := json.Unmarshal(request.Params, &reviewParams); err != nil || !reflect.DeepEqual(reviewParams.Selection, selection) {
		t.Fatalf("review params = %+v, err=%v", reviewParams, err)
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.review == nil {
		t.Fatalf("review result = %#v", result)
	}

	check := servingTestCheck(review)
	request, result = runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupCheckCmd(client, review)
	}, check)
	if request.Method != "engine:vllm-group-check" || !strings.Contains(string(request.Params), review.ReviewID) || strings.Contains(string(request.Params), "elevation") {
		t.Fatalf("check request = %s %s", request.Method, request.Params)
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.check == nil {
		t.Fatalf("check result = %#v", result)
	}

	run := servingTestRun()
	run.State, run.Failure = "starting", ""
	request, result = runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupStartCmd(client, review, "", nil)
	}, run)
	if request.Method != "engine:vllm-group-start" || strings.Contains(string(request.Params), "elevation") || !strings.Contains(string(request.Params), review.ReviewID) {
		t.Fatalf("start request = %s %s", request.Method, request.Params)
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.run == nil {
		t.Fatalf("start result = %#v", result)
	}

	status := servingGroupStatus{ActivationEnabled: servingBool(true), Reserved: servingBool(true), Run: &run}
	request, result = runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupOperationCmd(client, "engine:vllm-group-stop", "stop", run, nil)
	}, status)
	if request.Method != "engine:vllm-group-stop" || strings.Contains(string(request.Params), "elevation") || !strings.Contains(string(request.Params), `"generation":13`) {
		t.Fatalf("stop request = %s %s", request.Method, request.Params)
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.status == nil {
		t.Fatalf("stop result = %#v", result)
	}

	request, result = runTUICommand(t, func(client *rpc.Client) tea.Cmd {
		return servingGroupCleanupCmd(client, run)
	}, status)
	if request.Method != "engine:vllm-group-cleanup" || !strings.Contains(string(request.Params), run.PlanDigest) || strings.Contains(string(request.Params), "elevation") {
		t.Fatalf("cleanup request = %s %s", request.Method, request.Params)
	}
	if msg, ok := result.(servingGroupActionMsg); !ok || msg.err != nil || msg.status == nil {
		t.Fatalf("cleanup result = %#v", result)
	}
}

func TestServingGroupCleanupTimeoutCoversBackendBudget(t *testing.T) {
	if servingGroupCleanupTimeout < 60*time.Second || servingGroupCleanupTimeout <= callTimeout {
		t.Fatalf("cleanup timeout = %s, want backend budget plus ordinary-call margin", servingGroupCleanupTimeout)
	}
}
