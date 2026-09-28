// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    supervisor: { ready: true, callProcess: vi.fn() }
}))
vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))

import { fabricHandlers } from '@/electron/service-bridge/fabric-handlers'

const operationId = 'a'.repeat(32)
const hostKeyFingerprint = `SHA256:${'A'.repeat(43)}`
const invalidHostKeyFingerprints = [
    `sha256:${'A'.repeat(43)}`,
    `SHA256:${'A'.repeat(42)}`,
    `SHA256:${'A'.repeat(44)}`,
    `SHA256:${'A'.repeat(42)}_`
]
const selection = {
    nodeIds: ['node-a', 'node-b'],
    ports: [
        { nodeId: 'node-a', switchId: 'switch-a', portName: 'p0' },
        { nodeId: 'node-b', switchId: 'switch-b', portName: 'p0' }
    ]
}
const operation = (state = 'recovery-required') => ({
    schemaVersion: 1,
    operationId,
    reviewId: operationId,
    ownerNodeId: 'node-a',
    state,
    targets: [],
    cleanupConfirmed: false,
    effectsApplied: false,
    message: 'Authoritative operation status.',
    createdAt: 1,
    expiresAt: 0
})
const cableTarget = (nodeId: string, switchId: string) => ({
    nodeId,
    principal: `principal-${nodeId}`,
    ports: [
        {
            switchId,
            portName: 'p0',
            interfaces: [
                {
                    name: `eth-${nodeId}`,
                    index: nodeId === 'node-a' ? 1 : 2,
                    mac: '02:00:00:00:00:01'
                }
            ]
        }
    ],
    rawPrivilege: 'approval-needed'
})
const cableReview = (hostKeySha256: string) => ({
    reviewId: 'cable-review-a',
    ownerNodeId: 'node-a',
    targets: [cableTarget('node-a', 'switch-a'), cableTarget('node-b', 'switch-b')],
    available: false,
    reason: 'Accept the exact observed host key.',
    remainingMs: 20_000,
    permission: {
        mode: 'temporary-admin',
        targets: ['node-a', 'node-b'].map(nodeId => ({
            nodeId,
            candidateId: `candidate-${nodeId}`,
            accessLabel: nodeId,
            hostKeySha256,
            hostKeyTrusted: false,
            accessAvailable: false,
            elevationAvailable: false,
            workerAvailable: false
        })),
        effects: ['Review only.']
    }
})

describe('physical cable and fabric bridge', () => {
    beforeEach(() => {
        mocks.supervisor.ready = true
        mocks.supervisor.callProcess.mockReset()
    })

    it('reads only the selected nodes bounded physical-port projection', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            nodes: [
                {
                    nodeId: 'node-a',
                    status: 'observed',
                    observedAt: 1_800_000_000_000,
                    ports: [{ switchId: 'switch-a', portName: 'p0', eligible: true }]
                },
                {
                    nodeId: 'node-b',
                    status: 'unavailable',
                    ports: [],
                    reason: 'Current paired node-info fabric inventory is unavailable.'
                }
            ]
        })
        const result = await fabricHandlers['engine:fabric-inventory']({
            nodeIds: ['node-a', 'node-b']
        })
        expect(result.nodes.map(node => node.status)).toEqual(['observed', 'unavailable'])
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:fabric-inventory',
            { nodeIds: ['node-a', 'node-b'] },
            20_000
        )

        await expect(
            fabricHandlers['engine:fabric-inventory']({
                nodeIds: ['node-a', 'node-a']
            })
        ).rejects.toThrow()
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
    })

    it('injects fixed cable approval and accepts a bound not-started disposition', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            disposition: 'not-started',
            reviewId: 'cable-review-a',
            ownerNodeId: 'node-a',
            reason: 'review-expired'
        })
        const result = await fabricHandlers['engine:cable-start']({
            reviewId: 'cable-review-a'
        })
        expect(result).toMatchObject({ disposition: 'not-started', reason: 'review-expired' })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:cable-start',
            { reviewId: 'cable-review-a', approveAdmin: true },
            75_000
        )
    })

    it('passes only canonical OpenSSH host fingerprints across the cable bridge', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(cableReview(hostKeyFingerprint))
        const review = await fabricHandlers['engine:cable-review'](selection)
        expect(review.permission?.targets[0].hostKeySha256).toBe(hostKeyFingerprint)

        for (const fingerprint of invalidHostKeyFingerprints) {
            mocks.supervisor.callProcess.mockResolvedValueOnce(cableReview(fingerprint))
            await expect(fabricHandlers['engine:cable-review'](selection)).rejects.toThrow(
                /invalid host key fingerprint/
            )
        }
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(
            1 + invalidHostKeyFingerprints.length
        )
    })

    it('forwards only the typed review selection and optional read-only inspection flag', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            schemaVersion: 1,
            reviewId: operationId,
            ownerNodeId: '',
            recipeId: 'spark-two-node-temporary-addresses-v1',
            persistence: 'until-reboot',
            state: 'blocked',
            executable: false,
            remainingMs: 20_000,
            targets: [],
            blockers: ['Current inventory unavailable.'],
            effectsApplied: false
        })
        await fabricHandlers['engine:fabric-review']({
            selection,
            inspectSelectedProfiles: true
        })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:fabric-review',
            { ...selection, inspectSelectedProfiles: true },
            35_000
        )
    })

    it('injects administrator consent for exact cancel/recovery but never accepts private fields', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(operation())
        await fabricHandlers['engine:fabric-recover']({ operationId })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:fabric-recover',
            { operationId, administratorApproved: true },
            90_000
        )

        await expect(
            fabricHandlers['engine:fabric-cancel']({ operationId, password: 'secret' } as never)
        ).rejects.toThrow()
        await expect(
            fabricHandlers['engine:fabric-review']({
                selection: { ...selection, callback: 'http://private' } as never
            })
        ).rejects.toThrow()
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
    })

    it('lists retained fabric operations read-only and rejects confirmed cleanup', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({ operations: [operation()] })
        const retained = await fabricHandlers['engine:fabric-retained-operations']()
        expect(retained.operations.map(entry => entry.state)).toEqual(['recovery-required'])
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:fabric-retained-operations',
            {},
            30_000
        )

        mocks.supervisor.callProcess.mockResolvedValueOnce({
            operations: [{ ...operation('cancelled'), cleanupConfirmed: true }]
        })
        await expect(fabricHandlers['engine:fabric-retained-operations']()).rejects.toThrow()
        await expect(
            fabricHandlers['engine:fabric-retained-operations']({ operationId } as never)
        ).rejects.toThrow()
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(2)
    })

    it('reviews only the exact held cable run for bounded cleanup recovery', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            reviewId: 'cable-cleanup-review-a',
            runId: operationId,
            available: false,
            remainingMs: 20_000,
            reason: 'Fresh administrator access is required.',
            effects: ['Inspect only this run owned worker.'],
            targets: []
        })
        const review = await fabricHandlers['engine:cable-cleanup-review']({
            runId: operationId
        })
        expect(review.runId).toBe(operationId)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:cable-cleanup-review',
            { runId: operationId },
            35_000
        )
        await expect(
            fabricHandlers['engine:cable-cleanup-review']({
                runId: operationId,
                callback: 'http://private'
            } as never)
        ).rejects.toThrow()
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
    })

    it('rejects an operation reply that drifts from the requested server-issued id', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            ...operation(),
            operationId: 'b'.repeat(32),
            reviewId: 'b'.repeat(32)
        })
        await expect(fabricHandlers['engine:fabric-status']({ operationId })).rejects.toThrow(
            /different fabric operation/
        )
    })
})
