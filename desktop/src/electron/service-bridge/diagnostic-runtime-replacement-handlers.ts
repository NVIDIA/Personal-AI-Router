// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto'
import type { WsInvokeRequest } from '@/shared/types/ws-channels'
import type { NCCLReplacementSelector } from '@/shared/types/diagnostic-runtime-replacement'
import {
    parseReplacementAdoption,
    parseReplacementBuild,
    parseReplacementManagedRecord,
    parseReplacementOperation,
    parseReplacementReview,
    parseReplacementSelector,
    parseReplacementStatus
} from '@/shared/utils/diagnostic-runtime-replacement'
import { getModularSupervisor } from './modular-supervisor'
import type { JsonObject } from './json-rpc-subprocess'

type Method =
    | 'engine:diagnostic-managed-runtime'
    | 'engine:diagnostic-runtime-review'
    | 'engine:diagnostic-runtime-approve'
    | 'engine:diagnostic-runtime-status'
    | 'engine:diagnostic-runtime-cancel'
    | 'engine:diagnostic-runtime-retry'
    | 'engine:diagnostic-runtime-adopt'

async function call(method: Method, params: JsonObject) {
    const supervisor = getModularSupervisor()
    if (!supervisor.ready) throw new Error('PAIR service is unavailable for NCCL replacement.')
    try {
        return await supervisor.callProcess(
            'broker',
            method,
            params,
            method === 'engine:diagnostic-runtime-review' ||
                method === 'engine:diagnostic-runtime-adopt'
                ? 255_000
                : 110_000
        )
    } catch {
        throw new Error(
            'PAIR did not confirm the NCCL replacement action. Refresh its exact new operation.'
        )
    }
}

async function trusted(buildOperationId: string) {
    // The UI inventory is redacted. Only this exact, durable Engine Manager
    // record supplies the principal identities used for the replacement roster.
    const record = parseReplacementManagedRecord(
        await call('engine:diagnostic-managed-runtime', { operationId: buildOperationId }),
        buildOperationId
    )
    const identities = record.targets.flatMap(target => [target.nodeId, target.principal])
    const digest = createHash('sha256')
        .update(JSON.stringify(identities))
        .digest('hex')
        .slice(0, 32)
    if (record.groupId !== `pair-recipe/nccl-${digest}`)
        throw new Error('The adopted NCCL roster digest changed; replacement is held.')
    return record
}

async function context(payload: unknown) {
    const selector = parseReplacementSelector(payload)
    const record = await trusted(selector.buildOperationId)
    return { selector, record }
}

async function status(
    selector: NCCLReplacementSelector,
    record: Awaited<ReturnType<typeof trusted>>
) {
    const raw = await call('engine:diagnostic-runtime-status', {
        operationId: selector.operationId,
        reviewId: selector.reviewId
    })
    const result = parseReplacementStatus(raw, selector, record.groupId, record.targets)
    if (result.operation?.state === 'completed' && !result.operation.adopted) {
        const lookupRaw = await call('engine:diagnostic-managed-runtime', {
            operationId: selector.operationId
        })
        if (!lookupRaw || typeof lookupRaw !== 'object' || Array.isArray(lookupRaw))
            throw new Error('PAIR did not confirm the new registry state.')
        const lookup = lookupRaw as { record?: unknown }
        if (!('record' in lookup)) throw new Error('PAIR did not confirm the new registry state.')
        if (lookup.record === null) {
            result.registrationAbsent = result.operation.stage !== 'adoption-publication-unknown'
        } else {
            const retained = parseReplacementManagedRecord(lookup, selector.operationId)
            const rawRecord = lookup.record as { reviewId?: unknown }
            if (
                retained.groupId !== record.groupId ||
                rawRecord.reviewId !== selector.reviewId ||
                retained.targets.some(
                    (target, index) =>
                        target.nodeId !== record.targets[index].nodeId ||
                        target.principal !== record.targets[index].principal
                )
            )
                throw new Error('The new NCCL registration changed its reviewed binding.')
            result.registrationAbsent = false
            result.registrationConfirmed = true
        }
    }
    return result
}

function revision(value: unknown): number {
    if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 1)
        throw new Error('The current new NCCL build revision is required.')
    return value
}

export const diagnosticRuntimeReplacementHandlers = {
    'engine:diagnostic-nccl-replacement-review': async (
        payload?: WsInvokeRequest<'engine:diagnostic-nccl-replacement-review'>
    ) => {
        const buildOperationId = parseReplacementBuild(payload)
        const record = await trusted(buildOperationId)
        const acceptedHostKeys = record.targets.flatMap(target =>
            target.candidateId && target.sshHostKeySha256
                ? [{ candidateId: target.candidateId, sha256: target.sshHostKeySha256 }]
                : []
        )
        const raw = await call('engine:diagnostic-runtime-review', {
            groupId: record.groupId,
            members: record.targets.map(target => ({
                nodeId: target.nodeId,
                principal: target.principal,
                // A retained adoption is not a fresh platform observation.
                gb10Observed: false,
                controlAdvertised: false
            })),
            ...(acceptedHostKeys.length ? { acceptedHostKeys } : {})
        })
        return parseReplacementReview(raw, buildOperationId, record.groupId, record.targets)
    },

    'engine:diagnostic-nccl-replacement-approve': async (
        payload?: WsInvokeRequest<'engine:diagnostic-nccl-replacement-approve'>
    ) => {
        const { selector, record } = await context(payload)
        try {
            return parseReplacementOperation(
                await call('engine:diagnostic-runtime-approve', { reviewId: selector.reviewId }),
                selector,
                record.groupId,
                record.targets
            )
        } catch (error) {
            // Approval is one-use; a lost reply is recovered by the new identity.
            try {
                const recovered = await status(selector, record)
                if (recovered.operation) return recovered.operation
            } catch {
                // Keep the first error; the renderer retains this selector.
            }
            throw error
        }
    },

    'engine:diagnostic-nccl-replacement-status': async (
        payload?: WsInvokeRequest<'engine:diagnostic-nccl-replacement-status'>
    ) => {
        const { selector, record } = await context(payload)
        return status(selector, record)
    },

    'engine:diagnostic-nccl-replacement-close': async (
        payload?: WsInvokeRequest<'engine:diagnostic-nccl-replacement-close'>
    ) => {
        const { selector, record } = await context(payload)
        const result = parseReplacementStatus(
            await call('engine:diagnostic-runtime-status', {
                reviewId: selector.reviewId,
                closeUnstartedReview: true
            }),
            selector,
            record.groupId,
            record.targets
        )
        if (!result.reviewClosed && !result.operation)
            throw new Error('PAIR did not confirm closure of the new NCCL review.')
        return result
    },

    'engine:diagnostic-nccl-replacement-cancel': async (
        payload?: WsInvokeRequest<'engine:diagnostic-nccl-replacement-cancel'>
    ) => {
        const { selector, record } = await context(payload)
        return parseReplacementOperation(
            await call('engine:diagnostic-runtime-cancel', { operationId: selector.operationId }),
            selector,
            record.groupId,
            record.targets
        )
    },

    'engine:diagnostic-nccl-replacement-retry': async (
        payload?: WsInvokeRequest<'engine:diagnostic-nccl-replacement-retry'>
    ) => {
        const raw = payload as Record<string, unknown> | undefined
        const { selector, record } = await context(
            raw && {
                buildOperationId: raw.buildOperationId,
                reviewId: raw.reviewId,
                operationId: raw.operationId
            }
        )
        const expectedRevision = revision(raw?.expectedRevision)
        try {
            const next = parseReplacementOperation(
                await call('engine:diagnostic-runtime-retry', {
                    operationId: selector.operationId,
                    expectedRevision
                }),
                selector,
                record.groupId,
                record.targets
            )
            if (next.revision <= expectedRevision)
                throw new Error('NCCL retry revision did not advance.')
            return next
        } catch (error) {
            const recovered = await status(selector, record)
            if (recovered.operation && recovered.operation.revision > expectedRevision)
                return recovered.operation
            throw error
        }
    },

    'engine:diagnostic-nccl-replacement-adopt': async (
        payload?: WsInvokeRequest<'engine:diagnostic-nccl-replacement-adopt'>
    ) => {
        const raw = payload as Record<string, unknown> | undefined
        const { selector, record } = await context(
            raw && {
                buildOperationId: raw.buildOperationId,
                reviewId: raw.reviewId,
                operationId: raw.operationId
            }
        )
        const expectedRevision = revision(raw?.expectedRevision)
        try {
            return parseReplacementAdoption(
                await call('engine:diagnostic-runtime-adopt', {
                    operationId: selector.operationId,
                    expectedRevision
                }),
                selector,
                record.groupId,
                record.targets,
                expectedRevision
            )
        } catch (error) {
            // The publication reply can be lost after a durable write. Read only
            // the exact new operation and its exact new record; never re-adopt.
            try {
                const current = await status(selector, record)
                if (current.operation?.adopted) {
                    const lookup = await call('engine:diagnostic-managed-runtime', {
                        operationId: selector.operationId
                    })
                    const newRecord = parseReplacementManagedRecord(lookup, selector.operationId)
                    const raw = lookup as {
                        record?: { reviewId?: unknown; approvedRevision?: unknown }
                    }
                    if (
                        newRecord.groupId === record.groupId &&
                        newRecord.targets.every(
                            (target, index) =>
                                target.nodeId === record.targets[index].nodeId &&
                                target.principal === record.targets[index].principal
                        ) &&
                        raw.record?.reviewId === selector.reviewId &&
                        raw.record?.approvedRevision === expectedRevision
                    )
                        return { operation: current.operation, adoptedAt: newRecord.adoptedAt }
                }
            } catch {
                // Preserve the uncertain adoption state in the renderer.
            }
            throw error
        }
    }
}
