// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cableprobe contains only the fixed-purpose, finite LLDP probe contract
// and primitive. Its run marker is correlation, never a peer authenticator.
package cableprobe

import "time"

const ReviewLifetime = 30 * time.Second
const Window = 20 * time.Second
const Freshness = 2 * time.Second

type PortRef struct {
	NodeID   string `json:"nodeId"`
	SwitchID string `json:"switchId"`
	PortName string `json:"portName"`
}
type ReviewRequest struct {
	NodeIDs []string  `json:"nodeIds"`
	Ports   []PortRef `json:"ports"`
}
type Interface struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
	MAC   string `json:"mac"`
}
type Port struct {
	SwitchID   string      `json:"switchId"`
	PortName   string      `json:"portName"`
	Interfaces []Interface `json:"interfaces"`
}
type Target struct {
	NodeID       string `json:"nodeId"`
	Principal    string `json:"principal"`
	Ports        []Port `json:"ports"`
	RawPrivilege string `json:"rawPrivilege"` // present, approval-needed, unsupported, unknown
	Reason       string `json:"reason,omitempty"`
}
type Review struct {
	ReviewID      string            `json:"reviewId"`
	OwnerNodeID   string            `json:"ownerNodeId"`
	Targets       []Target          `json:"targets"`
	Available     bool              `json:"available"`
	Reason        string            `json:"reason,omitempty"`
	RemainingMs   int64             `json:"remainingMs"`
	ConsumedRunID string            `json:"consumedRunId,omitempty"`
	Permission    *PermissionReview `json:"permission,omitempty"`
}
type PermissionTarget struct {
	NodeID             string `json:"nodeId"`
	CandidateID        string `json:"candidateId"`
	AccessLabel        string `json:"accessLabel"`
	HostKeySHA256      string `json:"hostKeySha256,omitempty"`
	HostKeyTrusted     bool   `json:"hostKeyTrusted"`
	AccessAvailable    bool   `json:"accessAvailable"`
	ElevationAvailable bool   `json:"elevationAvailable"`
	WorkerAvailable    bool   `json:"workerAvailable"`
	WorkerSHA256       string `json:"workerSha256,omitempty"`
	Reason             string `json:"reason,omitempty"`
}
type PermissionReview struct {
	Mode    string             `json:"mode"`
	Targets []PermissionTarget `json:"targets"`
	Effects []string           `json:"effects"`
}
type StartRequest struct {
	ReviewID     string `json:"reviewId"`
	ApproveAdmin bool   `json:"approveAdmin,omitempty"`
}

// StartNotStarted is returned only after serialized admission retires a known
// review before creating an operation. Missing history is not this disposition.
// Accepted starts retain the existing Run response shape.
type StartNotStarted struct {
	Disposition string `json:"disposition"`
	ReviewID    string `json:"reviewId"`
	OwnerNodeID string `json:"ownerNodeId"`
	Reason      string `json:"reason"` // review-expired, review-unavailable, trust-changed, access-changed
}

// Status permits exactly one ID, so a lost start reply can be recovered without
// reminting a run. Cancel requires RunID, never a host/path/packet override.
type StatusRequest struct {
	RunID    string `json:"runId,omitempty"`
	ReviewID string `json:"reviewId,omitempty"`
}

// Retained discovery carries only bounded historical identity/scope summaries.
// It is not a review, execution approval or new cable observation.
type RetainedRun struct {
	RunID            string    `json:"runId"`
	ReviewID         string    `json:"reviewId"`
	OwnerNodeID      string    `json:"ownerNodeId"`
	Revision         uint64    `json:"revision"`
	State            string    `json:"state"`
	CleanupConfirmed bool      `json:"cleanupConfirmed"`
	StartedAt        int64     `json:"startedAt"`
	NodeIDs          []string  `json:"nodeIds"`
	Ports            []PortRef `json:"ports"`
}

type RetainedRuns struct {
	OwnerNodeID string        `json:"ownerNodeId"`
	Held        bool          `json:"held"`
	Limited     bool          `json:"limited"`
	Reason      string        `json:"reason,omitempty"`
	Runs        []RetainedRun `json:"runs"`
}
type Edge struct {
	Left  PortRef `json:"left"`
	Right PortRef `json:"right"`
	AgeMs int64   `json:"ageMs"`
	Fresh bool    `json:"fresh"`
}
type Topology struct {
	Layout string `json:"layout"`
	Status string `json:"status"`
}
type Run struct {
	RunID                string           `json:"runId"`
	ReviewID             string           `json:"reviewId"`
	OwnerNodeID          string           `json:"ownerNodeId"`
	Revision             uint64           `json:"revision"`
	State                string           `json:"state"` // preparing, running, cancelling, completed, cancelled, failed
	Targets              []Target         `json:"targets"`
	Edges                []Edge           `json:"edges"`
	Result               string           `json:"result"`     // unavailable, incomplete, reciprocal-observations, ambiguous
	Directness           string           `json:"directness"` // always unverified; nonconformant bridging is not excluded
	RemainingMs          int64            `json:"remainingMs"`
	FreshnessRemainingMs int64            `json:"freshnessRemainingMs"`
	CleanupConfirmed     bool             `json:"cleanupConfirmed"`
	StartedAt            int64            `json:"startedAt"`
	FinishedAt           int64            `json:"finishedAt,omitempty"`
	Message              string           `json:"message"`
	Diagnostics          *RunDiagnostics  `json:"diagnostics,omitempty"`
	CleanupRecovery      *CleanupRecovery `json:"cleanupRecovery,omitempty"`
	Topology             *Topology        `json:"topology,omitempty"`
}

type CleanupReviewTarget struct {
	NodeID             string `json:"nodeId"`
	CandidateID        string `json:"candidateId"`
	AccessLabel        string `json:"accessLabel"`
	HostKeySHA256      string `json:"hostKeySha256,omitempty"`
	HostKeyTrusted     bool   `json:"hostKeyTrusted"`
	AccessAvailable    bool   `json:"accessAvailable"`
	ElevationAvailable bool   `json:"elevationAvailable"`
	InspectorAvailable bool   `json:"inspectorAvailable"`
	Reason             string `json:"reason,omitempty"`
}

type CleanupReview struct {
	ReviewID    string                `json:"reviewId"`
	RunID       string                `json:"runId"`
	Available   bool                  `json:"available"`
	RemainingMs int64                 `json:"remainingMs"`
	Reason      string                `json:"reason,omitempty"`
	Effects     []string              `json:"effects"`
	Targets     []CleanupReviewTarget `json:"targets"`
}

type CleanupRecoveryTarget struct {
	NodeID string `json:"nodeId"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type CleanupRecovery struct {
	AttemptID    string                  `json:"attemptId"`
	ReviewID     string                  `json:"reviewId"`
	Revision     uint64                  `json:"revision"`
	State        string                  `json:"state"`
	HoldReleased bool                    `json:"holdReleased"`
	Code         string                  `json:"code"`
	Message      string                  `json:"message"`
	StartedAt    int64                   `json:"startedAt"`
	FinishedAt   int64                   `json:"finishedAt,omitempty"`
	Targets      []CleanupRecoveryTarget `json:"targets"`
}

// Diagnostics contain only fixed public codes and bounded reported counters.
// They never contain command output, credentials, or arbitrary worker text.
type Failure struct {
	Phase string `json:"phase"`
	Code  string `json:"code"`
}

type ParticipantDiagnostic struct {
	NodeID                     string           `json:"nodeId"`
	Phase                      string           `json:"phase"`
	Code                       string           `json:"code,omitempty"`
	WorkerState                string           `json:"workerState,omitempty"`
	Sent                       *int             `json:"sent,omitempty"`
	Received                   *int             `json:"received,omitempty"`
	CleanupConfirmed           bool             `json:"cleanupConfirmed"`
	FinalValidationCode        string           `json:"finalValidationCode,omitempty"`
	PreparationResourceOutcome string           `json:"preparationResourceOutcome,omitempty"`
	FactsDifference            *FactsDifference `json:"factsDifference,omitempty"`
}

// Only the approved target position and a fixed field/read category are public.
// No observed values, error text, addresses, hashes or new approval are carried.
type FactsDifference struct {
	TargetIndex *int   `json:"targetIndex"`
	Field       string `json:"field"`
	ReadStatus  string `json:"readStatus,omitempty"`
}

type RunDiagnostics struct {
	Failure      *Failure                `json:"failure,omitempty"`
	Participants []ParticipantDiagnostic `json:"participants"`
}

// This acknowledgement concerns admission only. It cannot establish cleanup or
// release the original operation's hold.
type CleanupVerifyResult struct {
	Disposition string `json:"disposition"`
	ReviewID    string `json:"reviewId"`
	Run         Run    `json:"run"`
}
