// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

const mocks = vi.hoisted(() => ({
    supervisor: {
        callProcess: vi.fn<ReturnType<typeof getModularSupervisor>['callProcess']>()
    },
    getSelfId: vi.fn<() => string | null>()
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => ({ getSelfId: mocks.getSelfId })
}))

import { getEngineHubModels, warmEngineHubs } from '@/electron/service-bridge/model-catalog'
import { MODULAR_CATALOG_CALL_TIMEOUT_MS } from '@/shared/constants/modular-runtime'
import { searchEngineHub } from '@/ui/utils/model-hub-search'

describe('model catalog relay', () => {
    beforeEach(() => {
        mocks.supervisor.callProcess.mockReset().mockResolvedValue({ models: [] })
        mocks.getSelfId.mockReset().mockReturnValue('self')
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
        await getEngineHubModels('lm-studio', undefined, '  gemma  ')
        expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
            'broker',
            'engine:catalog',
            { engine: 'lmstudio', query: 'gemma' },
            MODULAR_CATALOG_CALL_TIMEOUT_MS
        )
        await getEngineHubModels('lm-studio', undefined, '   ')
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

    // Unknown capability lets the renderer distinguish failure from a valid
    // empty list while background warming remains safe to fire and forget.
    it('marks a failed catalog unread so the renderer can offer recovery', async () => {
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
        await expect(searchEngineHub('vllm', 'self')).rejects.toThrow(
            'Available models could not be loaded. Please try again.'
        )

        const result = await searchEngineHub('vllm', 'self', 'coder')
        expect(result.searchable).toBe(true)
        expect(result.models.map(model => model.id)).toEqual(['org/coder'])
        expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
            'broker',
            'engine:catalog',
            { engine: 'vllm', query: 'coder' },
            MODULAR_CATALOG_CALL_TIMEOUT_MS
        )
    })

    describe('for a llama.cpp node', () => {
        function settingsFor(modelSource: string | undefined) {
            return {
                nodeId: 'mac',
                engine: 'llamacpp',
                phase: 'idle',
                epoch: 'e1',
                sequence: 1,
                revision: 1,
                appliedRevision: 1,
                settings: {
                    serverPort: 8081,
                    proxyPort: 8080,
                    launchText: '',
                    ...(modelSource ? { modelSource } : {})
                },
                effectiveServerPort: 8081,
                effectiveProxyPort: 8080,
                running: true,
                adopted: false,
                editable: true,
                reason: '',
                format: 'pair-arguments-v1',
                error: '',
                requestId: ''
            }
        }

        function answer(modelSource: string | undefined) {
            mocks.supervisor.callProcess.mockImplementation(async (_process, method) =>
                method === 'engine:get-settings'
                    ? settingsFor(modelSource)
                    : { models: [], searchable: true }
            )
        }

        // The catalog is served by this device's engine manager, which would
        // otherwise answer with this device's source rather than the peer's.
        // Settings calls are budgeted for minutes of engine restart, so the
        // read carries the catalog's bound rather than waiting that out.
        it("asks for the catalog from that peer's saved download source", async () => {
            answer('modelscope')

            await getEngineHubModels('llama-cpp', 'mac', 'qwen')

            expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
                1,
                'broker',
                'engine:get-settings',
                { nodeId: 'mac', engine: 'llamacpp' },
                MODULAR_CATALOG_CALL_TIMEOUT_MS
            )
            expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
                'broker',
                'engine:catalog',
                { engine: 'llamacpp', query: 'qwen', modelSource: 'modelscope' },
                MODULAR_CATALOG_CALL_TIMEOUT_MS
            )
        })

        // Settings change between opens, so a saved source is not reused.
        it('reads the source again for every request', async () => {
            answer('modelscope')
            await getEngineHubModels('llama-cpp', 'mac')
            answer('huggingface')
            await getEngineHubModels('llama-cpp', 'mac')

            expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
                'broker',
                'engine:catalog',
                { engine: 'llamacpp', modelSource: 'huggingface' },
                MODULAR_CATALOG_CALL_TIMEOUT_MS
            )
        })

        // This device's engine manager already uses its own selection, and a
        // settings read would broadcast a settings push on every search.
        it("reads no settings for this device's own node", async () => {
            answer('modelscope')

            await getEngineHubModels('llama-cpp', 'self', 'qwen')

            expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'engine:catalog',
                { engine: 'llamacpp', query: 'qwen' },
                MODULAR_CATALOG_CALL_TIMEOUT_MS
            )
        })

        it('still reads the source while this device is not yet identified', async () => {
            mocks.getSelfId.mockReturnValue(null)
            answer('modelscope')

            await getEngineHubModels('llama-cpp', 'mac')

            expect(mocks.supervisor.callProcess).toHaveBeenLastCalledWith(
                'broker',
                'engine:catalog',
                { engine: 'llamacpp', modelSource: 'modelscope' },
                MODULAR_CATALOG_CALL_TIMEOUT_MS
            )
        })

        // Falling back to this device's source would list models the peer
        // then fetches from a different provider. The message names the peer,
        // not the device showing it.
        it('refuses a peer that does not report its source', async () => {
            answer(undefined)

            await expect(getEngineHubModels('llama-cpp', 'mac')).rejects.toThrow(
                'That device does not report its model download source. Update PAIR on that device.'
            )
            expect(mocks.supervisor.callProcess).not.toHaveBeenCalledWith(
                'broker',
                'engine:catalog',
                expect.anything(),
                expect.anything()
            )
        })

        it('surfaces a failed settings read rather than an empty hub', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(new Error('peer unreachable'))

            await expect(getEngineHubModels('llama-cpp', 'mac')).rejects.toThrow(
                "Could not read that device's model download source: peer unreachable"
            )
        })

        it('leaves engines with a fixed source alone', async () => {
            await getEngineHubModels('lm-studio', 'mac')

            expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'engine:catalog',
                { engine: 'lmstudio' },
                MODULAR_CATALOG_CALL_TIMEOUT_MS
            )
        })
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
