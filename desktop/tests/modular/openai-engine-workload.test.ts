// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { parseWorkloadsInitial } from '@/electron/service-bridge/modular-state'

// External OpenAI-compatible endpoints report the workload engine "openai".
// The desktop's EngineType union is closed; the mapping must render those
// workloads through the existing lm-studio path instead of silently dropping
// them at the unknown-engine guard.
describe('workload engine mapping for external endpoints', () => {
    it('keeps an "openai" workload, rendered as lm-studio', () => {
        const workloads = parseWorkloadsInitial({
            workloads: [
                {
                    id: 'job-openai',
                    engine: 'openai',
                    state: 'running',
                    model: 'stub-model',
                    originatedFrom: 'openai-wl-seed',
                    createdAt: 100
                }
            ]
        })

        expect(workloads).toHaveLength(1)
        expect(workloads[0].engine).toBe('lm-studio')
    })

    it('still maps the engine-manager "lmstudio" id and drops unknown engines', () => {
        const workloads = parseWorkloadsInitial({
            workloads: [
                {
                    id: 'job-lmstudio',
                    engine: 'lmstudio',
                    state: 'running',
                    model: 'local-model',
                    originatedFrom: 'openai-wl-seed',
                    createdAt: 100
                },
                {
                    id: 'job-mystery',
                    engine: 'mystery-engine',
                    state: 'running',
                    model: 'ghost-model',
                    originatedFrom: 'openai-wl-seed',
                    createdAt: 200
                }
            ]
        })

        expect(workloads).toHaveLength(1)
        expect(workloads[0].id).toBe('job-lmstudio')
        expect(workloads[0].engine).toBe('lm-studio')
    })
})
