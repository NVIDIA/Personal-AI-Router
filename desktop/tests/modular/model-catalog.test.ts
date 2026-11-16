// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

const mocks = vi.hoisted(() => ({
    supervisor: {
        callProcess: vi.fn<ReturnType<typeof getModularSupervisor>['callProcess']>()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))

import { getEngineHubModels, warmEngineHubs } from '@/electron/service-bridge/model-catalog'
import { MODULAR_CATALOG_CALL_TIMEOUT_MS } from '@/shared/constants/modular-runtime'
import { searchEngineHub } from '@/ui/utils/model-hub-search'

describe('model catalog relay', () => {
    beforeEach(() => {
        mocks.supervisor.callProcess.mockReset().mockResolvedValue({ models: [] })
    })

    afterEach(() => {
        vi.unstubAllGlobals()
    })

    // The renderer spells the engine `lm-studio`; the engine manager keys on
    // its manifest name. Catalog requests currently omit the destination's
    // platform and architecture.
    it('asks the backend in its own vocabulary, for this machine', async () => {
        await getEngineHubModels('lm-studio')
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:catalog',
            { engine: 'lmstudio' },
            MODULAR_CATALOG_CALL_TIMEOUT_MS
        )
    })

    // The backend decides whether a source can search; the relay only carries
    // the query, and a blank one is no query at all.
    it('passes a search query through, trimmed, and leaves a blank one out', async () => {
        await getEngineHubModels('lm-studio', '  gemma  ')
        expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
            'broker',
            'engine:catalog',
            { engine: 'lmstudio', query: 'gemma' },
            MODULAR_CATALOG_CALL_TIMEOUT_MS
        )
        await getEngineHubModels('lm-studio', '   ')
        expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
            'broker',
            'engine:catalog',
            { engine: 'lmstudio' },
            MODULAR_CATALOG_CALL_TIMEOUT_MS
        )
    })

    it('keeps only rows that can be pulled, and drops empty optional fields', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({
            models: [
                {
                    id: 'qwen3:8b',
                    name: 'qwen3:8b',
                    author: 'ollama',
                    url: 'https://ollama.com/library/qwen3',
                    size: 5000,
                    downloads: 3,
                    likes: 1,
                    updatedAt: '2026-07-21T00:00:00Z',
                    tags: ['qwen3', 7, 'Q4_K_M'],
                    family: 'qwen3',
                    parameterSize: '8B'
                },
                // A name alone is still pull-ready.
                { name: 'phi4:latest', size: 0, family: '', parameterSize: '' },
                // No id and no name: nothing to hand to a pull.
                { author: 'nobody' },
                'not an object',
                null
            ]
        })

        const { models } = await getEngineHubModels('ollama')

        expect(models).toEqual([
            {
                id: 'qwen3:8b',
                name: 'qwen3:8b',
                author: 'ollama',
                url: 'https://ollama.com/library/qwen3',
                size: 5000,
                downloads: 3,
                likes: 1,
                updatedAt: '2026-07-21T00:00:00Z',
                tags: ['qwen3', 'Q4_K_M'],
                family: 'qwen3',
                parameterSize: '8B'
            },
            {
                id: 'phi4:latest',
                name: 'phi4:latest',
                author: '',
                url: '',
                size: undefined,
                downloads: 0,
                likes: 0,
                updatedAt: '',
                tags: [],
                family: undefined,
                parameterSize: undefined
            }
        ])
    })

    it('reads a reply without a model list as an empty hub', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({ source: 'nowhere' })
        await expect(getEngineHubModels('ollama')).resolves.toEqual({
            models: [],
            searchable: false
        })
    })

    // The modal renders its empty state for this; a rejection would surface as
    // a failure the user can do nothing about.
    it('turns a failed call into an empty hub rather than an error', async () => {
        mocks.supervisor.callProcess.mockRejectedValue(new Error('lm studio catalog unavailable'))
        await expect(getEngineHubModels('lm-studio')).resolves.toEqual({
            models: [],
            searchable: null
        })
    })

    it('preserves the catalog capability reported by the backend', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({ models: [], searchable: true })

        await expect(getEngineHubModels('ollama')).resolves.toEqual({
            models: [],
            searchable: true
        })
    })

    it('keeps a catalog without search support limited to local filtering', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({ models: [], searchable: false })

        await expect(getEngineHubModels('vllm')).resolves.toEqual({
            models: [],
            searchable: false
        })
    })

    it('keeps search capability unknown after a failed browse and permits a later query', async () => {
        mocks.supervisor.callProcess
            .mockRejectedValueOnce(new Error('catalog temporarily unavailable'))
            .mockResolvedValueOnce({
                models: [{ id: 'org/coder', updatedAt: '2026-01-01T00:00:00.000Z' }],
                searchable: true
            })
        // Both the relay and display mapper must preserve unknown support so a
        // failed browse does not prevent the next query reaching the engine.
        vi.stubGlobal('window', { pairApi: { engines: { searchHub: getEngineHubModels } } })

        const browse = await searchEngineHub('vllm')
        expect(browse).toEqual({ models: [], searchable: null })

        const result = await searchEngineHub('vllm', 'coder')
        expect(result.searchable).toBe(true)
        expect(result.models.map(model => model.id)).toEqual(['org/coder'])
        expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
            'broker',
            'engine:catalog',
            { engine: 'vllm', query: 'coder' },
            MODULAR_CATALOG_CALL_TIMEOUT_MS
        )
    })

    // Ollama's list is compiled into the engine manager, so warming it would
    // cost a multi-megabyte reply for nothing.
    it('warms only the live-fetched catalogs', () => {
        warmEngineHubs()
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(3)
        for (const engine of ['lmstudio', 'llamacpp', 'vllm']) {
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'engine:catalog',
                { engine },
                MODULAR_CATALOG_CALL_TIMEOUT_MS
            )
        }
    })
})
