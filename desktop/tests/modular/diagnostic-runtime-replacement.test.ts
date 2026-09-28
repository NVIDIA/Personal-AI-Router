// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { diagnosticRuntimeReplacementHandlers as handlers } from '@/electron/service-bridge/diagnostic-runtime-replacement-handlers'
import NCCLReplacementCard from '@/ui/components/ClusterSettings/NCCLReplacementCard'
import type { DiagnosticMPIManagedInventory } from '@/shared/types/diagnostic-mpi'
import type {
    NCCLReplacementOperation,
    NCCLReplacementReview,
    NCCLReplacementSelector
} from '@/shared/types/diagnostic-runtime-replacement'

const fake = vi.hoisted(() => ({ callProcess: vi.fn(), ready: true }))
vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => fake
}))

const oldId = 'a'.repeat(32)
const reviewId = 'b'.repeat(32)
const newId = 'c'.repeat(32)
const nodes = ['node-a', 'node-b']
const groupId = `pair-recipe/nccl-${createHash('sha256')
    .update(JSON.stringify(nodes.flatMap(nodeId => [nodeId, nodeId])))
    .digest('hex')
    .slice(0, 32)}`
const selector: NCCLReplacementSelector = {
    buildOperationId: oldId,
    reviewId,
    operationId: newId
}
const plan = (nodeId: string) => ({
    schemaVersion: 1,
    recipeId: 'dgx-spark-nccl-cuda13-sm121-user-build-v2',
    operationId: newId,
    planDigest: 'd'.repeat(64),
    identity: { nodeId, principal: nodeId },
    sources: {
        nccl: {
            url: 'https://github.com/NVIDIA/nccl.git',
            commit: '73cf112295c33aee2b895f329f592f2a9b4b0f97',
            tag: 'v2.30.7-1'
        },
        'nccl-tests': {
            url: 'https://github.com/NVIDIA/nccl-tests.git',
            commit: 'b4d5beebca8a76cf01335f724d154b9b9d394d96',
            tag: 'v2.20.0'
        }
    },
    limits: { parallelJobs: 2, maxBuildSeconds: 1800, maxAttempts: 3 },
    gpuExecuted: false,
    mpiExecuted: false,
    managerAdopted: false,
    runtimeValidated: false
})
const targets = nodes.map(nodeId => ({
    nodeId,
    principal: nodeId,
    state: 'reviewed',
    cleanupConfirmed: true,
    review: { state: 'reviewed', plan: plan(nodeId) }
}))
const rawRecord = {
    schemaVersion: 1,
    owner: 'pair-managed-nccl-registry-v1',
    operationId: oldId,
    reviewId: 'e'.repeat(32),
    groupId,
    approvedRevision: 4,
    adoptedAt: 1_790_000_000_000,
    adopted: true,
    runtimeValidated: false,
    runAvailable: false,
    targets: nodes.map((nodeId, index) => ({
        nodeId,
        principal: nodeId,
        local: index === 0,
        address: `192.0.2.${index + 1}`,
        ...(index
            ? { candidateId: 'f'.repeat(32), sshHostKeySha256: `SHA256:${'A'.repeat(43)}` }
            : {}),
        clusterPinSha256: 'f'.repeat(64),
        planDigest: 'd'.repeat(64),
        attempt: 1,
        artifactObservedAt: '2026-09-22T00:00:00Z',
        registration: {
            schemaVersion: 1,
            kind: 'pair-nccl-runtime-candidate-v1',
            operationId: oldId,
            planDigest: 'd'.repeat(64),
            managerAdopted: false,
            runtimeValidated: false,
            gpuExecuted: false,
            mpiExecuted: false,
            linkValidation: 'static-elf-resolution-only',
            identity: { nodeId, principal: nodeId },
            secret: 'must-stay-in-electron'
        }
    }))
}
const rawReview = {
    reviewId,
    operationId: newId,
    groupId,
    expiresAt: Date.now() + 60_000,
    canBuild: true,
    targets
}
const rawOperation = {
    operationId: newId,
    reviewId,
    groupId,
    state: 'running',
    stage: 'building',
    revision: 2,
    startedAt: 1_790_000_000_000,
    targets,
    cleanupConfirmed: false,
    runtimeValidated: false,
    adopted: false,
    retrySourceStatus: 'current'
}
const builtTargets = targets.map(target => ({
    ...target,
    state: 'built',
    attempt: 1,
    receipt: {
        operationId: newId,
        state: 'built',
        planDigest: 'd'.repeat(64),
        artifactsValidated: true,
        cleanupConfirmed: true,
        registration: {
            operationId: newId,
            planDigest: 'd'.repeat(64),
            managerAdopted: false,
            runtimeValidated: false,
            gpuExecuted: false,
            mpiExecuted: false
        }
    }
}))
const completed = {
    ...rawOperation,
    state: 'completed',
    stage: 'built',
    revision: 7,
    finishedAt: 1_790_000_000_100,
    cleanupConfirmed: true,
    targets: builtTargets
}
const newRecord = {
    ...rawRecord,
    operationId: newId,
    reviewId,
    approvedRevision: 7,
    targets: rawRecord.targets.map(target => ({
        ...target,
        registration: { ...target.registration, operationId: newId }
    }))
}

function backend(overrides: Record<string, unknown> = {}) {
    fake.callProcess.mockImplementation(
        async (_process: string, method: string, params: { operationId?: string }) => {
            if (method === 'engine:diagnostic-managed-runtime')
                return params.operationId === newId
                    ? { record: overrides.newRecord ?? null }
                    : { record: overrides.record ?? rawRecord }
            if (method === 'engine:diagnostic-runtime-review') return overrides.review ?? rawReview
            if (method === 'engine:diagnostic-runtime-status')
                return overrides.status ?? { operation: rawOperation, recoveryRequired: false }
            if (method === 'engine:diagnostic-runtime-approve')
                return overrides.approve ?? rawOperation
            if (method === 'engine:diagnostic-runtime-adopt') return overrides.adopt
            throw new Error(`unexpected ${method}`)
        }
    )
}

describe('managed NCCL replacement boundary', () => {
    beforeEach(() => {
        fake.callProcess.mockReset()
        backend()
    })

    it('derives the full review roster only from the exact raw adopted record', async () => {
        const review = await handlers['engine:diagnostic-nccl-replacement-review']({
            buildOperationId: oldId
        })
        expect(fake.callProcess.mock.calls[0][1]).toBe('engine:diagnostic-managed-runtime')
        expect(fake.callProcess.mock.calls[1][1]).toBe('engine:diagnostic-runtime-review')
        expect(fake.callProcess.mock.calls[1][2]).toEqual({
            groupId,
            members: nodes.map(nodeId => ({
                nodeId,
                principal: nodeId,
                gb10Observed: false,
                controlAdvertised: false
            })),
            acceptedHostKeys: [
                {
                    candidateId: 'f'.repeat(32),
                    sha256: `SHA256:${'A'.repeat(43)}`
                }
            ]
        })
        expect(review.operationId).toBe(newId)
        expect(JSON.stringify(review)).not.toMatch(
            /principal|registration|candidateId|sshHostKey|must-stay-in-electron/
        )
    })

    it('rejects a different record, roster tamper, and review response mismatch', async () => {
        backend({ record: { ...rawRecord, operationId: newId } })
        await expect(
            handlers['engine:diagnostic-nccl-replacement-review']({ buildOperationId: oldId })
        ).rejects.toThrow()
        expect(fake.callProcess.mock.calls.map(call => call[1])).not.toContain(
            'engine:diagnostic-runtime-review'
        )

        fake.callProcess.mockReset()
        backend({ record: { ...rawRecord, targets: [rawRecord.targets[0], rawRecord.targets[0]] } })
        await expect(
            handlers['engine:diagnostic-nccl-replacement-review']({ buildOperationId: oldId })
        ).rejects.toThrow()

        fake.callProcess.mockReset()
        backend({ review: { ...rawReview, targets: [...targets].reverse() } })
        await expect(
            handlers['engine:diagnostic-nccl-replacement-review']({ buildOperationId: oldId })
        ).rejects.toThrow()
    })

    it('never addresses the adopted operation for status or retry discovery', async () => {
        await expect(
            handlers['engine:diagnostic-nccl-replacement-status']({
                ...selector,
                operationId: oldId
            })
        ).rejects.toThrow()
        await expect(
            handlers['engine:diagnostic-nccl-replacement-retry']({
                ...selector,
                operationId: oldId,
                expectedRevision: 1
            })
        ).rejects.toThrow()
        expect(fake.callProcess).not.toHaveBeenCalled()
    })

    it('binds status and approval replies to the exact new operation', async () => {
        backend({
            status: { operation: { ...rawOperation, operationId: oldId }, recoveryRequired: false }
        })
        await expect(
            handlers['engine:diagnostic-nccl-replacement-status'](selector)
        ).rejects.toThrow()
        fake.callProcess.mockReset()
        backend({
            approve: { ...rawOperation, reviewId: 'f'.repeat(32) },
            status: {
                operation: { ...rawOperation, reviewId: 'f'.repeat(32) },
                recoveryRequired: false
            }
        })
        await expect(
            handlers['engine:diagnostic-nccl-replacement-approve'](selector)
        ).rejects.toThrow()
        expect(fake.callProcess.mock.calls.map(call => call[1])).toContain(
            'engine:diagnostic-runtime-status'
        )
    })

    it('recovers a lost approval reply using only the exact new review and operation', async () => {
        fake.callProcess.mockImplementation(
            async (_process: string, method: string, params: { operationId?: string }) => {
                if (method === 'engine:diagnostic-managed-runtime')
                    return { record: params.operationId === oldId ? rawRecord : null }
                if (method === 'engine:diagnostic-runtime-approve') throw new Error('reply lost')
                if (method === 'engine:diagnostic-runtime-status')
                    return { operation: rawOperation, recoveryRequired: false }
                throw new Error(`unexpected ${method}`)
            }
        )
        const result = await handlers['engine:diagnostic-nccl-replacement-approve'](selector)
        expect(result.operationId).toBe(newId)
        expect(
            fake.callProcess.mock.calls.filter(
                call => call[1] === 'engine:diagnostic-runtime-approve'
            )
        ).toHaveLength(1)
        expect(
            fake.callProcess.mock.calls.find(
                call => call[1] === 'engine:diagnostic-runtime-status'
            )?.[2]
        ).toEqual({
            operationId: newId,
            reviewId
        })
    })

    it('distinguishes a confirmed new record from a conclusively absent publication', async () => {
        backend({ status: { operation: completed, recoveryRequired: false } })
        const absent = await handlers['engine:diagnostic-nccl-replacement-status'](selector)
        expect(absent.registrationAbsent).toBe(true)
        expect(absent.registrationConfirmed).toBeUndefined()
        fake.callProcess.mockReset()
        backend({ status: { operation: completed, recoveryRequired: false }, newRecord })
        const present = await handlers['engine:diagnostic-nccl-replacement-status'](selector)
        expect(present.registrationConfirmed).toBe(true)
        expect(present.registrationAbsent).toBe(false)
    })

    it('registers only the completed clean new revision and projects the new record', async () => {
        backend({
            status: { operation: completed, recoveryRequired: false },
            adopt: {
                operation: { ...completed, adopted: true, stage: 'runtime-adopted', revision: 8 },
                record: newRecord
            }
        })
        const result = await handlers['engine:diagnostic-nccl-replacement-adopt']({
            ...selector,
            expectedRevision: 7
        })
        expect(result.operation.adopted).toBe(true)
        expect(result.adoptedAt).toBe(newRecord.adoptedAt)
        expect(JSON.stringify(result)).not.toMatch(/"principal"|registration|must-stay-in-electron/)
        expect(fake.callProcess.mock.calls[1][1]).toBe('engine:diagnostic-runtime-adopt')
    })

    it('lets Engine Manager arbitrate stale retry and adoption revisions atomically', async () => {
        fake.callProcess.mockImplementation(
            async (_process: string, method: string, params: { operationId?: string }) => {
                if (method === 'engine:diagnostic-managed-runtime')
                    return { record: params.operationId === oldId ? rawRecord : null }
                if (
                    method === 'engine:diagnostic-runtime-retry' ||
                    method === 'engine:diagnostic-runtime-adopt'
                )
                    throw new Error('revision changed')
                if (method === 'engine:diagnostic-runtime-status')
                    return { operation: completed, recoveryRequired: false }
                throw new Error(`unexpected ${method}`)
            }
        )
        const recovered = await handlers['engine:diagnostic-nccl-replacement-retry']({
            ...selector,
            expectedRevision: 6
        })
        expect(recovered.revision).toBe(7)
        await expect(
            handlers['engine:diagnostic-nccl-replacement-adopt']({
                ...selector,
                expectedRevision: 6
            })
        ).rejects.toThrow()
        expect(fake.callProcess.mock.calls.map(call => call[1])).toContain(
            'engine:diagnostic-runtime-retry'
        )
        expect(fake.callProcess.mock.calls.map(call => call[1])).toContain(
            'engine:diagnostic-runtime-adopt'
        )
    })
})

describe('replacement UI stages', () => {
    const inventory = {
        records: [
            {
                buildOperationId: oldId,
                adoptedAt: 1,
                adopted: true,
                runtimeValidated: false,
                runAvailable: false,
                targets: nodes.map(nodeId => ({ nodeId, local: nodeId === 'node-a' }))
            }
        ],
        recoveryRequired: false
    } satisfies DiagnosticMPIManagedInventory
    const props = {
        inventory,
        buildOperationId: oldId,
        blocked: false,
        onRegistered: async () => {}
    }
    const review: NCCLReplacementReview = {
        ...selector,
        groupId,
        expiresAt: Date.now() + 60_000,
        canBuild: true,
        targets: nodes.map(nodeId => ({
            nodeId,
            state: 'reviewed',
            attempt: 0,
            cleanupConfirmed: true,
            retryClosed: false,
            plan: {
                nccl: 'v2.30.7-1 · 73cf1122',
                ncclTests: 'v2.20.0 · b4d5beeb',
                parallelJobs: 2,
                maxBuildSeconds: 1800,
                maxAttempts: 3
            }
        }))
    }
    const operation: NCCLReplacementOperation = {
        ...selector,
        groupId,
        state: 'completed',
        stage: 'built',
        revision: 7,
        startedAt: 1,
        finishedAt: 2,
        targets: review.targets.map(target => ({ ...target, state: 'built' })),
        cleanupConfirmed: true,
        adopted: false,
        retrySourceStatus: 'current'
    }

    it('shows explicit review, approval, progress, and register steps without raw identity', () => {
        const start = renderToStaticMarkup(createElement(NCCLReplacementCard, props))
        const reviewed = renderToStaticMarkup(
            createElement(NCCLReplacementCard, {
                ...props,
                initialSelector: selector,
                initialReview: review
            })
        )
        const built = renderToStaticMarkup(
            createElement(NCCLReplacementCard, {
                ...props,
                initialSelector: selector,
                initialOperation: operation
            })
        )
        expect(start).toContain('Review new build')
        expect(reviewed).toContain('Approve new build')
        expect(reviewed).toContain('NCCL v2.30.7-1')
        expect(reviewed).toContain('CPU only')
        expect(built).toContain('Register new build')
        expect(built).toContain('Cleanup confirmed')
        expect(built).not.toMatch(/"principal"|privateKey|must-stay-in-electron/i)
    })

    it('keeps exact in-flight adoption cancellation available after a lost Register reply', () => {
        const adopting = renderToStaticMarkup(
            createElement(NCCLReplacementCard, {
                ...props,
                initialSelector: selector,
                initialAdoptionAttempted: true,
                initialOperation: {
                    ...operation,
                    stage: 'adopting-runtime'
                }
            })
        )
        expect(adopting).toMatch(
            /<button(?![^>]*disabled)[^>]*>Cancel or reconcile new build<\/button>/
        )
        expect(adopting).not.toContain('Registering…')
    })
})
