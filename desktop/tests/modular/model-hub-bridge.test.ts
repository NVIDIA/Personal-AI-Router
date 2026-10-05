// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { getEngineHubModels } from '@/electron/service-bridge/model-catalog'

const mocks = vi.hoisted(() => ({
    getEngineHubModels: vi.fn<typeof getEngineHubModels>()
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => ({})
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => ({}),
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/service-bridge/model-catalog', () => ({
    getEngineHubModels: mocks.getEngineHubModels
}))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('model hub service bridge', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.getEngineHubModels.mockResolvedValue({ models: [] })
    })

    it('forwards an explicit llama.cpp query to the catalog relay', async () => {
        await handleServiceBridgeInvoke('engine:search-hub', {
            engineType: 'llama-cpp',
            query: 'qwen coder'
        })

        expect(mocks.getEngineHubModels).toHaveBeenCalledWith('llama-cpp', 'qwen coder')
    })

    it('preserves populated-catalog requests without a query', async () => {
        await handleServiceBridgeInvoke('engine:search-hub', { engineType: 'llama-cpp' })

        expect(mocks.getEngineHubModels).toHaveBeenCalledWith('llama-cpp', undefined)
    })
})
