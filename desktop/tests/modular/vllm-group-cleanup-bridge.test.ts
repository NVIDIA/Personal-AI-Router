// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { HELD_RUN_ID, heldStatus, wire } from '../fixtures/vllm-group'

const digest = 'a'.repeat(64)
const binding = { runId: HELD_RUN_ID, generation: 13, planDigest: digest }

const mocks = vi.hoisted(() => ({
    state: {
        setVllmGroupStatus: vi.fn(),
        holdVllmGroupStatus: vi.fn()
    },
    supervisor: { ready: true, callProcess: vi.fn() }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => true,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => [],
    parseWorkloadsInitial: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import { JsonRpcResponseError } from '@/electron/service-bridge/json-rpc-subprocess'

function held(generation = 13) {
    const status = heldStatus()
    status.run!.generation = generation
    return wire(status)
}

/** PAIR's reply after its own reconcile/closure: the same operation, every attempted rank cleaned. */
function cleaned() {
    const status = heldStatus()
    status.reserved = false
    status.reason = ''
    status.run!.state = 'failed'
    status.run!.cleanupConfirmed = true
    status.run!.ranks.forEach(rank => {
        rank.cleanupConfirmed = true
    })
    return wire(status)
}

describe('vLLM cleanup bridge', () => {
    beforeEach(() => vi.clearAllMocks())

    it('freshly reads, sends the exact binding, and publishes PAIR reply status as authority', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(held()).mockResolvedValueOnce(cleaned())

        const status = await handleServiceBridgeInvoke('engine:vllm-group-cleanup', binding)

        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            1,
            'broker',
            'engine:vllm-group-status',
            {},
            30_000
        )
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            2,
            'broker',
            'engine:vllm-group-cleanup',
            binding,
            110_000
        )
        expect(status.reserved).toBe(false)
        expect(status.run?.cleanupConfirmed).toBe(true)
        expect(status).not.toHaveProperty('performed')
        expect(status).not.toHaveProperty('outcome')
        expect(mocks.state.setVllmGroupStatus).toHaveBeenCalledTimes(2)
        expect(mocks.state.setVllmGroupStatus).toHaveBeenLastCalledWith(status)
        expect(mocks.state.holdVllmGroupStatus).not.toHaveBeenCalled()
    })

    it('keeps the hold when PAIR replies with the same operation still reserved', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(held()).mockResolvedValueOnce(held())
        const status = await handleServiceBridgeInvoke('engine:vllm-group-cleanup', binding)
        expect(status.reserved).toBe(true)
        expect(status.run?.cleanupConfirmed).toBe(false)
        expect(mocks.state.setVllmGroupStatus).toHaveBeenLastCalledWith(status)
    })

    it('refuses a stale displayed generation before the cleanup RPC', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(held(14))
        await expect(
            handleServiceBridgeInvoke('engine:vllm-group-cleanup', binding)
        ).rejects.toThrow(/changed after it was displayed/)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
        expect(mocks.state.holdVllmGroupStatus).not.toHaveBeenCalled()
    })

    it('surfaces unresolved-rank refusals verbatim and holds', async () => {
        mocks.supervisor.callProcess
            .mockResolvedValueOnce(held())
            .mockRejectedValueOnce(
                new Error('rank node-b closure is unconfirmed; group remains held')
            )
        await expect(
            handleServiceBridgeInvoke('engine:vllm-group-cleanup', binding)
        ).rejects.toThrow('rank node-b closure is unconfirmed; group remains held')
        expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)
    })

    it('shows the backend refusal text verbatim without the transport code prefix', async () => {
        mocks.supervisor.callProcess
            .mockResolvedValueOnce(held())
            .mockRejectedValueOnce(
                new JsonRpcResponseError(
                    '-32000: cleanup remains unconfirmed; no replacement generation was admitted'
                )
            )
        await expect(
            handleServiceBridgeInvoke('engine:vllm-group-cleanup', binding)
        ).rejects.toThrow(/^cleanup remains unconfirmed; no replacement generation was admitted$/)
        expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)
    })

    it('refuses to publish a reply for a different operation and holds', async () => {
        const drifted = heldStatus()
        drifted.run!.generation = 14
        mocks.supervisor.callProcess
            .mockResolvedValueOnce(held())
            .mockResolvedValueOnce(wire(drifted))
        await expect(
            handleServiceBridgeInvoke('engine:vllm-group-cleanup', binding)
        ).rejects.toThrow(/different operation/)
        expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)
    })

    it('holds all mutations when the fresh status read fails', async () => {
        mocks.supervisor.callProcess.mockRejectedValueOnce(new Error('status unavailable'))
        await expect(
            handleServiceBridgeInvoke('engine:vllm-group-cleanup', binding)
        ).rejects.toThrow('status unavailable')
        expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
    })

    it('routes the managed lifecycle channels through the same bridge map', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(held())
        const status = await handleServiceBridgeInvoke('engine:vllm-group-status', undefined)
        expect(status.reserved).toBe(true)
        mocks.supervisor.callProcess.mockRejectedValueOnce(
            new Error('method not found: engine:vllm-group-review')
        )
        await expect(
            handleServiceBridgeInvoke('engine:vllm-group-review', {
                selection: { nodeIds: ['node-a', 'node-b'], model: 'example/model' }
            })
        ).rejects.toThrow(/method not found/)
    })
})
