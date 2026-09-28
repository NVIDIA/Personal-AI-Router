// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"

	"nvpair-shared/cableprobe"
)

func cableDiagnosticPhase(value string) bool {
	switch value {
	case "preparing", "access", "routing", "trust", "ssh", "launch", "arm", "barrier", "start", "observation", "final-validation", "cleanup", "retention", "completed":
		return true
	}
	return false
}

func cableDiagnosticCode(value string) bool {
	if validCablePrearmCode(value) {
		return true
	}
	switch value {
	case "cancelled", "deadline-exceeded", "random-source-unavailable", "access-unavailable", "participant-unavailable", "trust-changed", "ssh-unavailable", "launch-failed", "arm-invalid", "review-expired", "arm-expired", "start-unconfirmed", "worker-output-invalid", "worker-failed", "worker-cancelled", "session-unavailable", "admission-expired", "receive-budget-exhausted", "current-revalidation-failed", "transmit-failed", "receive-failed", "edge-budget-exhausted", "final-revalidation-failed", "facts-stale", "cleanup-unconfirmed", "receipt-write-failed":
		return true
	}
	return false
}

func cableFailure(phase, fallback string, err error) *cableprobe.Failure {
	if !cableDiagnosticPhase(phase) {
		phase = "preparing"
	}
	if !cableDiagnosticCode(fallback) {
		fallback = "worker-failed"
	}
	code := fallback
	var outputFailure cableWorkerOutputError
	if errors.As(err, &outputFailure) {
		code = "worker-output-invalid"
	} else if errors.Is(err, context.Canceled) {
		code = "cancelled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "deadline-exceeded"
	} else if errors.Is(err, errCableFactsStale) {
		code = "facts-stale"
	}
	return &cableprobe.Failure{Phase: phase, Code: code}
}

func latchCableFailure(row *cableprobe.ParticipantDiagnostic, phase, code string, err error) {
	if row.Code != "" {
		return
	}
	failure := cableFailure(phase, code, err)
	row.Phase, row.Code = failure.Phase, failure.Code
}

// These messages are fixed by cableProbeSession.Run. Unknown remote text is
// deliberately reduced to worker-failed rather than retained or displayed.
func cableWorkerFailure(message cableWorkerMessage) *cableprobe.Failure {
	if message.State == "completed" {
		return nil
	}
	if message.State == "cancelled" {
		return &cableprobe.Failure{Phase: "observation", Code: "worker-cancelled"}
	}
	phase, code := "observation", "worker-failed"
	switch message.Message {
	case "Cable observation cancelled.", "Prepared cable worker cancelled before transmission.":
		code = "worker-cancelled"
	case "Cable session is already used or closed.":
		phase, code = "start", "session-unavailable"
	case "Cable admission expired before start.":
		phase, code = "start", "admission-expired"
	case "Cable receive budget exhausted.":
		code = "receive-budget-exhausted"
	case "Reviewed identity or native ports changed during the cable observation.":
		code = "current-revalidation-failed"
	case "Reviewed native port observations remained stale.":
		code = "facts-stale"
	case "Fixed-profile cable transmission failed.":
		code = "transmit-failed"
	case "Cable receive failed.":
		code = "receive-failed"
	case "Cable observation is ambiguous within its edge bound.":
		code = "edge-budget-exhausted"
	case "Final reviewed identity or ports changed; observations discarded.":
		phase, code = "final-validation", "final-revalidation-failed"
	case "Final native port observations remained stale; observations discarded.":
		phase, code = "final-validation", "facts-stale"
	case "Cable descriptor cleanup is not confirmed.", "Prepared cable worker cleanup is not confirmed.":
		phase, code = "cleanup", "cleanup-unconfirmed"
	}
	return &cableprobe.Failure{Phase: phase, Code: code}
}

func validCableFinalValidation(code, state string) bool {
	return code == "" || (state == "failed" && (code == "facts-stale" || code == "final-revalidation-failed"))
}

func captureCableTerminal(row *cableprobe.ParticipantDiagnostic, message cableWorkerMessage, runID, reviewID string) {
	if row.WorkerState != "" || message.RunID != runID || message.ReviewID != reviewID ||
		(message.State != "completed" && message.State != "cancelled" && message.State != "failed") ||
		!validCableFinalValidation(message.FinalValidationCode, message.State) || (message.FinalValidationCode != "" && len(message.Observations) != 0) ||
		message.Sent < 0 || message.Sent > 40 || message.Received < 0 || message.Received > cableProbeReceiveLimit {
		return
	}
	row.WorkerState = message.State
	row.FinalValidationCode = message.FinalValidationCode
	sent, received := message.Sent, message.Received
	row.Sent, row.Received = &sent, &received
	if failure := cableWorkerFailure(message); failure != nil {
		latchCableFailure(row, failure.Phase, failure.Code, nil)
	}
}

func validCableDiagnostics(run cableprobe.Run) bool {
	diagnostics := run.Diagnostics
	if diagnostics == nil {
		return true // Existing retained records predate this optional projection.
	}
	if len(diagnostics.Participants) != len(run.Targets) || len(diagnostics.Participants) > 3 {
		return false
	}
	if failure := diagnostics.Failure; failure != nil && (!cableDiagnosticPhase(failure.Phase) || !cableDiagnosticCode(failure.Code)) {
		return false
	}
	if failure := diagnostics.Failure; failure != nil && validCablePrearmCode(failure.Code) && failure.Phase != "arm" {
		return false
	}
	for i, row := range diagnostics.Participants {
		if row.NodeID != run.Targets[i].NodeID || !cableDiagnosticPhase(row.Phase) || (row.Code != "" && !cableDiagnosticCode(row.Code)) {
			return false
		}
		if !validCableFinalValidation(row.FinalValidationCode, row.WorkerState) {
			return false
		}
		prearm := validCablePrearmCode(row.Code)
		if row.FactsDifference != nil && (row.Code != "prepare-facts-mismatch" || row.PreparationResourceOutcome != "not-attempted" || !validCableFactsDifference(row.FactsDifference, run.Targets)) {
			return false
		}
		if row.PreparationResourceOutcome != "" && (!prearm || !validCablePrearmResourceOutcome(row.PreparationResourceOutcome)) {
			return false
		}
		if prearm && (row.Phase != "arm" || row.WorkerState != "failed" || row.Sent == nil || row.Received == nil || *row.Sent != 0 || *row.Received != 0 || row.CleanupConfirmed || run.CleanupConfirmed || row.FinalValidationCode != "") {
			return false
		}
		if row.WorkerState != "" && row.WorkerState != "completed" && row.WorkerState != "cancelled" && row.WorkerState != "failed" {
			return false
		}
		if (row.Sent == nil) != (row.Received == nil) {
			return false
		}
		if row.Sent != nil && (row.WorkerState == "" || *row.Sent < 0 || *row.Sent > 40 || *row.Received < 0 || *row.Received > cableProbeReceiveLimit) {
			return false
		}
	}
	return true
}
