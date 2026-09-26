// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nvpair-shared/cableprobe"
	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
)

const setupOperationTimeout = 3*time.Minute + 10*time.Second

type setupCandidate struct {
	CandidateID           string `json:"candidateId"`
	Label                 string `json:"label"`
	Address               string `json:"address"`
	Port                  int    `json:"port"`
	AccessID              string `json:"accessId"`
	AccessLabel           string `json:"accessLabel"`
	AccessAvailable       bool   `json:"accessAvailable"`
	HostKeySHA256         string `json:"hostKeySha256,omitempty"`
	HostKeyTrusted        bool   `json:"hostKeyTrusted"`
	BootstrapSource       string `json:"bootstrapSource"`
	BootstrapState        string `json:"bootstrapState"`
	BootstrapPlatform     string `json:"bootstrapPlatform,omitempty"`
	BootstrapArchitecture string `json:"bootstrapArchitecture,omitempty"`
	Reason                string `json:"reason,omitempty"`
}

type setupArtifact struct {
	ArtifactID        string `json:"artifactId"`
	Version           string `json:"version"`
	Platform          string `json:"platform"`
	Arch              string `json:"arch"`
	SHA256            string `json:"sha256"`
	Provenance        string `json:"provenance"`
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
}

type setupReviewTarget struct {
	CandidateID     string         `json:"candidateId"`
	Label           string         `json:"label"`
	Address         string         `json:"address"`
	Port            int            `json:"port"`
	AccessID        string         `json:"accessId"`
	AccessLabel     string         `json:"accessLabel"`
	Hostname        string         `json:"hostname,omitempty"`
	Platform        string         `json:"platform,omitempty"`
	Arch            string         `json:"arch,omitempty"`
	HostKeySHA256   string         `json:"hostKeySha256,omitempty"`
	Artifact        *setupArtifact `json:"artifact,omitempty"`
	Status          string         `json:"status"`
	Reason          string         `json:"reason,omitempty"`
	Action          string         `json:"action,omitempty"`
	StartupLifetime string         `json:"startupLifetime"`
	NeedsLinger     bool           `json:"needsLinger"`
}

type setupReview struct {
	ReviewID         string              `json:"reviewId"`
	ExpiresAt        int64               `json:"expiresAt"`
	ControllerNodeID string              `json:"controllerNodeId"`
	TargetClusterID  string              `json:"targetClusterId"`
	Targets          []setupReviewTarget `json:"targets"`
	CanApprove       bool                `json:"canApprove"`
}

type setupTargetState struct {
	CandidateID      string `json:"candidateId"`
	Stage            string `json:"stage"`
	Message          string `json:"message,omitempty"`
	NodeID           string `json:"nodeId,omitempty"`
	CanRetry         bool   `json:"canRetry"`
	CanCancel        bool   `json:"canCancel"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
}

type setupOperation struct {
	OperationID     string             `json:"operationId"`
	ReviewID        string             `json:"reviewId"`
	TargetClusterID string             `json:"targetClusterId"`
	Revision        uint64             `json:"revision"`
	State           string             `json:"state"`
	Targets         []setupTargetState `json:"targets"`
	StartedAt       int64              `json:"startedAt"`
	FinishedAt      int64              `json:"finishedAt,omitempty"`
}

type setupAcceptedHostKey struct {
	CandidateID string `json:"candidateId"`
	SHA256      string `json:"sha256"`
}

// transientSecret is the only mutable holder used after a masked text field is
// submitted. The RPC command clears it on every completion path.
type transientSecret []byte

func (s transientSecret) MarshalJSON() ([]byte, error) { return json.Marshal(string(s)) }

func clearSecret(s transientSecret) {
	for i := range s {
		s[i] = 0
	}
}

type setupAccessRequest struct {
	Purpose           string          `json:"purpose,omitempty"`
	OperationID       string          `json:"operationId,omitempty"`
	CandidateIDs      []string        `json:"candidateIds"`
	Username          string          `json:"username"`
	Auth              string          `json:"auth"`
	KeyPath           string          `json:"keyPath,omitempty"`
	Password          transientSecret `json:"password,omitempty"`
	Passphrase        transientSecret `json:"passphrase,omitempty"`
	ElevationPassword transientSecret `json:"elevationPassword,omitempty"`
	StartupLifetime   string          `json:"startupLifetime,omitempty"`
}

func (r *setupAccessRequest) clear() {
	clearSecret(r.Password)
	clearSecret(r.Passphrase)
	clearSecret(r.ElevationPassword)
	r.Password, r.Passphrase, r.ElevationPassword = nil, nil, nil
}

type setupCandidatesMsg struct {
	candidates []setupCandidate
	artifacts  []setupArtifact
	replace    bool
	err        error
}

type setupAccessMsg struct {
	candidates []setupCandidate
	username   string
	err        error
}

type setupReviewMsg struct {
	review setupReview
	err    error
}

type setupOperationMsg struct {
	action    string
	operation *setupOperation
	err       error
}

type setupCableRetainedMsg struct {
	retained cableprobe.RetainedRuns
	err      error
}

type setupCableReviewMsg struct {
	review cableprobe.Review
	err    error
}

type setupCableRunMsg struct {
	action string
	run    *cableprobe.Run
	err    error
}

type setupCleanupReviewMsg struct {
	review cableprobe.CleanupReview
	err    error
}

type setupFabricPhysicalPort struct {
	Source   string `json:"source"`
	SwitchID string `json:"switchId"`
	PortName string `json:"portName"`
}

type setupFabricInterface struct {
	Name         string                  `json:"name"`
	Index        int                     `json:"index"`
	MAC          string                  `json:"mac"`
	PhysicalPort setupFabricPhysicalPort `json:"physicalPort"`
	Addresses    []string                `json:"addresses"`
	Address      string                  `json:"address"`
	Driver       string                  `json:"driver"`
	RDMADevices  []string                `json:"rdmaDevices"`
	MTU          int                     `json:"mtu"`
}

type setupFabricTarget struct {
	NodeID     string                  `json:"nodeId"`
	Principal  string                  `json:"principal"`
	SwitchID   string                  `json:"switchId,omitempty"`
	PortName   string                  `json:"portName,omitempty"`
	Ports      []setupFabricTargetPort `json:"ports,omitempty"`
	Interfaces []setupFabricInterface  `json:"interfaces"`
}

type setupFabricTargetPort struct {
	SwitchID string `json:"switchId"`
	PortName string `json:"portName"`
}

type setupFabricReview struct {
	SchemaVersion       int                          `json:"schemaVersion"`
	ReviewID            string                       `json:"reviewId"`
	OwnerNodeID         string                       `json:"ownerNodeId"`
	RecipeID            string                       `json:"recipeId"`
	CableRunID          string                       `json:"cableRunId,omitempty"`
	Persistence         string                       `json:"persistence"`
	State               string                       `json:"state"`
	Executable          bool                         `json:"executable"`
	RemainingMs         int64                        `json:"remainingMs"`
	Targets             []setupFabricTarget          `json:"targets"`
	Blockers            []string                     `json:"blockers"`
	EffectsApplied      bool                         `json:"effectsApplied"`
	Permission          *cableprobe.PermissionReview `json:"permission,omitempty"`
	InspectionRequired  bool                         `json:"inspectionRequired,omitempty"`
	InspectionAvailable bool                         `json:"inspectionAvailable,omitempty"`
}

type setupFabricFailure struct {
	NodeID string `json:"nodeId"`
	Phase  string `json:"phase"`
	Code   string `json:"code"`
}

type setupFabricCandidateIP struct {
	NodeID         string `json:"nodeId"`
	PeerNodeID     string `json:"peerNodeId"`
	PeerPrincipal  string `json:"peerPrincipal"`
	Address        string `json:"address"`
	PeerAddress    string `json:"peerAddress"`
	InterfaceName  string `json:"interfaceName"`
	InterfaceIndex int    `json:"interfaceIndex"`
	MAC            string `json:"mac"`
	SwitchID       string `json:"switchId"`
	PortName       string `json:"portName"`
	RDMADevice     string `json:"rdmaDevice"`
	RDMAPort       int    `json:"rdmaPort"`
	GIDIndex       int    `json:"gidIndex"`
	GIDType        string `json:"gidType"`
}

type setupFabricOperation struct {
	Failure             *setupFabricFailure          `json:"failure,omitempty"`
	Permission          *cableprobe.PermissionReview `json:"permission,omitempty"`
	SchemaVersion       int                          `json:"schemaVersion"`
	OperationID         string                       `json:"operationId"`
	ReviewID            string                       `json:"reviewId"`
	OwnerNodeID         string                       `json:"ownerNodeId"`
	RecipeID            string                       `json:"recipeId,omitempty"`
	CableRunID          string                       `json:"cableRunId,omitempty"`
	State               string                       `json:"state"`
	Targets             []setupFabricTarget          `json:"targets"`
	CleanupConfirmed    bool                         `json:"cleanupConfirmed"`
	EffectsApplied      bool                         `json:"effectsApplied"`
	EffectsUnconfirmed  bool                         `json:"effectsUnconfirmed,omitempty"`
	Message             string                       `json:"message"`
	CreatedAt           int64                        `json:"createdAt"`
	ExpiresAt           int64                        `json:"expiresAt"`
	QualifiedAt         int64                        `json:"qualifiedAt,omitempty"`
	QualificationDigest string                       `json:"qualificationDigest,omitempty"`
	CandidateIPs        []setupFabricCandidateIP     `json:"candidateIPs,omitempty"`
}

type setupFabricReviewMsg struct {
	review setupFabricReview
	err    error
}

type setupFabricOperationMsg struct {
	action    string
	operation *setupFabricOperation
	err       error
}

func setupCandidatesCmd(client *rpc.Client) tea.Cmd {
	return call(client, "engine:onboarding-candidates", struct{}{}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCandidatesMsg{err: err}
		}
		var result struct {
			Candidates []setupCandidate `json:"candidates"`
			Artifacts  []setupArtifact  `json:"artifacts"`
		}
		if err := decodeParams(msg.Result, &result); err != nil {
			return setupCandidatesMsg{err: err}
		}
		return setupCandidatesMsg{candidates: result.Candidates, artifacts: result.Artifacts, replace: true}
	})
}

func setupAddTargetCmd(client *rpc.Client, address string, port int) tea.Cmd {
	params := map[string]any{"address": address, "port": port}
	return call(client, "engine:onboarding-add-target", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCandidatesMsg{err: err}
		}
		var candidate setupCandidate
		if err := decodeParams(msg.Result, &candidate); err != nil {
			return setupCandidatesMsg{err: err}
		}
		return setupCandidatesMsg{candidates: []setupCandidate{candidate}}
	})
}

func setupAccessCmd(client *rpc.Client, request setupAccessRequest) tea.Cmd {
	return func() tea.Msg {
		defer request.clear()
		ctx, cancel := context.WithTimeout(context.Background(), setupOperationTimeout)
		defer cancel()
		msg, err := client.Call(ctx, "engine:onboarding-access", request)
		if err != nil {
			return setupAccessMsg{username: request.Username, err: err}
		}
		var result struct {
			Candidates []setupCandidate `json:"candidates"`
		}
		if err := decodeParams(msg.Result, &result); err != nil {
			return setupAccessMsg{username: request.Username, err: err}
		}
		return setupAccessMsg{
			candidates: result.Candidates,
			username:   request.Username,
		}
	}
}

func setupInspectCmd(client *rpc.Client, candidateIDs []string, artifactID string, keys []setupAcceptedHostKey) tea.Cmd {
	params := struct {
		CandidateIDs     []string               `json:"candidateIds"`
		ArtifactID       string                 `json:"artifactId,omitempty"`
		AcceptedHostKeys []setupAcceptedHostKey `json:"acceptedHostKeys,omitempty"`
	}{candidateIDs, artifactID, keys}
	return callWithTimeout(client, setupOperationTimeout, "engine:onboarding-inspect", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupReviewMsg{err: err}
		}
		var review setupReview
		if err := decodeParams(msg.Result, &review); err != nil {
			return setupReviewMsg{err: err}
		}
		return setupReviewMsg{review: review}
	})
}

func setupApproveCmd(client *rpc.Client, review setupReview) tea.Cmd {
	return callWithTimeout(client, setupOperationTimeout, "engine:onboarding-approve", map[string]string{"reviewId": review.ReviewID}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupOperationMsg{action: "setup approval", err: err}
		}
		var operation setupOperation
		if err := decodeParams(msg.Result, &operation); err != nil {
			return setupOperationMsg{action: "setup approval", err: err}
		}
		if operation.ReviewID != review.ReviewID || operation.OperationID == "" {
			return setupOperationMsg{action: "setup approval", err: errors.New("setup approval returned a different operation")}
		}
		return setupOperationMsg{action: "setup approval", operation: &operation}
	})
}

func setupOperationCmd(client *rpc.Client, method, action, operationID string) tea.Cmd {
	params := map[string]string{}
	if operationID != "" {
		params["operationId"] = operationID
	}
	return callWithTimeout(client, setupOperationTimeout, method, params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupOperationMsg{action: action, err: err}
		}
		if string(msg.Result) == "null" || len(msg.Result) == 0 {
			return setupOperationMsg{action: action}
		}
		var operation setupOperation
		if err := decodeParams(msg.Result, &operation); err != nil {
			return setupOperationMsg{action: action, err: err}
		}
		if operationID != "" && operation.OperationID != operationID {
			return setupOperationMsg{action: action, err: errors.New("setup status returned a different operation")}
		}
		return setupOperationMsg{action: action, operation: &operation}
	})
}

func setupOperationByReviewCmd(client *rpc.Client, reviewID string) tea.Cmd {
	return callWithTimeout(client, setupOperationTimeout, "engine:onboarding-status", map[string]string{"reviewId": reviewID}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupOperationMsg{action: "setup reconciliation", err: err}
		}
		if string(msg.Result) == "null" || len(msg.Result) == 0 {
			return setupOperationMsg{action: "setup reconciliation", err: errors.New("setup outcome remains unknown")}
		}
		var operation setupOperation
		if err := decodeParams(msg.Result, &operation); err != nil {
			return setupOperationMsg{action: "setup reconciliation", err: err}
		}
		if operation.ReviewID != reviewID || operation.OperationID == "" {
			return setupOperationMsg{action: "setup reconciliation", err: errors.New("setup reconciliation returned a different operation")}
		}
		return setupOperationMsg{action: "setup reconciliation", operation: &operation}
	})
}

func setupCableRetainedCmd(client *rpc.Client) tea.Cmd {
	return call(client, "engine:cable-retained-runs", struct{}{}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCableRetainedMsg{err: err}
		}
		var retained cableprobe.RetainedRuns
		if err := decodeParams(msg.Result, &retained); err != nil {
			return setupCableRetainedMsg{err: err}
		}
		return setupCableRetainedMsg{retained: retained}
	})
}

func setupCableReviewCmd(client *rpc.Client, selection cableprobe.ReviewRequest, keys []setupAcceptedHostKey) tea.Cmd {
	params := struct {
		cableprobe.ReviewRequest
		AcceptedHostKeys []setupAcceptedHostKey `json:"acceptedHostKeys,omitempty"`
	}{selection, keys}
	return callWithTimeout(client, setupOperationTimeout, "engine:cable-review", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCableReviewMsg{err: err}
		}
		var review cableprobe.Review
		if err := decodeParams(msg.Result, &review); err != nil {
			return setupCableReviewMsg{err: err}
		}
		return setupCableReviewMsg{review: review}
	})
}

func setupCableStartCmd(client *rpc.Client, review cableprobe.Review) tea.Cmd {
	params := cableprobe.StartRequest{ReviewID: review.ReviewID, ApproveAdmin: true}
	return callWithTimeout(client, setupOperationTimeout, "engine:cable-start", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCableRunMsg{action: "cable start", err: err}
		}
		var run cableprobe.Run
		if err := decodeParams(msg.Result, &run); err != nil {
			return setupCableRunMsg{action: "cable start", err: err}
		}
		if run.RunID == "" {
			var refused cableprobe.StartNotStarted
			_ = decodeParams(msg.Result, &refused)
			return setupCableRunMsg{action: "cable start", err: fmt.Errorf("cable did not start: %s", refused.Reason)}
		}
		if run.ReviewID != review.ReviewID {
			return setupCableRunMsg{action: "cable start", err: errors.New("cable start returned a different operation")}
		}
		return setupCableRunMsg{action: "cable start", run: &run}
	})
}

func setupCableStatusCmd(client *rpc.Client, runID string) tea.Cmd {
	return call(client, "engine:cable-status", cableprobe.StatusRequest{RunID: runID}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCableRunMsg{action: "cable status", err: err}
		}
		var run cableprobe.Run
		if err := decodeParams(msg.Result, &run); err != nil {
			return setupCableRunMsg{action: "cable status", err: err}
		}
		if run.RunID != runID {
			return setupCableRunMsg{action: "cable status", err: errors.New("cable status returned a different run")}
		}
		return setupCableRunMsg{action: "cable status", run: &run}
	})
}

func setupCableStatusByReviewCmd(client *rpc.Client, reviewID string) tea.Cmd {
	return call(client, "engine:cable-status", cableprobe.StatusRequest{ReviewID: reviewID}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCableRunMsg{action: "cable reconciliation", err: err}
		}
		var run cableprobe.Run
		if err := decodeParams(msg.Result, &run); err != nil {
			return setupCableRunMsg{action: "cable reconciliation", err: err}
		}
		if run.ReviewID != reviewID || run.RunID == "" {
			return setupCableRunMsg{action: "cable reconciliation", err: errors.New("cable reconciliation returned a different run")}
		}
		return setupCableRunMsg{action: "cable reconciliation", run: &run}
	})
}

func setupCableCancelCmd(client *rpc.Client, runID string, cleanup bool) tea.Cmd {
	method, action := "engine:cable-cancel", "cable cancel"
	if cleanup {
		method, action = "engine:cable-cleanup-cancel", "cleanup cancel"
	}
	return call(client, method, map[string]string{"runId": runID}, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCableRunMsg{action: action, err: err}
		}
		var run cableprobe.Run
		if err := decodeParams(msg.Result, &run); err != nil {
			return setupCableRunMsg{action: action, err: err}
		}
		if run.RunID != runID {
			return setupCableRunMsg{action: action, err: errors.New("cable cancellation returned a different run")}
		}
		return setupCableRunMsg{action: action, run: &run}
	})
}

func setupCleanupReviewCmd(client *rpc.Client, runID string, keys []setupAcceptedHostKey) tea.Cmd {
	params := struct {
		RunID            string                 `json:"runId"`
		AcceptedHostKeys []setupAcceptedHostKey `json:"acceptedHostKeys,omitempty"`
	}{runID, keys}
	return callWithTimeout(client, setupOperationTimeout, "engine:cable-cleanup-review", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCleanupReviewMsg{err: err}
		}
		var review cableprobe.CleanupReview
		if err := decodeParams(msg.Result, &review); err != nil {
			return setupCleanupReviewMsg{err: err}
		}
		if review.RunID != runID {
			return setupCleanupReviewMsg{err: errors.New("cleanup review returned a different run")}
		}
		return setupCleanupReviewMsg{review: review}
	})
}

func setupCleanupVerifyCmd(client *rpc.Client, review cableprobe.CleanupReview) tea.Cmd {
	params := map[string]any{"runId": review.RunID, "reviewId": review.ReviewID, "approveAdmin": true}
	return callWithTimeout(client, setupOperationTimeout, "engine:cable-cleanup-verify", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupCableRunMsg{action: "cleanup verify", err: err}
		}
		var result cableprobe.CleanupVerifyResult
		if err := decodeParams(msg.Result, &result); err != nil {
			return setupCableRunMsg{action: "cleanup verify", err: err}
		}
		if result.ReviewID != review.ReviewID || result.Run.RunID != review.RunID {
			return setupCableRunMsg{action: "cleanup verify", err: errors.New("cleanup verification returned a different operation")}
		}
		if result.Disposition != "accepted" {
			return setupCableRunMsg{action: "cleanup verify", run: &result.Run, err: errors.New("cleanup verification was not started")}
		}
		return setupCableRunMsg{action: "cleanup verify", run: &result.Run}
	})
}

func setupFabricReviewCmd(client *rpc.Client, selection cableprobe.ReviewRequest, keys []setupAcceptedHostKey) tea.Cmd {
	params := struct {
		cableprobe.ReviewRequest
		AcceptedHostKeys        []setupAcceptedHostKey `json:"acceptedHostKeys,omitempty"`
		InspectSelectedProfiles bool                   `json:"inspectSelectedProfiles"`
	}{selection, keys, true}
	return callWithTimeout(client, setupOperationTimeout, "engine:fabric-review", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupFabricReviewMsg{err: err}
		}
		var review setupFabricReview
		if err := decodeParams(msg.Result, &review); err != nil {
			return setupFabricReviewMsg{err: err}
		}
		return setupFabricReviewMsg{review: review}
	})
}

func setupFabricApproveCmd(client *rpc.Client, review setupFabricReview) tea.Cmd {
	params := map[string]any{"reviewId": review.ReviewID, "administratorApproved": true, "selectedPortPauseApproved": true}
	return callWithTimeout(client, setupOperationTimeout, "engine:fabric-approve", params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupFabricOperationMsg{action: "fabric approval", err: err}
		}
		var operation setupFabricOperation
		if err := decodeParams(msg.Result, &operation); err != nil {
			return setupFabricOperationMsg{action: "fabric approval", err: err}
		}
		if operation.OperationID != review.ReviewID || operation.ReviewID != review.ReviewID {
			return setupFabricOperationMsg{action: "fabric approval", err: errors.New("fabric approval returned a different operation")}
		}
		return setupFabricOperationMsg{action: "fabric approval", operation: &operation}
	})
}

func setupFabricOperationCmd(client *rpc.Client, method, action, operationID string) tea.Cmd {
	params := map[string]any{"operationId": operationID}
	if method == "engine:fabric-cancel" || method == "engine:fabric-recover" {
		params["administratorApproved"] = true
	}
	return callWithTimeout(client, setupOperationTimeout, method, params, func(msg *rpc.Message, err error) tea.Msg {
		if err != nil {
			return setupFabricOperationMsg{action: action, err: err}
		}
		var operation setupFabricOperation
		if err := decodeParams(msg.Result, &operation); err != nil {
			return setupFabricOperationMsg{action: action, err: err}
		}
		if operation.OperationID != operationID {
			return setupFabricOperationMsg{action: action, err: errors.New("fabric response returned a different operation")}
		}
		return setupFabricOperationMsg{action: action, operation: &operation}
	})
}
