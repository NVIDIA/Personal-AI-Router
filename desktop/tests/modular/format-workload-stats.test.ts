// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { formatWorkloadStats } from '@/ui/utils/format-workload-stats'

describe('formatWorkloadStats', () => {
    it('shows engine-reported throughput, token count and time to first token', () => {
        expect(
            formatWorkloadStats({
                promptTokens: 12,
                completionTokens: 1234,
                tokensPerSecond: 42.34,
                ttftMs: 812
            })
        ).toEqual(['42.3 tok/s', '1,234 tokens', '812 ms to first token'])
    })

    it('marks estimated counts and rounds fast rates to whole tokens', () => {
        expect(
            formatWorkloadStats({
                completionTokens: 40,
                tokensPerSecond: 133.7,
                ttftMs: 1480,
                estimated: true
            })
        ).toEqual(['~134 tok/s', '~40 tokens', '1.5 s to first token'])
    })

    it('falls back to prompt tokens for a job that generated nothing', () => {
        expect(formatWorkloadStats({ promptTokens: 8, ttftMs: 30 })).toEqual([
            '8 prompt tokens',
            '30 ms to first token'
        ])
    })

    it('is empty when nothing was measured', () => {
        expect(formatWorkloadStats(undefined)).toEqual([])
        expect(formatWorkloadStats({})).toEqual([])
    })
})
