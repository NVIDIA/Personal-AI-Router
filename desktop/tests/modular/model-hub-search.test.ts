// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { mergeModelHubResults } from '@/ui/utils/model-hub-search'
import type { ModelEntry } from '@/ui/types/model-hub'

function model(id: string, updatedAt: string): ModelEntry {
    return {
        id,
        name: id,
        author: id.slice(0, id.indexOf('/')),
        url: `https://huggingface.co/${id.slice(0, id.lastIndexOf(':'))}`,
        updatedAt: new Date(updatedAt)
    }
}

describe('model hub result merging', () => {
    it('appends remote matches while preserving populated order', () => {
        const populated = [
            model('approved/alpha:Q4_K_M', '2026-01-01T00:00:00.000Z'),
            model('approved/shared:Q4_K_M', '2026-01-01T00:00:00.000Z')
        ]
        const remote = [
            model('approved/shared:Q4_K_M', '2026-09-01T00:00:00.000Z'),
            model('community/beta:Q4_K_M', '2026-08-01T00:00:00.000Z')
        ]

        const merged = mergeModelHubResults(populated, remote)

        expect(merged.map(entry => entry.id)).toEqual([
            'approved/alpha:Q4_K_M',
            'approved/shared:Q4_K_M',
            'community/beta:Q4_K_M'
        ])
        expect(merged[1]).toBe(remote[0])
    })

    it('does not mutate either source list', () => {
        const populated = [model('approved/alpha:Q4_K_M', '2026-01-01T00:00:00.000Z')]
        const remote = [model('community/beta:Q4_K_M', '2026-08-01T00:00:00.000Z')]

        mergeModelHubResults(populated, remote)

        expect(populated.map(entry => entry.id)).toEqual(['approved/alpha:Q4_K_M'])
        expect(remote.map(entry => entry.id)).toEqual(['community/beta:Q4_K_M'])
    })
})
