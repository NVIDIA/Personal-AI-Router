// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { parseOnboardingHistory } from '@/shared/utils/onboarding-history'

const valid = {
    total: 2,
    history_only: 1,
    current: 1,
    invalid: 0,
    recovery_required: false,
    diagnostic_recovery_required: false,
    discovery_blocked: false,
    mutation_supported: false,
    operations: [
        {
            operation_id: 'a'.repeat(32),
            state: 'completed',
            classification: 'history-only',
            target_count: 2,
            finished_at: 1,
            mutation_allowed: false
        },
        {
            operation_id: 'b'.repeat(32),
            state: 'completed',
            classification: 'current',
            target_count: 1,
            mutation_allowed: false
        }
    ]
}

describe('parseOnboardingHistory', () => {
    it('normalizes the read-only Engine Manager response', () => {
        const parsed = parseOnboardingHistory(valid)
        expect(parsed.historyOnly).toBe(1)
        expect(parsed.diagnosticRecoveryRequired).toBe(false)
        expect(parsed.operations[0].mutationAllowed).toBe(false)
    })

    it('rejects mutation authority and unreconciled counts', () => {
        expect(() => parseOnboardingHistory({ ...valid, mutation_supported: true })).toThrow(
            /mutation/i
        )
        expect(() => parseOnboardingHistory({ ...valid, total: 3 })).toThrow(/reconcile/i)
        expect(() =>
            parseOnboardingHistory({
                ...valid,
                total: 1,
                history_only: 0,
                current: 1,
                operations: [{ ...valid.operations[0], classification: 'invalid' }]
            })
        ).toThrow(/fail-closed|reconcile/i)
        expect(() =>
            parseOnboardingHistory({
                ...valid,
                operations: [valid.operations[0], { ...valid.operations[0] }]
            })
        ).toThrow(/duplicate/i)
    })
})
