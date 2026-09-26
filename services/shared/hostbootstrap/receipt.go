// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostbootstrap

// NewReceipt seals an exact verified snapshot for a reviewed non-refusal plan.
func NewReceipt(plan Plan, verified Observations) (Receipt, error) {
	if err := plan.Validate(); err != nil {
		return Receipt{}, err
	}
	if plan.Phase != PhaseReview || plan.Decision == DecisionRefuseForeign {
		return Receipt{}, ErrInvalid
	}
	if err := verified.Validate(plan.Binding.Target); err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{
		SchemaVersion: SchemaVersion,
		OperationID:   plan.OperationID,
		Phase:         PhaseComplete,
		Decision:      plan.Decision,
		Binding:       plan.Binding,
		Verified:      verified,
	}
	if err := receipt.Validate(); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
