// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { GPU_COLOR_PALETTE, WORKLOAD_COLOR_MAP } from '@/ui/constants/colors'
import { workloadNodeLabel } from '@/ui/utils/workload-labels'

const STARTED_AT = 1_700_000_000_000

describe('workloadNodeLabel', () => {
    it('says Sent to for an in-flight job', () => {
        expect(workloadNodeLabel({ state: 'queued', startedAt: null })).toBe('Sent to')
    })

    it('says Running on for a running job', () => {
        expect(workloadNodeLabel({ state: 'running', startedAt: STARTED_AT })).toBe('Running on')
    })

    it('says Ran on for a completed job', () => {
        expect(workloadNodeLabel({ state: 'completed', startedAt: STARTED_AT })).toBe('Ran on')
    })

    // The proxy keeps scheduledOn on the terminal event, so a job that ends
    // before it starts still names the node it was sent to.
    it('says Sent to for a job that failed before it started', () => {
        expect(workloadNodeLabel({ state: 'failed', startedAt: null })).toBe('Sent to')
    })

    it('says Sent to for a job that was cancelled before it started', () => {
        expect(workloadNodeLabel({ state: 'cancelled', startedAt: null })).toBe('Sent to')
    })

    it('says Ran on for a job that failed after it started', () => {
        expect(workloadNodeLabel({ state: 'failed', startedAt: STARTED_AT })).toBe('Ran on')
    })

    it('says Ran on for a job that was cancelled after it started', () => {
        expect(workloadNodeLabel({ state: 'cancelled', startedAt: STARTED_AT })).toBe('Ran on')
    })
})

describe('WORKLOAD_COLOR_MAP', () => {
    // Connection lines append a two-digit alpha to these values.
    it('holds only six-digit hex colors', () => {
        for (const [name, color] of Object.entries(WORKLOAD_COLOR_MAP)) {
            expect(color, name).toMatch(/^#[0-9a-f]{6}$/i)
        }
    })

    it('colors in-flight jobs with the GPU chart yellow', () => {
        expect(GPU_COLOR_PALETTE).toContain(WORKLOAD_COLOR_MAP.yellow)
    })
})
