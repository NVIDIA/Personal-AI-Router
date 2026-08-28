// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Invite } from '@/shared/types/cluster'
import type { getEngineHubModels } from '@/electron/model-hub'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type Supervisor = ReturnType<typeof getModularSupervisor>
type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        prunePendingInvite: vi.fn<BridgeState['prunePendingInvite']>()
    },
    supervisor: {
        callProcess: vi.fn<Supervisor['callProcess']>(),
        markAutoCreatedSoloForInvite: vi.fn<Supervisor['markAutoCreatedSoloForInvite']>()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({
    getEngineHubModels: vi.fn<typeof getEngineHubModels>()
}))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

const pendingInvite = {
    inviteId: 'invite-1',
    fromNodeId: 'sender.local',
    fromNodeUuid: 'sender-uuid',
    fromNodeName: 'Sender',
    toNodeId: null,
    clusterId: 'cluster-1',
    clusterFriendlyName: 'Home',
    pin: null,
    state: 'pending',
    reason: '',
    createdAt: 1,
    respondedAt: null
} satisfies Invite

function timeoutFor(method: string): number | undefined {
    const call = mocks.supervisor.callProcess.mock.calls.find(([, name]) => name === method)
    expect(call).toBeDefined()
    return call?.[3]
}

describe('cluster pairing RPC timeouts', () => {
    beforeEach(() => {
        mocks.supervisor.callProcess.mockImplementation((_process, method) => {
            if (method === 'cluster:get-node-id') {
                return Promise.resolve({ clusterId: 'cluster-1' })
            }
            return Promise.resolve(pendingInvite)
        })
    })

    it('allows an outbound invite to finish beyond the default RPC timeout', async () => {
        await handleServiceBridgeInvoke('cluster:invite-node', { ipAddress: '192.168.1.2' })

        expect(timeoutFor('cluster:invite-node')).toBe(35_000)
    })

    it('allows an invite response to finish beyond the default RPC timeout', async () => {
        await handleServiceBridgeInvoke('cluster:respond-to-invite', {
            inviteId: 'invite-1',
            accept: true,
            pin: '123456'
        })

        expect(timeoutFor('cluster:respond-to-invite')).toBe(35_000)
    })

    it('keeps ordinary cluster calls on the default RPC timeout', async () => {
        await handleServiceBridgeInvoke('cluster:invite-status', { inviteId: 'invite-1' })

        expect(timeoutFor('cluster:invite-status')).toBeUndefined()
    })
})
