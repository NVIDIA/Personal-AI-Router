// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { buildNodeLabelSegments } from '@/ui/utils/node-label-segments'

// The node card header previously rendered `gpus[0].name` alone, so a host with
// more than one GPU was represented by its first card and every other card was
// invisible in the summary view. `buildNodeLabelSegments` emits one segment per
// GPU instead. These tests pin that every GPU survives, in order, at the counts
// real hosts actually have — including the 8-GPU servers this has to hold for.

const gpuTexts = (name: string, ip: string, gpus: string[]) =>
    buildNodeLabelSegments(name, ip, gpus)
        .filter(segment => segment.id.startsWith('gpu-'))
        .map(segment => segment.text)

describe('buildNodeLabelSegments', () => {
    it('puts the name first, uppercased, followed by the address', () => {
        const segments = buildNodeLabelSegments('pluto.local.tld', '172.16.24.46', [])
        expect(segments).toEqual([
            { id: 'primary', text: 'PLUTO.LOCAL.TLD' },
            { id: 'ip', text: '172.16.24.46' }
        ])
    })

    it('falls back to the address as the primary segment when there is no name', () => {
        const segments = buildNodeLabelSegments('', '172.16.24.46', [])
        expect(segments).toEqual([{ id: 'primary', text: '172.16.24.46' }])
    })

    it('renders a single-GPU host unchanged', () => {
        expect(gpuTexts('TELESTO', '172.16.29.18', ['NVIDIA GeForce GTX 1060 6GB'])).toEqual([
            'NVIDIA GeForce GTX 1060 6GB'
        ])
    })

    it('renders BOTH cards of a dual-GPU host, in topology order', () => {
        // The reported case: only the first card was ever shown.
        expect(
            gpuTexts('pluto.local.tld', '172.16.24.46', [
                'Tesla P100-PCIE-16GB',
                'NVIDIA GeForce RTX 3060'
            ])
        ).toEqual(['Tesla P100-PCIE-16GB', 'NVIDIA GeForce RTX 3060'])
    })

    it.each([3, 4, 5, 6, 7, 8])('renders every GPU on a %i-GPU host', count => {
        const gpus = Array.from({ length: count }, (_, index) => `NVIDIA H100 80GB HBM3 #${index}`)
        const texts = gpuTexts('dgx-01', '10.0.0.10', gpus)

        expect(texts).toHaveLength(count)
        expect(texts).toEqual(gpus)
    })

    it('keeps ids unique and stable across an 8-GPU host so React can key them', () => {
        const gpus = Array.from({ length: 8 }, (_, index) => `NVIDIA A100-SXM4-80GB #${index}`)
        const ids = buildNodeLabelSegments('dgx-02', '10.0.0.11', gpus).map(segment => segment.id)

        expect(new Set(ids).size).toBe(ids.length)
        expect(ids).toEqual([
            'primary',
            'ip',
            ...Array.from({ length: 8 }, (_, index) => `gpu-${index}`)
        ])
    })

    it('drops blank GPU names without disturbing the indices of the rest', () => {
        // A blank name would otherwise render a separator with nothing after it.
        const texts = gpuTexts('mixed', '10.0.0.12', ['Tesla P100-PCIE-16GB', '', 'RTX 3060'])
        expect(texts).toEqual(['Tesla P100-PCIE-16GB', 'RTX 3060'])
    })

    it('emits no GPU segments for a host that reports none', () => {
        expect(gpuTexts('cpu-only', '10.0.0.13', [])).toEqual([])
    })
})
