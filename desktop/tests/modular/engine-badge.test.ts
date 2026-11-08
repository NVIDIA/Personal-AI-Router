// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { engineBadge } from '@/ui/utils/engine-badge'

// The badge for an engine PAIR has no logo for: a third-party producer on the
// workload manager's local ingress names its own engine, and the card shows a
// monogram of it.
describe('engineBadge', () => {
    it('uses the first two letters of the identifier, upper-cased', () => {
        expect(engineBadge('vllm')).toEqual({ monogram: 'VL', name: 'vllm' })
    })

    it('skips punctuation when picking the monogram', () => {
        expect(engineBadge('-whisper.cpp')).toEqual({ monogram: 'WH', name: '-whisper.cpp' })
    })

    it('keeps digits in the monogram', () => {
        expect(engineBadge('3dgen').monogram).toBe('3D')
    })

    it('uses a single character when that is all the identifier has', () => {
        expect(engineBadge('x').monogram).toBe('X')
    })

    it('falls back to a question mark when the identifier has no letters or digits', () => {
        expect(engineBadge('---')).toEqual({ monogram: '?', name: '---' })
    })

    it('caps the name that reaches the DOM', () => {
        const badge = engineBadge('e'.repeat(500))
        expect(badge.name).toHaveLength(64)
        expect(badge.monogram).toBe('EE')
    })
})
