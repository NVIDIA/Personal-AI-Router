// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
import type { getEngineHubModels } from '@/electron/service-bridge/model-catalog'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type Supervisor = ReturnType<typeof getModularSupervisor>
type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn<BridgeState['getSelfId']>()
    },
    supervisor: {
        callProcess: vi.fn<Supervisor['callProcess']>()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => [],
    parseWorkloadsInitial: () => ({})
}))
vi.mock('@/electron/service-bridge/model-catalog', () => ({
    getEngineHubModels: vi.fn<typeof getEngineHubModels>()
}))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('manual node removal', () => {
    // The desktop knows a broker node key. Its request must select that key
    // explicitly so an identical worker entry name cannot remove another entry.
    it('identifies a broker node key separately from a manual entry ID', async () => {
        const nodeId = 'manual:lab'
        mocks.state.getSelfId.mockReturnValue('self')
        mocks.supervisor.callProcess.mockResolvedValue({ nodeId, removed: true })

        const result = await handleServiceBridgeInvoke('nodes:remove-member', { nodeId })

        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'node/remove', {
            nodeId
        })
        expect(result).toEqual({ nodeId, removed: true })
    })
})
