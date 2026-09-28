// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// MaxHelperFrameBytes is the fixed upper bound for one local helper frame.
const MaxHelperFrameBytes = 64 << 10

var (
	// ErrHelperProtocol reports a malformed or inconsistent helper document.
	ErrHelperProtocol = errors.New("invalid helper protocol document")
	// ErrHelperFrameTooLarge reports a helper frame above the fixed wire bound.
	ErrHelperFrameTooLarge = errors.New("helper frame exceeds its fixed limit")
)

var helperNodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// HelperAction is the closed action set accepted by nvpair-host-helper.
type HelperAction string

const (
	HelperActionInspect       HelperAction = "inspect"
	HelperActionApply         HelperAction = "apply"
	HelperActionVerify        HelperAction = "verify"
	HelperActionRankReconcile HelperAction = "rank-reconcile"
)

// RankReconcileRequest is transport-only rank ownership metadata.
type RankReconcileRequest struct {
	RunID      string `json:"runId"`
	Generation uint64 `json:"generation"`
	Rank       int    `json:"rank"`
	PlanDigest string `json:"planDigest"`
	NodeID     string `json:"nodeId"`
}

// HelperRequest selects one fixed target-local helper action.
type HelperRequest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	OperationID   string                `json:"operationId"`
	Action        HelperAction          `json:"action"`
	RankReconcile *RankReconcileRequest `json:"rankReconcile,omitempty"`
}

// HelperResponse carries only a result produced by the target helper.
type HelperResponse struct {
	SchemaVersion int          `json:"schemaVersion"`
	OperationID   string       `json:"operationId,omitempty"`
	Action        HelperAction `json:"action,omitempty"`
	Accepted      bool         `json:"accepted"`
	Status        *Status      `json:"status,omitempty"`
	Receipt       *Receipt     `json:"receipt,omitempty"`
	Reason        string       `json:"reason,omitempty"`
}

// Validate checks a helper request without performing an action.
func (request HelperRequest) Validate() error {
	if request.SchemaVersion != SchemaVersion ||
		!operationIDPattern.MatchString(request.OperationID) {
		return ErrHelperProtocol
	}
	switch request.Action {
	case HelperActionInspect, HelperActionApply, HelperActionVerify:
		if request.RankReconcile != nil {
			return ErrHelperProtocol
		}
	case HelperActionRankReconcile:
		if request.RankReconcile == nil ||
			request.RankReconcile.validate() != nil {
			return ErrHelperProtocol
		}
	default:
		return ErrHelperProtocol
	}
	return nil
}

func (request RankReconcileRequest) validate() error {
	if !operationIDPattern.MatchString(request.RunID) ||
		request.Generation == 0 ||
		request.Rank < 0 ||
		!sha256Pattern.MatchString(request.PlanDigest) ||
		!helperNodeIDPattern.MatchString(request.NodeID) {
		return ErrHelperProtocol
	}
	return nil
}

// Validate checks response correlation and action-specific target results.
func (response HelperResponse) Validate() error {
	if response.SchemaVersion != SchemaVersion {
		return ErrHelperProtocol
	}
	if !response.Accepted {
		if response.OperationID != "" &&
			!operationIDPattern.MatchString(response.OperationID) {
			return ErrHelperProtocol
		}
		if response.Action != "" && !validHelperAction(response.Action) {
			return ErrHelperProtocol
		}
		if response.Status != nil || response.Receipt != nil ||
			!validHelperReason(response.Reason) {
			return ErrHelperProtocol
		}
		return nil
	}
	if !operationIDPattern.MatchString(response.OperationID) ||
		!validHelperAction(response.Action) ||
		response.Reason != "" {
		return ErrHelperProtocol
	}
	switch response.Action {
	case HelperActionInspect:
		if response.Status == nil || response.Receipt != nil ||
			response.Status.OperationID != response.OperationID ||
			response.Status.Phase != PhaseInspect ||
			response.Status.Validate() != nil {
			return ErrHelperProtocol
		}
	case HelperActionApply:
		if response.Status == nil || response.Receipt != nil ||
			response.Status.OperationID != response.OperationID ||
			(response.Status.Phase != PhaseApply &&
				response.Status.Phase != PhaseVerify) ||
			response.Status.Validate() != nil {
			return ErrHelperProtocol
		}
	case HelperActionVerify:
		if response.Status != nil || response.Receipt == nil ||
			response.Receipt.OperationID != response.OperationID ||
			response.Receipt.Validate() != nil {
			return ErrHelperProtocol
		}
	case HelperActionRankReconcile:
		if response.Status != nil || response.Receipt != nil {
			return ErrHelperProtocol
		}
	default:
		return ErrHelperProtocol
	}
	return nil
}

func validHelperAction(action HelperAction) bool {
	switch action {
	case HelperActionInspect, HelperActionApply, HelperActionVerify,
		HelperActionRankReconcile:
		return true
	default:
		return false
	}
}

func validHelperReason(reason string) bool {
	if reason == "" || len(reason) > 256 || !utf8.ValidString(reason) ||
		containsPrivateKey(reason) {
		return false
	}
	for _, character := range reason {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

// EncodeHelperRequest validates and encodes one canonical helper request.
func EncodeHelperRequest(request HelperRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

// DecodeHelperRequest strictly decodes one bounded helper request.
func DecodeHelperRequest(raw []byte) (HelperRequest, error) {
	request, err := decodeStrict(raw, HelperRequest.Validate)
	if err != nil || !helperRequestFieldsPresent(raw, request) {
		return HelperRequest{}, ErrHelperProtocol
	}
	return request, nil
}

func helperRequestFieldsPresent(raw []byte, request HelperRequest) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil ||
		len(fields) < 3 ||
		fields["schemaVersion"] == nil ||
		fields["operationId"] == nil ||
		fields["action"] == nil {
		return false
	}
	if request.Action != HelperActionRankReconcile {
		return len(fields) == 3
	}
	var rankFields map[string]json.RawMessage
	if json.Unmarshal(fields["rankReconcile"], &rankFields) != nil ||
		len(fields) != 4 ||
		len(rankFields) != 5 {
		return false
	}
	for _, field := range []string{
		"runId", "generation", "rank", "planDigest", "nodeId",
	} {
		if rankFields[field] == nil {
			return false
		}
	}
	return true
}

// EncodeHelperResponse validates and encodes one canonical helper response.
func EncodeHelperResponse(response HelperResponse) ([]byte, error) {
	if err := response.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

// DecodeHelperResponse strictly decodes one bounded helper response.
func DecodeHelperResponse(raw []byte) (HelperResponse, error) {
	response, err := decodeStrict(raw, HelperResponse.Validate)
	if err != nil {
		return HelperResponse{}, ErrHelperProtocol
	}
	return response, nil
}

// ReadHelperFrame reads one big-endian length-prefixed bounded helper frame.
func ReadHelperFrame(reader io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header)
	if size == 0 {
		return nil, ErrHelperProtocol
	}
	if size > MaxHelperFrameBytes {
		return nil, ErrHelperFrameTooLarge
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// WriteHelperFrame writes one big-endian length-prefixed bounded helper frame.
func WriteHelperFrame(writer io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return ErrHelperProtocol
	}
	if len(payload) > MaxHelperFrameBytes {
		return ErrHelperFrameTooLarge
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}
