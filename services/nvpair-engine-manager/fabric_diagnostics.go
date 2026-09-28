// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
)

const fabricWorkerProtocol = "pair-fabric-address/4"

// Only the fixed code is public. Native and transport details remain private.
type fabricInspectError struct {
	Code  string
	cause error
}

func (e *fabricInspectError) Error() string { return e.Code }
func (e *fabricInspectError) Unwrap() error { return e.cause }

func validFabricFailureCode(code string) bool {
	switch code {
	case "request-invalid", "access-unavailable", "ssh-unavailable", "worker-changed", "transport-unavailable", "result-invalid", "inventory-unavailable", "identity-mismatch", "facts-changed", "network-manager-unavailable", "network-manager-query-failed", "network-manager-bus-unavailable", "network-manager-owner-unavailable", "network-manager-device-unavailable", "network-manager-profiles-unavailable", "network-manager-generation-changed", "route-query-failed", "route-data-invalid", "rule-query-failed", "address-inventory-unavailable", "configuration-unavailable", "configuration-blocked", "dns-unavailable", "interface-changed", "cancelled", "deadline-exceeded", "native-inspect-failed":
		return true
	}
	return false
}

func fabricInspectionError(code string, err error) error {
	if err == nil {
		return nil
	}
	var first *fabricInspectError
	if errors.As(err, &first) && validFabricFailureCode(first.Code) {
		return first
	}
	if !validFabricFailureCode(code) {
		code = "native-inspect-failed"
	}
	if errors.Is(err, context.Canceled) {
		code = "cancelled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = "deadline-exceeded"
	}
	return &fabricInspectError{Code: code, cause: err}
}
