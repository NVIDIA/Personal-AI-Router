// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	servingGroupReviewTimeout  = 125 * time.Second
	servingGroupQwenTimeout    = 36 * time.Minute
	servingGroupStopTimeout    = 60 * time.Second
	servingGroupCleanupTimeout = servingGroupStopTimeout
	servingGroupQwenModel      = "nvidia/Qwen3.8-Flash-Next-NVFP4@fc694b54fb0174e0913e6adf86691ef85a4ead47"
	servingGroupQwenRuntime    = "0.28.1rc1.dev361+gd4d703caf"
)

var (
	servingGroupIDPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	servingGroupDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	servingGroupTokenPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,127}$`)
)

// These wire shapes follow the sealed managed Engine Manager group contract.
// The TUI remains a client: the backend owns admission, durable state, native
// effects, identity checks, cleanup proofs and lifecycle recovery.
type servingGroupStatus struct {
	ActivationEnabled *bool            `json:"activationEnabled"`
	Reserved          *bool            `json:"reserved"`
	Reason            string           `json:"reason"`
	Run               *servingGroupRun `json:"run,omitempty"`
}

type servingGroupRun struct {
	RunID            string             `json:"runId"`
	Generation       uint64             `json:"generation"`
	PlanDigest       string             `json:"planDigest"`
	Plan             servingGroupPlan   `json:"plan"`
	State            string             `json:"state"`
	Ranks            []servingGroupRank `json:"ranks"`
	CleanupConfirmed bool               `json:"cleanupConfirmed"`
	Failure          string             `json:"failure,omitempty"`
}

type servingGroupPlan struct {
	Coordinator  string                 `json:"coordinator"`
	Model        string                 `json:"model"`
	Runtime      string                 `json:"runtime"`
	Topology     servingGroupTopology   `json:"topology"`
	Members      []servingGroupMember   `json:"members"`
	Transport    *servingGroupTransport `json:"transport,omitempty"`
	DirectSocket *servingGroupSocket    `json:"directSocket,omitempty"`
	RingSocket   *servingGroupSocket    `json:"ringSocket,omitempty"`
}

// Only the reviewed fabric identity of an ordinary NCCL Socket binding; Engine
// Manager validates its interfaces and addresses.
type servingGroupSocket struct {
	Mode                string `json:"mode"`
	OperationID         string `json:"operationId"`
	QualificationSHA256 string `json:"qualificationSha256"`
}

type servingGroupTransport struct {
	Mode                  string `json:"mode"`
	OperationID           string `json:"operationId"`
	QualificationSHA256   string `json:"qualificationSha256"`
	NetGDRLevel           *int   `json:"netGdrLevel"`
	NetGDRC2C             *int   `json:"netGdrC2c"`
	NetGDRRead            *int   `json:"netGdrRead"`
	NetPlugin             string `json:"netPlugin"`
	EnvPlugin             string `json:"envPlugin"`
	GINPlugin             string `json:"ginPlugin"`
	SubnetAwareRouting    *bool  `json:"subnetAwareRouting"`
	SubnetPrefixLength    *int   `json:"subnetPrefixLength"`
	MergeNICs             *bool  `json:"mergeNICs"`
	SocketPayloadFallback *bool  `json:"socketPayloadFallback"`
}

type servingGroupTopology struct {
	TensorParallel   int `json:"tensorParallel"`
	PipelineParallel int `json:"pipelineParallel"`
	DataParallel     int `json:"dataParallel"`
}

type servingGroupMember struct {
	NodeID string `json:"nodeId"`
}

type servingGroupRank struct {
	NodeID           string                    `json:"nodeId"`
	Attempted        bool                      `json:"attempted"`
	Started          bool                      `json:"started"`
	CleanupConfirmed bool                      `json:"cleanupConfirmed"`
	StartFailure     *servingGroupStartFailure `json:"startFailure,omitempty"`
}

type servingGroupStartFailure struct {
	Stage             string   `json:"stage"`
	Code              string   `json:"code"`
	Exit              int      `json:"exit"`
	StdoutBytes       int      `json:"stdoutBytes"`
	StderrCode        string   `json:"stderrCode"`
	MissingProperties []string `json:"missingProperties,omitempty"`
}

type servingGroupSelection struct {
	NodeIDs     []string `json:"nodeIds"`
	Model       string   `json:"model"`
	Parallelism string   `json:"parallelism,omitempty"`
}

type servingGroupReview struct {
	ReviewID          string           `json:"reviewId"`
	PlanDigest        string           `json:"planDigest"`
	Plan              servingGroupPlan `json:"plan"`
	ExpiresAt         int64            `json:"expiresAt"`
	ActivationEnabled *bool            `json:"activationEnabled"`
	Reason            string           `json:"reason"`
}

type servingGroupParticipant struct {
	Protocol          string                    `json:"protocol"`
	Action            string                    `json:"action"`
	NodeID            string                    `json:"nodeId"`
	Controller        string                    `json:"controller"`
	RunID             string                    `json:"runId"`
	Generation        uint64                    `json:"generation"`
	PlanDigest        string                    `json:"planDigest"`
	Rank              int                       `json:"rank"`
	State             string                    `json:"state"`
	Code              string                    `json:"code"`
	Reason            string                    `json:"reason"`
	ActivationEnabled *bool                     `json:"activationEnabled"`
	EffectsApplied    *bool                     `json:"effectsApplied"`
	CleanupConfirmed  *bool                     `json:"cleanupConfirmed"`
	StartFailure      *servingGroupStartFailure `json:"startFailure,omitempty"`
}

type servingGroupCheck struct {
	ReviewID          string                    `json:"reviewId"`
	ActivationEnabled *bool                     `json:"activationEnabled"`
	Participants      []servingGroupParticipant `json:"participants"`
}

// servingGroupElevation is one operation-scoped administrator choice for one
// exact reviewed participant. Password bytes stay mutable so the RPC command
// can zero them on every completion path; a blank prompt is represented only by
// the explicit noninteractive choice.
type servingGroupElevation struct {
	NodeID            string          `json:"nodeId"`
	ElevationPassword transientSecret `json:"elevationPassword,omitempty"`
	NonInteractive    bool            `json:"nonInteractive"`
}

func clearServingGroupElevations(entries []servingGroupElevation) {
	for i := range entries {
		clearSecret(entries[i].ElevationPassword)
		entries[i].ElevationPassword = nil
	}
}

type servingGroupMsg struct {
	status servingGroupStatus
	err    error
}

type servingGroupActionMsg struct {
	action string
	review *servingGroupReview
	check  *servingGroupCheck
	run    *servingGroupRun
	status *servingGroupStatus
	err    error
}

type servingGroupInputMode uint8

const (
	servingGroupInputNone servingGroupInputMode = iota
	servingGroupInputNodes
	servingGroupInputModel
	servingGroupInputParallelism
	servingGroupInputElevation
)

type servingGroupView struct {
	client *rpc.Client

	status servingGroupStatus
	review *servingGroupReview
	check  *servingGroupCheck
	err    error
	loaded bool

	pending        string
	actionStatus   string
	uncertainStart bool
	pendingDigest  string
	previousRunID  string

	input           textinput.Model
	inputMode       servingGroupInputMode
	draftSelection  servingGroupSelection
	elevationAction string
	elevationNodes  []string
	elevationIndex  int
	elevationDraft  []servingGroupElevation
	elevationRun    servingGroupRun

	width, height int
}

var (
	refreshServingGroupKey   = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))
	reviewServingGroupKey    = key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "review group"))
	checkServingGroupKey     = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "check review"))
	startServingGroupKey     = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start reviewed"))
	stopServingGroupKey      = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop exact run"))
	reconcileServingGroupKey = key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "reconcile cleanup"))
	cleanupServingGroupKey   = key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "request cleanup"))
	submitServingGroupKey    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "continue"))
	cancelServingGroupKey    = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel"))
)

func newServingGroupView(client *rpc.Client) *servingGroupView {
	input := textinput.New()
	input.CharLimit = 512
	return &servingGroupView{client: client, input: input}
}

func (v *servingGroupView) Title() string { return "Serving" }

func (v *servingGroupView) Init() tea.Cmd { return v.refresh() }

func (v *servingGroupView) refresh() tea.Cmd {
	return servingGroupStatusCmd(v.client)
}

func servingGroupStatusCmd(client *rpc.Client) tea.Cmd {
	return call(client, "engine:vllm-group-status", map[string]any{}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return servingGroupMsg{err: err}
		}
		var status servingGroupStatus
		if err := decodeParams(msg.Result, &status); err != nil {
			return servingGroupMsg{err: err}
		}
		if err := validateServingGroupStatus(status); err != nil {
			return servingGroupMsg{err: err}
		}
		return servingGroupMsg{status: status}
	})
}

func servingGroupReviewCmd(client *rpc.Client, selection servingGroupSelection) tea.Cmd {
	timeout := servingGroupReviewTimeout
	if selection.Model == servingGroupQwenModel {
		timeout = servingGroupQwenTimeout
	}
	selectionParams := map[string]any{"nodeIds": selection.NodeIDs, "model": selection.Model}
	if selection.Parallelism != "" {
		selectionParams["parallelism"] = selection.Parallelism
	}
	return callWithTimeout(client, timeout, "engine:vllm-group-review", map[string]any{"selection": selectionParams}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return servingGroupActionMsg{action: "review", err: err}
		}
		var review servingGroupReview
		if err := decodeParams(msg.Result, &review); err != nil {
			return servingGroupActionMsg{action: "review", err: err}
		}
		if err := validateServingGroupReview(review, selection); err != nil {
			return servingGroupActionMsg{action: "review", err: err}
		}
		return servingGroupActionMsg{action: "review", review: &review}
	})
}

func servingGroupCheckCmd(client *rpc.Client, review servingGroupReview) tea.Cmd {
	return call(client, "engine:vllm-group-check", map[string]any{"reviewId": review.ReviewID}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return servingGroupActionMsg{action: "check", err: err}
		}
		var check servingGroupCheck
		if err := decodeParams(msg.Result, &check); err != nil {
			return servingGroupActionMsg{action: "check", err: err}
		}
		if err := validateServingGroupCheck(check, review); err != nil {
			return servingGroupActionMsg{action: "check", err: err}
		}
		return servingGroupActionMsg{action: "check", check: &check}
	})
}

func servingGroupStartCmd(client *rpc.Client, review servingGroupReview, previousRunID string, elevation []servingGroupElevation) tea.Cmd {
	return func() tea.Msg {
		defer clearServingGroupElevations(elevation)
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		params := struct {
			ReviewID  string                  `json:"reviewId"`
			Elevation []servingGroupElevation `json:"elevation,omitempty"`
		}{review.ReviewID, elevation}
		msg, err := client.Call(ctx, "engine:vllm-group-start", params)
		if err != nil {
			return servingGroupActionMsg{action: "start", err: err}
		}
		var run servingGroupRun
		if err := decodeParams(msg.Result, &run); err != nil {
			return servingGroupActionMsg{action: "start", err: err}
		}
		if err := validateServingGroupRun(run); err != nil {
			return servingGroupActionMsg{action: "start", err: err}
		}
		if run.PlanDigest != review.PlanDigest || !reflect.DeepEqual(run.Plan, review.Plan) || run.RunID == previousRunID {
			return servingGroupActionMsg{action: "start", err: fmt.Errorf("Start returned a different or previous operation; cleanup is unconfirmed")}
		}
		return servingGroupActionMsg{action: "start", run: &run}
	}
}

func servingGroupOperationCmd(client *rpc.Client, method, action string, run servingGroupRun, elevation []servingGroupElevation) tea.Cmd {
	return func() tea.Msg {
		defer clearServingGroupElevations(elevation)
		ctx, cancel := context.WithTimeout(context.Background(), servingGroupStopTimeout)
		defer cancel()
		params := struct {
			RunID      string                  `json:"runId"`
			Generation uint64                  `json:"generation"`
			Elevation  []servingGroupElevation `json:"elevation,omitempty"`
		}{run.RunID, run.Generation, elevation}
		msg, err := client.Call(ctx, method, params)
		if err != nil {
			return servingGroupActionMsg{action: action, err: err}
		}
		var status servingGroupStatus
		if err := decodeParams(msg.Result, &status); err != nil {
			return servingGroupActionMsg{action: action, err: err}
		}
		if err := validateServingGroupStatus(status); err != nil {
			return servingGroupActionMsg{action: action, err: err}
		}
		if status.Run == nil || status.Run.RunID != run.RunID || status.Run.Generation != run.Generation || status.Run.PlanDigest != run.PlanDigest {
			return servingGroupActionMsg{action: action, err: fmt.Errorf("%s returned a different operation; cleanup is unconfirmed", action)}
		}
		return servingGroupActionMsg{action: action, status: &status}
	}
}

func servingGroupCleanupCmd(client *rpc.Client, run servingGroupRun) tea.Cmd {
	params := map[string]any{"runId": run.RunID, "generation": run.Generation, "planDigest": run.PlanDigest}
	return callWithTimeout(client, servingGroupCleanupTimeout, "engine:vllm-group-cleanup", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return servingGroupActionMsg{action: "cleanup", err: err}
		}
		var status servingGroupStatus
		if err := decodeParams(msg.Result, &status); err != nil {
			return servingGroupActionMsg{action: "cleanup", err: err}
		}
		if err := validateServingGroupStatus(status); err != nil {
			return servingGroupActionMsg{action: "cleanup", err: err}
		}
		if status.Run == nil || status.Run.RunID != run.RunID || status.Run.Generation != run.Generation || status.Run.PlanDigest != run.PlanDigest {
			return servingGroupActionMsg{action: "cleanup", err: fmt.Errorf("cleanup returned a different operation; cleanup is unconfirmed")}
		}
		return servingGroupActionMsg{action: "cleanup", status: &status}
	})
}

func validateServingGroupStatus(status servingGroupStatus) error {
	if status.ActivationEnabled == nil || status.Reserved == nil {
		return fmt.Errorf("serving-group authority fields are missing")
	}
	if !validServingGroupText(status.Reason, 2048) {
		return fmt.Errorf("serving-group reason is invalid")
	}
	if !*status.Reserved && status.Run == nil {
		return nil
	}
	if status.Run == nil {
		return fmt.Errorf("reserved serving-group status has no retained run")
	}
	if err := validateServingGroupRun(*status.Run); err != nil {
		return err
	}
	if *status.Reserved == status.Run.CleanupConfirmed {
		return fmt.Errorf("serving-group reservation contradicts cleanup evidence")
	}
	return nil
}

func validateServingGroupRun(run servingGroupRun) error {
	if !servingGroupIDPattern.MatchString(run.RunID) || run.Generation == 0 || !servingGroupDigestPattern.MatchString(run.PlanDigest) {
		return fmt.Errorf("serving-group identity is incomplete")
	}
	known := map[string]bool{"starting": true, "ready": true, "stopping": true, "cleanup-required": true, "stopped": true, "failed": true}
	historicalTransport := run.CleanupConfirmed && (run.State == "stopped" || run.State == "failed")
	if !known[run.State] || validateServingGroupPlanMode(run.Plan, historicalTransport) != nil || len(run.Plan.Members) != len(run.Ranks) {
		return fmt.Errorf("serving-group state or participants are invalid")
	}
	if !validServingGroupText(run.Failure, 2048) {
		return fmt.Errorf("serving-group failure is invalid")
	}
	for i, rank := range run.Ranks {
		if rank.NodeID != run.Plan.Members[i].NodeID || rank.Started && !rank.Attempted || rank.CleanupConfirmed && !rank.Attempted {
			return fmt.Errorf("serving-group ranks do not match members")
		}
		if rank.StartFailure != nil && validateServingGroupStartFailure(*rank.StartFailure) != nil {
			return fmt.Errorf("serving-group rank failure is invalid")
		}
	}
	if run.CleanupConfirmed && (!allRanksClean(run.Ranks) || run.State != "stopped" && run.State != "failed") {
		return fmt.Errorf("serving-group cleanup evidence is inconsistent")
	}
	return nil
}

func validateServingGroupPlan(plan servingGroupPlan) error {
	return validateServingGroupPlanMode(plan, false)
}

func validateServingGroupPlanMode(plan servingGroupPlan, allowHistoricalTransport bool) error {
	if len(plan.Members) < 2 || len(plan.Members) > 3 || plan.Coordinator == "" || plan.Coordinator != plan.Members[0].NodeID || !validServingGroupModel(plan.Model) || !validServingGroupText(plan.Runtime, 128) {
		return fmt.Errorf("serving-group plan is incomplete")
	}
	if plan.Topology.TensorParallel <= 0 || plan.Topology.PipelineParallel <= 0 || plan.Topology.DataParallel <= 0 {
		return fmt.Errorf("serving-group topology is incomplete")
	}
	seen := make(map[string]bool, len(plan.Members))
	for _, member := range plan.Members {
		if !validServingGroupNodeID(member.NodeID) || seen[member.NodeID] {
			return fmt.Errorf("serving-group members are invalid")
		}
		seen[member.NodeID] = true
	}
	qwen := plan.Model == servingGroupQwenModel
	if !qwen {
		if plan.Transport != nil {
			return fmt.Errorf("ordinary serving-group plan claimed the fixed Qwen transport")
		}
		return validateServingGroupSocket(plan)
	}
	if plan.DirectSocket != nil || plan.RingSocket != nil {
		return fmt.Errorf("fixed Qwen serving-group plan claimed an ordinary socket binding")
	}
	// Qwen3.8 serves only on two Sparks, as TP2+EP2.
	if plan.Runtime != servingGroupQwenRuntime || len(plan.Members) != 2 || plan.Topology.TensorParallel != 2 || plan.Topology.PipelineParallel != 1 || plan.Topology.DataParallel != 1 {
		return fmt.Errorf("fixed Qwen serving-group topology is invalid")
	}
	if err := validateServingGroupTransport(plan.Transport, allowHistoricalTransport); err != nil {
		return err
	}
	return nil
}

// An ordinary plan binds NCCL Socket to at most one fabric: the direct lane of
// a two-node TP2 group, or the routed ring of a three-node TP3 or PP3 group.
func validateServingGroupSocket(plan servingGroupPlan) error {
	topology := plan.Topology
	socket, mode := plan.DirectSocket, "qualified-direct-socket"
	fits := len(plan.Members) == 2 && topology.TensorParallel == 2 && topology.PipelineParallel == 1
	if plan.RingSocket != nil {
		socket, mode = plan.RingSocket, "qualified-ring-socket"
		fits = len(plan.Members) == 3 && (topology.TensorParallel == 3 && topology.PipelineParallel == 1 || topology.TensorParallel == 1 && topology.PipelineParallel == 3)
	}
	if socket == nil {
		return nil
	}
	if plan.DirectSocket != nil && plan.RingSocket != nil || !fits || topology.DataParallel != 1 || socket.Mode != mode ||
		!servingGroupIDPattern.MatchString(socket.OperationID) || !servingGroupDigestPattern.MatchString(socket.QualificationSHA256) {
		return fmt.Errorf("ordinary serving-group socket binding is invalid")
	}
	return nil
}

func validateServingGroupTransport(transport *servingGroupTransport, allowHistorical bool) error {
	if transport == nil || transport.Mode != "host-buffer-roce" ||
		!servingGroupIDPattern.MatchString(transport.OperationID) ||
		!servingGroupDigestPattern.MatchString(transport.QualificationSHA256) ||
		transport.NetGDRLevel == nil || *transport.NetGDRLevel != 0 ||
		transport.NetGDRC2C == nil || *transport.NetGDRC2C != 0 ||
		transport.NetGDRRead == nil || *transport.NetGDRRead != 0 ||
		transport.NetPlugin != "none" || transport.EnvPlugin != "none" || transport.GINPlugin != "none" ||
		transport.SocketPayloadFallback == nil || *transport.SocketPayloadFallback {
		return fmt.Errorf("fixed Qwen serving-group transport is invalid")
	}
	historical := transport.SubnetAwareRouting == nil && transport.SubnetPrefixLength == nil && transport.MergeNICs == nil
	if historical {
		if allowHistorical {
			return nil
		}
		return fmt.Errorf("fixed Qwen serving-group transport lacks routing policy")
	}
	if transport.MergeNICs == nil || !*transport.MergeNICs {
		return fmt.Errorf("fixed Qwen serving-group transport is invalid")
	}
	if transport.SubnetAwareRouting == nil || *transport.SubnetAwareRouting ||
		transport.SubnetPrefixLength == nil || *transport.SubnetPrefixLength != 0 {
		return fmt.Errorf("fixed Qwen peer-subnet routing is invalid")
	}
	return nil
}

func validateServingGroupReview(review servingGroupReview, selection servingGroupSelection) error {
	if !servingGroupIDPattern.MatchString(review.ReviewID) || !servingGroupDigestPattern.MatchString(review.PlanDigest) || review.ExpiresAt <= 0 || review.ActivationEnabled == nil || !validServingGroupText(review.Reason, 2048) {
		return fmt.Errorf("serving-group review identity is invalid")
	}
	if err := validateServingGroupPlan(review.Plan); err != nil {
		return err
	}
	if review.Plan.Model != selection.Model || len(review.Plan.Members) != len(selection.NodeIDs) {
		return fmt.Errorf("PAIR returned a review for a different selection")
	}
	for i, nodeID := range selection.NodeIDs {
		if review.Plan.Members[i].NodeID != nodeID {
			return fmt.Errorf("PAIR returned a review for a different selection")
		}
	}
	switch selection.Parallelism {
	case "tensor":
		if review.Plan.Topology.TensorParallel != len(selection.NodeIDs) || review.Plan.Topology.PipelineParallel != 1 {
			return fmt.Errorf("PAIR returned a different tensor topology")
		}
	case "pipeline":
		if review.Plan.Topology.TensorParallel != 1 || review.Plan.Topology.PipelineParallel != len(selection.NodeIDs) {
			return fmt.Errorf("PAIR returned a different pipeline topology")
		}
	}
	return nil
}

func validateServingGroupCheck(check servingGroupCheck, review servingGroupReview) error {
	if check.ReviewID != review.ReviewID || check.ActivationEnabled == nil || len(check.Participants) != len(review.Plan.Members) {
		return fmt.Errorf("PAIR returned a check for a different review")
	}
	for i, participant := range check.Participants {
		if participant.Protocol != "pair-vllm-group/1" || participant.Action != "capability" || participant.NodeID != review.Plan.Members[i].NodeID || !validServingGroupNodeID(participant.Controller) || participant.RunID != review.ReviewID || participant.Generation != 1 || participant.PlanDigest != review.PlanDigest || participant.Rank != i || !servingGroupTokenPattern.MatchString(participant.State) || !servingGroupTokenPattern.MatchString(participant.Code) || !validServingGroupText(participant.Reason, 2048) || participant.ActivationEnabled == nil || participant.EffectsApplied == nil || participant.CleanupConfirmed == nil || *participant.EffectsApplied || *participant.CleanupConfirmed {
			return fmt.Errorf("serving-group participant check is invalid")
		}
		if participant.StartFailure != nil && validateServingGroupStartFailure(*participant.StartFailure) != nil {
			return fmt.Errorf("serving-group participant failure is invalid")
		}
	}
	return nil
}

func validateServingGroupStartFailure(failure servingGroupStartFailure) error {
	stages := map[string]bool{"admission": true, "helper": true, "native-return": true, "peer-return": true, "peer-transport": true, "journal": true}
	stderrCodes := map[string]bool{"none": true, "unclassified": true, "worker_rejected": true, "sudo_authentication_required": true, "sudo_not_permitted": true}
	if !stages[failure.Stage] || !servingGroupTokenPattern.MatchString(failure.Code) || !stderrCodes[failure.StderrCode] || failure.Exit < -1 || failure.Exit > 255 || failure.StdoutBytes < 0 || failure.StdoutBytes > 1<<20 || len(failure.MissingProperties) > 64 {
		return fmt.Errorf("invalid bounded rank failure")
	}
	return nil
}

func allRanksClean(ranks []servingGroupRank) bool {
	for _, rank := range ranks {
		if rank.Attempted && !rank.CleanupConfirmed {
			return false
		}
	}
	return true
}

func validServingGroupNodeID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char <= 32 || char == 127 {
			return false
		}
	}
	return true
}

func validServingGroupModel(value string) bool {
	return value != "" && len(value) <= 512 && !strings.ContainsFunc(value, func(char rune) bool { return char <= 32 || char == 127 })
}

func validServingGroupText(value string, limit int) bool {
	return len(value) <= limit && !strings.ContainsFunc(value, func(char rune) bool { return char < 32 || char == 127 })
}

func parseServingGroupNodes(value string) ([]string, error) {
	parts := strings.Split(value, ",")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fmt.Errorf("enter two or three comma-separated node IDs, coordinator first")
	}
	seen := make(map[string]bool, len(parts))
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		nodeID := strings.TrimSpace(part)
		if !validServingGroupNodeID(nodeID) || seen[nodeID] {
			return nil, fmt.Errorf("node IDs must be distinct and contain no spaces")
		}
		seen[nodeID] = true
		result = append(result, nodeID)
	}
	return result, nil
}

func (v *servingGroupView) SetSize(w, h int) { v.width, v.height = w, h }

func (v *servingGroupView) CapturingInput() bool { return v.inputMode != servingGroupInputNone }

func (v *servingGroupView) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case servingGroupMsg:
		v.loaded, v.status, v.err = true, msg.status, msg.err
		if msg.err == nil && v.uncertainStart {
			matching := msg.status.Run != nil && msg.status.Run.PlanDigest == v.pendingDigest && msg.status.Run.RunID != v.previousRunID
			if matching {
				v.uncertainStart = false
				v.pendingDigest, v.previousRunID = "", ""
				v.actionStatus = "Start reconciled to the retained serving group"
			} else {
				v.actionStatus = "Start outcome is still uncertain; Start will not be resent"
			}
		}
		if msg.err == nil && msg.status.Run != nil && !msg.status.Run.CleanupConfirmed {
			if v.inputMode == servingGroupInputElevation {
				v.cancelInput()
				v.actionStatus = "serving-group state changed; administrator input discarded"
			}
			v.review, v.check = nil, nil
		}
	case servingGroupActionMsg:
		return v.handleAction(msg)
	case tea.KeyMsg:
		return v.handleKey(msg)
	}
	return nil
}

func (v *servingGroupView) handleAction(msg servingGroupActionMsg) tea.Cmd {
	v.pending = ""
	if msg.err != nil {
		v.actionStatus = msg.action + " failed: " + msg.err.Error()
		if msg.action == "start" {
			v.uncertainStart = true
		}
		if msg.action == "start" || msg.action == "stop" || msg.action == "reconcile" || msg.action == "cleanup" {
			return v.refresh()
		}
		return nil
	}
	switch msg.action {
	case "review":
		v.review, v.check = msg.review, nil
		v.actionStatus = "review ready; check every participant before Start"
	case "check":
		v.check = msg.check
		if msg.check != nil && msg.check.ActivationEnabled != nil && *msg.check.ActivationEnabled {
			v.actionStatus = "participant check passed; Start uses PAIR's owned fixed helper"
		} else {
			v.actionStatus = "participant check completed; activation remains unavailable"
		}
	case "start":
		v.review, v.check, v.uncertainStart = nil, nil, false
		if msg.run != nil {
			v.status.Run = msg.run
		}
		v.actionStatus = "Start accepted; refreshing retained owner status"
		return v.refresh()
	case "stop", "reconcile", "cleanup":
		if msg.status != nil {
			v.status = *msg.status
		}
		v.actionStatus = msg.action + " completed; refreshing cleanup proof"
		return v.refresh()
	}
	return nil
}

func (v *servingGroupView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.inputMode != servingGroupInputNone {
		switch {
		case key.Matches(msg, submitServingGroupKey):
			return v.submitInput()
		case key.Matches(msg, cancelServingGroupKey):
			v.cancelInput()
			v.actionStatus = "serving-group input cancelled"
			return nil
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	if key.Matches(msg, refreshServingGroupKey) {
		return v.refresh()
	}
	if v.pending != "" || !v.loaded || v.err != nil {
		return nil
	}
	switch {
	case key.Matches(msg, reviewServingGroupKey):
		if v.canReview() {
			v.beginInput(servingGroupInputNodes, "node-a,node-b[,node-c] (coordinator first)")
			return textinput.Blink
		}
	case key.Matches(msg, checkServingGroupKey):
		if v.canCheck() {
			v.pending = "check"
			return servingGroupCheckCmd(v.client, *v.review)
		}
	case key.Matches(msg, startServingGroupKey):
		if v.canStart() {
			nodes := make([]string, 0, len(v.review.Plan.Members))
			for _, member := range v.review.Plan.Members {
				nodes = append(nodes, member.NodeID)
			}
			return v.beginElevation("start", nodes, nil)
		}
	case key.Matches(msg, stopServingGroupKey):
		if run := v.uncleanRun(); run != nil {
			v.pending = "stop"
			return servingGroupOperationCmd(v.client, "engine:vllm-group-stop", "stop", *run, nil)
		}
	case key.Matches(msg, reconcileServingGroupKey):
		if run := v.uncleanRun(); run != nil {
			nodes := unresolvedServingGroupNodes(*run)
			if len(nodes) == 0 {
				v.pending = "reconcile"
				return servingGroupOperationCmd(v.client, "engine:vllm-group-reconcile", "reconcile", *run, nil)
			}
			return v.beginElevation("reconcile", nodes, run)
		}
	case key.Matches(msg, cleanupServingGroupKey):
		if run := v.uncleanRun(); run != nil {
			v.pending = "cleanup"
			return servingGroupCleanupCmd(v.client, *run)
		}
	}
	return nil
}

func (v *servingGroupView) canReview() bool {
	if v.status.Reserved == nil || *v.status.Reserved || v.uncertainStart {
		return false
	}
	return v.status.Run == nil || v.status.Run.CleanupConfirmed
}

func (v *servingGroupView) canCheck() bool {
	return v.review != nil && v.review.ExpiresAt > time.Now().UnixMilli()
}

func (v *servingGroupView) canStart() bool {
	return v.canCheck() && v.review.ActivationEnabled != nil && *v.review.ActivationEnabled && v.check != nil && v.check.ActivationEnabled != nil && *v.check.ActivationEnabled && !v.uncertainStart
}

func (v *servingGroupView) uncleanRun() *servingGroupRun {
	if v.status.Run == nil || v.status.Run.CleanupConfirmed {
		return nil
	}
	return v.status.Run
}

func unresolvedServingGroupNodes(run servingGroupRun) []string {
	nodes := make([]string, 0, len(run.Ranks))
	for _, rank := range run.Ranks {
		if rank.Attempted && !rank.CleanupConfirmed {
			nodes = append(nodes, rank.NodeID)
		}
	}
	return nodes
}

func (v *servingGroupView) beginInput(mode servingGroupInputMode, placeholder string) {
	v.inputMode = mode
	v.input.SetValue("")
	v.input.Placeholder = placeholder
	v.input.EchoMode = textinput.EchoNormal
	v.input.Focus()
}

func (v *servingGroupView) beginElevation(action string, nodes []string, run *servingGroupRun) tea.Cmd {
	v.clearElevationDraft()
	v.elevationAction = action
	v.elevationNodes = append([]string(nil), nodes...)
	if run != nil {
		v.elevationRun = *run
	}
	v.beginElevationInput()
	return textinput.Blink
}

func (v *servingGroupView) beginElevationInput() {
	v.beginInput(servingGroupInputElevation, "administrator password (blank = passwordless sudo)")
	v.input.EchoMode = textinput.EchoPassword
	v.input.EchoCharacter = '•'
	v.actionStatus = fmt.Sprintf(
		"one-use administrator access for %s (%d/%d)",
		v.elevationNodes[v.elevationIndex],
		v.elevationIndex+1,
		len(v.elevationNodes),
	)
}

func (v *servingGroupView) clearElevationDraft() {
	clearServingGroupElevations(v.elevationDraft)
	v.elevationAction = ""
	v.elevationNodes = nil
	v.elevationIndex = 0
	v.elevationDraft = nil
	v.elevationRun = servingGroupRun{}
}

func (v *servingGroupView) cancelInput() {
	v.clearElevationDraft()
	v.inputMode = servingGroupInputNone
	v.input.SetValue("")
	v.input.Blur()
	v.draftSelection = servingGroupSelection{}
}

func (v *servingGroupView) submitInput() tea.Cmd {
	if v.inputMode == servingGroupInputElevation {
		return v.submitElevationInput()
	}
	value := strings.TrimSpace(v.input.Value())
	switch v.inputMode {
	case servingGroupInputNodes:
		nodes, err := parseServingGroupNodes(value)
		if err != nil {
			v.actionStatus = err.Error()
			return nil
		}
		v.draftSelection.NodeIDs = nodes
		v.beginInput(servingGroupInputModel, "exact downloaded model ID")
		return textinput.Blink
	case servingGroupInputModel:
		if !validServingGroupModel(value) {
			v.actionStatus = "one exact downloaded model ID is required"
			return nil
		}
		v.draftSelection.Model = value
		v.beginInput(servingGroupInputParallelism, "tensor, pipeline, or blank for PAIR default")
		return textinput.Blink
	case servingGroupInputParallelism:
		if value != "" && value != "tensor" && value != "pipeline" {
			v.actionStatus = "parallelism must be tensor, pipeline, or blank"
			return nil
		}
		v.draftSelection.Parallelism = value
		selection := v.draftSelection
		v.cancelInput()
		v.pending = "review"
		v.actionStatus = "building a fresh serving-group review..."
		return servingGroupReviewCmd(v.client, selection)
	}
	return nil
}

func (v *servingGroupView) submitElevationInput() tea.Cmd {
	nodeID := v.elevationNodes[v.elevationIndex]
	entry := servingGroupElevation{NodeID: nodeID}
	if value := v.input.Value(); value == "" {
		entry.NonInteractive = true
	} else {
		entry.ElevationPassword = transientSecret([]byte(value))
	}
	v.input.SetValue("")
	v.elevationDraft = append(v.elevationDraft, entry)
	v.elevationIndex++
	if v.elevationIndex < len(v.elevationNodes) {
		v.beginElevationInput()
		return textinput.Blink
	}

	action, run, nodes := v.elevationAction, v.elevationRun, v.elevationNodes
	elevation := v.elevationDraft
	v.elevationAction, v.elevationNodes, v.elevationDraft = "", nil, nil
	v.elevationIndex, v.elevationRun = 0, servingGroupRun{}
	v.inputMode = servingGroupInputNone
	v.input.Blur()

	switch action {
	case "start":
		return v.submitStart(elevation)
	case "reconcile":
		current := v.uncleanRun()
		if current == nil || current.RunID != run.RunID || current.Generation != run.Generation || current.PlanDigest != run.PlanDigest || !reflect.DeepEqual(nodes, unresolvedServingGroupNodes(*current)) {
			clearServingGroupElevations(elevation)
			v.actionStatus = "serving-group generation changed; administrator input discarded"
			return v.refresh()
		}
		v.pending = "reconcile"
		return servingGroupOperationCmd(v.client, "engine:vllm-group-reconcile", "reconcile", run, elevation)
	default:
		clearServingGroupElevations(elevation)
		v.actionStatus = "administrator input discarded"
		return nil
	}
}

func (v *servingGroupView) submitStart(elevation []servingGroupElevation) tea.Cmd {
	if !v.canStart() {
		clearServingGroupElevations(elevation)
		v.actionStatus = "review expired or changed before Start"
		return v.refresh()
	}
	review := *v.review
	previous := ""
	if v.status.Run != nil {
		previous = v.status.Run.RunID
	}
	// Consume before sending. A lost response is reconciled by status and
	// never turns into an automatic duplicate Start.
	v.review, v.check, v.uncertainStart, v.pending = nil, nil, true, "start"
	v.pendingDigest, v.previousRunID = review.PlanDigest, previous
	return servingGroupStartCmd(v.client, review, previous, elevation)
}

func (v *servingGroupView) View() string {
	if !v.loaded {
		return footerStyle.Render("Loading serving-group status…")
	}
	if v.err != nil {
		return statusErrStyle.Render("Serving-group status unavailable.") + "\n" + footerStyle.Render(v.err.Error())
	}
	var b strings.Builder
	if v.status.Run == nil {
		b.WriteString(statusOKStyle.Render("No retained serving group."))
		b.WriteByte('\n')
	} else {
		v.renderRun(&b, *v.status.Run)
	}
	if v.review != nil {
		remaining := time.Until(time.UnixMilli(v.review.ExpiresAt)).Round(time.Second)
		if remaining < 0 {
			remaining = 0
		}
		b.WriteString(fmt.Sprintf("\nReview %s · expires in %s · %s\n", truncate(v.review.ReviewID, 16), remaining, authorityLabel(v.review.ActivationEnabled)))
		b.WriteString(fmt.Sprintf("  %s · %s · %d nodes\n", truncate(v.review.Plan.Model, clampWidth(v.width-8, 16)), topologyLabel(v.review.Plan.Topology), len(v.review.Plan.Members)))
		if transport := servingGroupTransportLabel(v.review.Plan); transport != "" {
			b.WriteString("  " + transport + "\n")
		}
		if v.review.Reason != "" {
			b.WriteString("  " + footerStyle.Render(truncate(v.review.Reason, clampWidth(v.width-4, 20))) + "\n")
		}
	}
	if v.check != nil {
		b.WriteString("Participant check · " + authorityLabel(v.check.ActivationEnabled) + "\n")
		for _, participant := range v.check.Participants {
			b.WriteString(fmt.Sprintf("  %s  %s/%s\n", truncate(participant.NodeID, 16), participant.State, participant.Code))
			if participant.Reason != "" {
				b.WriteString("    " + footerStyle.Render(truncate(participant.Reason, clampWidth(v.width-6, 20))) + "\n")
			}
		}
	}
	if v.inputMode != servingGroupInputNone {
		if v.inputMode == servingGroupInputElevation {
			b.WriteString(fmt.Sprintf(
				"\nAdministrator access for %s (%d/%d). Press Enter blank only to select passwordless sudo.\n",
				truncate(v.elevationNodes[v.elevationIndex], 32),
				v.elevationIndex+1,
				len(v.elevationNodes),
			))
		}
		b.WriteString("\n" + v.input.View() + "\n")
	}
	if v.pending != "" {
		b.WriteString("\n" + footerStyle.Render(v.pending+" in progress...") + "\n")
	}
	if v.actionStatus != "" {
		b.WriteString("\n" + footerStyle.Render(truncate(v.actionStatus, clampWidth(v.width, 24))))
	}
	reason := v.status.Reason
	if reason != "" {
		b.WriteString("\n" + footerStyle.Render(truncate(reason, clampWidth(v.width, 24))))
	}
	return b.String()
}

func (v *servingGroupView) renderRun(b *strings.Builder, run servingGroupRun) {
	b.WriteString(fmt.Sprintf("Run %s · generation %d · %s\n", truncate(run.RunID, 16), run.Generation, run.State))
	b.WriteString(fmt.Sprintf("Coordinator %s · runtime %s\n", truncate(run.Plan.Coordinator, 16), run.Plan.Runtime))
	b.WriteString(fmt.Sprintf("%s · %d members\n", topologyLabel(run.Plan.Topology), len(run.Plan.Members)))
	b.WriteString("Model " + truncate(run.Plan.Model, clampWidth(v.width-6, 16)) + "\n")
	if transport := servingGroupTransportLabel(run.Plan); transport != "" {
		b.WriteString(transport + "\n")
	}
	for _, rank := range run.Ranks {
		state := "held"
		switch {
		case rank.CleanupConfirmed:
			state = "cleanup confirmed"
		case rank.StartFailure != nil:
			state = rank.StartFailure.Stage + "/" + rank.StartFailure.Code
		case rank.Started:
			state = "started"
		case rank.Attempted:
			state = "attempted"
		}
		b.WriteString(fmt.Sprintf("  %s  %s\n", truncate(rank.NodeID, 16), state))
	}
	if run.Failure != "" {
		b.WriteString(statusErrStyle.Render(truncate(run.Failure, clampWidth(v.width, 24))) + "\n")
	}
}

func topologyLabel(topology servingGroupTopology) string {
	return fmt.Sprintf("TP %d · PP %d · DP %d", topology.TensorParallel, topology.PipelineParallel, topology.DataParallel)
}

func servingGroupTransportLabel(plan servingGroupPlan) string {
	switch {
	case plan.DirectSocket != nil:
		return "NCCL Socket on the qualified direct fabric lane · control on management · RDMA off"
	case plan.RingSocket != nil:
		return "NCCL Socket on the qualified routed ring · control on management · RDMA off"
	case plan.Transport == nil:
		return ""
	case plan.Transport.SubnetAwareRouting == nil:
		return "host-buffer RoCE · historical peer-subnet policy not recorded · socket payload fallback off"
	}
	return "host-buffer RoCE · merged NICs on · socket payload fallback off"
}

func authorityLabel(enabled *bool) string {
	if enabled != nil && *enabled {
		return "activation available"
	}
	return "activation unavailable"
}

func (v *servingGroupView) Help() []key.Binding {
	if v.inputMode != servingGroupInputNone {
		return []key.Binding{submitServingGroupKey, cancelServingGroupKey}
	}
	result := []key.Binding{refreshServingGroupKey}
	if v.pending != "" || !v.loaded || v.err != nil {
		return result
	}
	if v.canReview() {
		result = append(result, reviewServingGroupKey)
	}
	if v.canCheck() {
		result = append(result, checkServingGroupKey)
	}
	if v.canStart() {
		result = append(result, startServingGroupKey)
	}
	if v.uncleanRun() != nil {
		result = append(result, stopServingGroupKey, reconcileServingGroupKey, cleanupServingGroupKey)
	}
	return result
}
