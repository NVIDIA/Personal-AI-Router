// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"

	"nvpair-shared/cableprobe"
)

const cableWorkerProtocol = "pair-cable-worker/4"

// This terminal reports preparation failure, never a cleanup acknowledgement.
// The eight base fields are mandatory. Optional facts detail contains only
// approved positions and fixed categories, never remote free-text values.
type cablePrearmMessage struct {
	State            string                      `json:"state"`
	RunID            string                      `json:"runId"`
	ReviewID         string                      `json:"reviewId"`
	Code             string                      `json:"code"`
	ResourceOutcome  string                      `json:"resourceOutcome"`
	CleanupConfirmed bool                        `json:"cleanupConfirmed"`
	Sent             int                         `json:"sent"`
	Received         int                         `json:"received"`
	FactsDifference  *cableprobe.FactsDifference `json:"factsDifference,omitempty"`
}

func validCablePrearmCode(code string) bool {
	switch code {
	case "prepare-pairing-unavailable", "prepare-review-unavailable", "prepare-facts-mismatch", "prepare-identity-mismatch", "prepare-admission-expired", "prepare-scope-invalid", "prepare-current-invalid", "prepare-reservation-failed", "prepare-open-failed", "prepare-configure-failed", "prepare-cancelled", "prepare-deadline-exceeded", "prepare-unknown":
		return true
	}
	return false
}

func validCablePrearmResourceOutcome(outcome string) bool {
	switch outcome {
	case "not-attempted", "rollback-confirmed", "rollback-unconfirmed", "unknown":
		return true
	}
	return false
}

// Lower preparation errors cannot produce wire evidence until the caller has
// successfully bound the request to this actual worker's local owner.
type cablePreparationError struct {
	Code            string
	ResourceOutcome string
	cause           error
	requestBound    bool
	FactsDifference *cableprobe.FactsDifference
}

func (e *cablePreparationError) Error() string { return e.Code }
func (e *cablePreparationError) Unwrap() error { return e.cause }

func newCablePreparationError(code, outcome string, cause error) *cablePreparationError {
	if !validCablePrearmCode(code) {
		code = "prepare-unknown"
	}
	if !validCablePrearmResourceOutcome(outcome) {
		outcome = "unknown"
	}
	if errors.Is(cause, context.Canceled) {
		code = "prepare-cancelled"
	} else if errors.Is(cause, context.DeadlineExceeded) {
		code = "prepare-deadline-exceeded"
	}
	return &cablePreparationError{Code: code, ResourceOutcome: outcome, cause: cause}
}

// Called only after bindCableProbeRequest succeeds. Preserve a typed first
// cause; an otherwise unclassified post-binding failure remains conservative.
func boundCablePreparationError(err error) error {
	var failure *cablePreparationError
	if errors.As(err, &failure) {
		bound := *failure
		bound.FactsDifference = cloneCableFactsDifference(failure.FactsDifference)
		bound.requestBound = true
		return &bound
	}
	bound := newCablePreparationError("prepare-unknown", "unknown", err)
	bound.requestBound = true
	return bound
}
