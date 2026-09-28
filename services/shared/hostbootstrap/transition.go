// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

import "reflect"

// ValidateTransition accepts only the next legal immutable operation status.
func ValidateTransition(previous, next Status) error {
	if previous.Validate() != nil || next.Validate() != nil {
		return ErrTransition
	}
	if previous.OperationID != next.OperationID || previous.Binding != next.Binding {
		return ErrTransition
	}
	if previous.Phase == PhaseBlocked || previous.Phase == PhaseComplete {
		return ErrTransition
	}
	if next.Phase == PhaseBlocked {
		if previous.Phase != PhaseReview || previous.Decision != DecisionRefuseForeign {
			return ErrTransition
		}
		if !observationsEqual(previous.Observed, next.Observed) {
			return ErrTransition
		}
		return nil
	}

	switch previous.Phase {
	case PhaseInspect:
		if next.Phase == PhaseReview && observationsEqual(previous.Observed, next.Observed) {
			return nil
		}
	case PhaseReview:
		if previous.Decision == DecisionNoOp &&
			next.Phase == PhaseVerify &&
			next.Decision == previous.Decision &&
			observationsEqual(previous.Observed, next.Observed) {
			return nil
		}
		if (previous.Decision == DecisionApply || previous.Decision == DecisionRepairOwned) &&
			next.Phase == PhaseApply &&
			next.Decision == previous.Decision &&
			observationsEqual(previous.Observed, next.Observed) {
			return nil
		}
	case PhaseApply:
		if next.Phase == PhaseVerify && next.Decision == previous.Decision {
			return nil
		}
	case PhaseVerify:
		if next.Phase == PhaseComplete &&
			next.Decision == previous.Decision &&
			observationsEqual(previous.Observed, next.Observed) {
			return nil
		}
	}
	return ErrTransition
}

func observationsEqual(left, right Observations) bool {
	return reflect.DeepEqual(left, right)
}
