// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import {
    getModularBridgeState,
    parseWorkloadsInitial
} from '@/electron/service-bridge/modular-state'

function workloadInfo(id: string, engine: string, originatedFrom: string) {
    return { id, engine, state: 'completed', model: 'm', originatedFrom, createdAt: 1 }
}

// A workload the broker stored is a card the desktop shows. PAIR's own engines
// map from the workload manager's spelling onto the closed `EngineType` union
// (`lmstudio` -> `lm-studio`, `llamacpp` -> `llama-cpp`); an identifier from a
// third-party producer on the workload manager's local ingress is kept
// verbatim instead of the whole card being dropped.
describe('workload engine passthrough', () => {
    it('keeps a third-party engine identifier verbatim', () => {
        const parsed = parseWorkloadsInitial({
            workloads: [
                workloadInfo('1', 'vllm', 'uuid-wl-pass'),
                workloadInfo('2', 'comfyui', 'uuid-wl-pass')
            ]
        })
        expect(parsed.map(w => [w.id, w.engine])).toEqual([
            ['1', 'vllm'],
            ['2', 'comfyui']
        ])
    })

    it('still maps the workload manager spelling of PAIR engines onto EngineType', () => {
        const parsed = parseWorkloadsInitial({
            workloads: [
                workloadInfo('1', 'ollama', 'uuid-wl-pass'),
                workloadInfo('2', 'lmstudio', 'uuid-wl-pass'),
                workloadInfo('3', 'llamacpp', 'uuid-wl-pass')
            ]
        })
        expect(parsed.map(w => [w.id, w.engine])).toEqual([
            ['1', 'ollama'],
            ['2', 'lm-studio'],
            ['3', 'llama-cpp']
        ])
    })

    it('drops a workload that names no engine', () => {
        const parsed = parseWorkloadsInitial({
            workloads: [
                {
                    id: '1',
                    state: 'completed',
                    model: 'm',
                    originatedFrom: 'uuid-wl-pass',
                    createdAt: 1
                },
                workloadInfo('2', '', 'uuid-wl-pass')
            ]
        })
        expect(parsed).toEqual([])
    })

    it('puts a live third-party upsert into the catalog', () => {
        const state = getModularBridgeState()
        state.upsertWorkloadFromInfo({
            workloadInfo: workloadInfo('job-third-party', 'whispercpp', 'uuid-wl-live')
        })
        const catalog = state.getWorkloads()
        expect(catalog['uuid-wl-live\u0000job-third-party']).toMatchObject({
            id: 'job-third-party',
            engine: 'whispercpp'
        })
    })
})
