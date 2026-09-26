// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"reflect"

	"nvpair-shared/cableprobe"
)

type cableFactsReadStatus string

func validCableFactsReadStatus(status string) bool {
	switch status {
	case "unavailable", "invalid", "stale", "expired", "binding-changed", "port-unavailable":
		return true
	}
	return false
}

type cableFactsReadError struct {
	status cableFactsReadStatus
	cause  error
}

func (e *cableFactsReadError) Error() string { return e.cause.Error() }
func (e *cableFactsReadError) Unwrap() error { return e.cause }
func classifyCableFactsRead(status cableFactsReadStatus, err error) error {
	return &cableFactsReadError{status: status, cause: err}
}
func cableFactsReadClassification(err error) cableFactsReadStatus {
	if errors.Is(err, errCableFactsStale) {
		return "stale"
	}
	var classified *cableFactsReadError
	if errors.As(err, &classified) {
		return classified.status
	}
	return "unavailable"
}

func validCableFactsDifference(d *cableprobe.FactsDifference, targets []cableprobe.Target) bool {
	if d == nil {
		return true
	}
	if d.TargetIndex == nil || *d.TargetIndex < 0 || *d.TargetIndex >= len(targets) || *d.TargetIndex > 2 {
		return false
	}
	if d.Field == "read" {
		return validCableFactsReadStatus(d.ReadStatus)
	}
	if d.ReadStatus != "" {
		return false
	}
	switch d.Field {
	case "target-node-id", "principal", "ports", "switch-id", "port-name", "interfaces", "interface-name", "interface-index", "interface-mac":
		return true
	}
	return false
}

func cloneCableFactsDifference(d *cableprobe.FactsDifference) *cableprobe.FactsDifference {
	if d == nil {
		return nil
	}
	copy := *d
	if d.TargetIndex != nil {
		index := *d.TargetIndex
		copy.TargetIndex = &index
	}
	return &copy
}

// This explains an existing failed DeepEqual; it never decides admission or
// changes ordering, normalization, slice semantics, freshness or identity gates.
func firstCableFactsDifference(approved, actual []cableprobe.Target, statuses []cableFactsReadStatus) *cableprobe.FactsDifference {
	for i, want := range approved {
		index := i
		difference := func(field string) *cableprobe.FactsDifference {
			return &cableprobe.FactsDifference{TargetIndex: &index, Field: field}
		}
		if i >= len(actual) {
			d := difference("read")
			d.ReadStatus = "unavailable"
			return d
		}
		got := actual[i]
		if reflect.DeepEqual(cableProbeTargetFacts([]cableprobe.Target{want}), cableProbeTargetFacts([]cableprobe.Target{got})) {
			continue
		}
		if i < len(statuses) && validCableFactsReadStatus(string(statuses[i])) {
			d := difference("read")
			d.ReadStatus = string(statuses[i])
			return d
		}
		if want.NodeID != got.NodeID {
			return difference("target-node-id")
		}
		if want.Principal != got.Principal {
			return difference("principal")
		}
		if (want.Ports == nil) != (got.Ports == nil) || len(want.Ports) != len(got.Ports) {
			return difference("ports")
		}
		for p, port := range want.Ports {
			other := got.Ports[p]
			if port.SwitchID != other.SwitchID {
				return difference("switch-id")
			}
			if port.PortName != other.PortName {
				return difference("port-name")
			}
			if (port.Interfaces == nil) != (other.Interfaces == nil) || len(port.Interfaces) != len(other.Interfaces) {
				return difference("interfaces")
			}
			for a, alias := range port.Interfaces {
				current := other.Interfaces[a]
				if alias.Name != current.Name {
					return difference("interface-name")
				}
				if alias.Index != current.Index {
					return difference("interface-index")
				}
				if alias.MAC != current.MAC {
					return difference("interface-mac")
				}
			}
		}
	}
	// An unexpected extra unapproved target cannot be attributed to an approved
	// target index. The original mismatch still fails with no invented detail.
	return nil
}
