// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    NCCLReplacementAdoption,
    NCCLReplacementOperation,
    NCCLReplacementReview,
    NCCLReplacementSelector,
    NCCLReplacementStatus,
    NCCLReplacementTarget
} from '@/shared/types/diagnostic-runtime-replacement'

const id = /^[a-f0-9]{32}$/
const digest = /^[a-f0-9]{64}$/
const token = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/
const group = /^pair-recipe\/nccl-[a-f0-9]{32}$/
const hostKey = /^SHA256:[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]$/
const states = new Set(['running', 'cancelling', 'completed', 'failed', 'cancelled', 'interrupted'])

function invalid(message: string): never {
    throw new Error(`Invalid NCCL replacement ${message}.`)
}

function object(value: unknown, label: string): Record<string, unknown> {
    if (!value || typeof value !== 'object' || Array.isArray(value)) invalid(label)
    return value as Record<string, unknown>
}

function string(value: unknown, label: string, pattern: RegExp): string {
    if (typeof value !== 'string' || !pattern.test(value)) invalid(label)
    return value
}

function text(value: unknown, label: string, max = 2048): string {
    if (
        typeof value !== 'string' ||
        value.length > max ||
        Array.from(value).some(character => character.charCodeAt(0) < 32)
    )
        invalid(label)
    return value
}

function number(value: unknown, label: string, minimum = 0): number {
    if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) invalid(label)
    return value
}

function bool(value: unknown, label: string): boolean {
    if (typeof value !== 'boolean') invalid(label)
    return value
}

function roster(value: unknown, label: string): Record<string, unknown>[] {
    if (!Array.isArray(value) || value.length < 2 || value.length > 3) invalid(label)
    const rows = value.map(item => object(item, label))
    const seen = new Set<string>()
    const principals = new Set<string>()
    for (const row of rows) {
        const nodeId = string(row.nodeId, `${label} node`, token)
        const principal = string(row.principal, `${label} principal`, token)
        if (seen.has(nodeId) || principals.has(principal)) invalid(`${label} duplicate identity`)
        seen.add(nodeId)
        principals.add(principal)
    }
    if (
        rows.some(
            (row, index) =>
                index > 0 && (rows[index - 1].nodeId as string) >= (row.nodeId as string)
        )
    )
        invalid(`${label} order`)
    return rows
}

/** Raw record stays in Electron. It is never a channel response. */
export function parseReplacementManagedRecord(value: unknown, expectedBuildOperationId: string) {
    const lookup = object(value, 'managed lookup')
    const record = object(lookup.record, 'managed record')
    if (
        record.schemaVersion !== 1 ||
        record.owner !== 'pair-managed-nccl-registry-v1' ||
        record.operationId !== string(expectedBuildOperationId, 'adopted build', id) ||
        record.adopted !== true ||
        record.runtimeValidated !== false ||
        record.runAvailable !== false ||
        'binding' in record
    )
        invalid('managed ownership')
    string(record.reviewId, 'managed review', id)
    string(record.groupId, 'managed group', group)
    number(record.approvedRevision, 'managed revision', 1)
    const adoptedAt = number(record.adoptedAt, 'managed adoption time', 1)
    const targets = roster(record.targets, 'managed roster').map(row => {
        const registration = object(row.registration, 'managed registration')
        const identity = object(registration.identity, 'managed registration identity')
        const nodeId = string(row.nodeId, 'managed node', token)
        const principal = string(row.principal, 'managed principal', token)
        const planDigest = string(row.planDigest, 'managed plan digest', digest)
        const local = bool(row.local, 'managed locality')
        if (
            principal !== nodeId ||
            typeof row.address !== 'string' ||
            !row.address ||
            row.address.length > 256 ||
            (local
                ? row.candidateId !== undefined || row.sshHostKeySha256 !== undefined
                : typeof row.candidateId !== 'string' ||
                  !token.test(row.candidateId) ||
                  typeof row.sshHostKeySha256 !== 'string' ||
                  !hostKey.test(row.sshHostKeySha256)) ||
            registration.schemaVersion !== 1 ||
            registration.kind !== 'pair-nccl-runtime-candidate-v1' ||
            registration.operationId !== expectedBuildOperationId ||
            registration.planDigest !== planDigest ||
            registration.managerAdopted !== false ||
            registration.runtimeValidated !== false ||
            registration.gpuExecuted !== false ||
            registration.mpiExecuted !== false ||
            registration.linkValidation !== 'static-elf-resolution-only' ||
            identity.nodeId !== nodeId ||
            identity.principal !== principal
        )
            invalid('managed target binding')
        string(row.clusterPinSha256, 'managed pin', digest)
        if (number(row.attempt, 'managed attempt', 1) > 3) invalid('managed attempt')
        if (
            typeof row.artifactObservedAt !== 'string' ||
            !/^\d{4}-\d{2}-\d{2}T/.test(row.artifactObservedAt) ||
            !Number.isFinite(Date.parse(row.artifactObservedAt))
        )
            invalid('managed artifact observation')
        return {
            nodeId,
            principal,
            ...(local
                ? {}
                : {
                      candidateId: row.candidateId as string,
                      sshHostKeySha256: row.sshHostKeySha256 as string
                  })
        }
    })
    return { groupId: record.groupId as string, adoptedAt, targets }
}

export function parseReplacementSelector(value: unknown): NCCLReplacementSelector {
    const row = object(value, 'selector')
    if (
        Object.keys(row).some(key => !['buildOperationId', 'reviewId', 'operationId'].includes(key))
    )
        invalid('selector fields')
    const buildOperationId = string(row.buildOperationId, 'adopted build', id)
    const operationId = string(row.operationId, 'new operation', id)
    if (buildOperationId === operationId) invalid('old operation reuse')
    return {
        buildOperationId,
        reviewId: string(row.reviewId, 'new review', id),
        operationId
    }
}

export function parseReplacementBuild(value: unknown): string {
    const row = object(value, 'build selector')
    if (Object.keys(row).length !== 1) invalid('build selector fields')
    return string(row.buildOperationId, 'adopted build', id)
}

function sameRoster(
    actual: Record<string, unknown>[],
    trusted: { nodeId: string; principal: string }[]
) {
    if (
        actual.length !== trusted.length ||
        actual.some(
            (row, index) =>
                row.nodeId !== trusted[index].nodeId || row.principal !== trusted[index].principal
        )
    )
        invalid('roster binding')
}

function target(value: Record<string, unknown>, operationId: string): NCCLReplacementTarget {
    const nodeId = string(value.nodeId, 'target node', token)
    const state = text(value.state, 'target state', 64)
    if (typeof value.cleanupConfirmed !== 'boolean') invalid('target cleanup')
    const review = value.review === undefined ? undefined : object(value.review, 'target review')
    const receipt =
        value.receipt === undefined ? undefined : object(value.receipt, 'target receipt')
    const rawPlan = review?.plan
    let plan: NCCLReplacementTarget['plan']
    if (review?.state === 'reviewed') {
        const sourcePlan = object(rawPlan, 'target plan')
        const sources = object(sourcePlan.sources, 'target sources')
        const nccl = object(sources.nccl, 'NCCL source')
        const tests = object(sources['nccl-tests'], 'NCCL tests source')
        const limits = object(sourcePlan.limits, 'target limits')
        if (
            sourcePlan.schemaVersion !== 1 ||
            sourcePlan.recipeId !== 'dgx-spark-nccl-cuda13-sm121-user-build-v2' ||
            sourcePlan.operationId !== operationId ||
            !digest.test(String(sourcePlan.planDigest)) ||
            limits.parallelJobs !== 2 ||
            limits.maxBuildSeconds !== 1800 ||
            limits.maxAttempts !== 3 ||
            nccl.url !== 'https://github.com/NVIDIA/nccl.git' ||
            nccl.commit !== '73cf112295c33aee2b895f329f592f2a9b4b0f97' ||
            nccl.tag !== 'v2.30.7-1' ||
            tests.url !== 'https://github.com/NVIDIA/nccl-tests.git' ||
            tests.commit !== 'b4d5beebca8a76cf01335f724d154b9b9d394d96' ||
            tests.tag !== 'v2.20.0' ||
            sourcePlan.gpuExecuted !== false ||
            sourcePlan.mpiExecuted !== false ||
            sourcePlan.managerAdopted !== false ||
            sourcePlan.runtimeValidated !== false
        )
            invalid('target source or CPU plan')
        const identity = object(sourcePlan.identity, 'plan identity')
        if (identity.nodeId !== value.nodeId || identity.principal !== value.principal)
            invalid('plan identity binding')
        plan = {
            nccl: 'v2.30.7-1 · 73cf1122',
            ncclTests: 'v2.20.0 · b4d5beeb',
            parallelJobs: 2,
            maxBuildSeconds: 1800,
            maxAttempts: 3
        }
    }
    if (receipt && receipt.operationId !== null && receipt.operationId !== operationId)
        invalid('target receipt identity')
    if (state === 'built') {
        const registration = object(receipt?.registration, 'built registration')
        const sourcePlan = object(rawPlan, 'built plan')
        if (
            !plan ||
            receipt?.state !== 'built' ||
            receipt?.artifactsValidated !== true ||
            receipt?.cleanupConfirmed !== true ||
            receipt?.planDigest !== sourcePlan.planDigest ||
            registration.operationId !== operationId ||
            registration.planDigest !== sourcePlan.planDigest ||
            registration.managerAdopted !== false ||
            registration.runtimeValidated !== false ||
            registration.gpuExecuted !== false ||
            registration.mpiExecuted !== false
        )
            invalid('built artifact binding')
    }
    return {
        nodeId,
        state,
        ...(value.reason === undefined ? {} : { reason: text(value.reason, 'target reason') }),
        attempt: value.attempt === undefined ? 0 : number(value.attempt, 'target attempt'),
        cleanupConfirmed: bool(value.cleanupConfirmed, 'target cleanup'),
        ...(plan ? { plan } : {}),
        retryClosed: receipt?.operationClosed === true
    }
}

export function parseReplacementReview(
    value: unknown,
    buildOperationId: string,
    expectedGroupId: string,
    trusted: { nodeId: string; principal: string }[]
): NCCLReplacementReview {
    const row = object(value, 'review')
    const rawTargets = roster(row.targets, 'review roster')
    sameRoster(rawTargets, trusted)
    const reviewId = string(row.reviewId, 'new review', id)
    const operationId = string(row.operationId, 'new operation', id)
    if (operationId === buildOperationId || row.groupId !== expectedGroupId)
        invalid('review identity binding')
    const canBuild = bool(row.canBuild, 'review admission')
    const targets = rawTargets.map(item => target(item, operationId))
    if (canBuild && targets.some(item => item.state !== 'reviewed' || !item.plan))
        invalid('review admission evidence')
    return {
        buildOperationId,
        reviewId,
        operationId,
        groupId: expectedGroupId,
        expiresAt: number(row.expiresAt, 'review expiry', 1),
        canBuild,
        targets
    }
}

export function parseReplacementOperation(
    value: unknown,
    selector: NCCLReplacementSelector,
    expectedGroupId: string,
    trusted: { nodeId: string; principal: string }[]
): NCCLReplacementOperation {
    const row = object(value, 'operation')
    const rawTargets = roster(row.targets, 'operation roster')
    sameRoster(rawTargets, trusted)
    if (
        row.operationId !== selector.operationId ||
        row.reviewId !== selector.reviewId ||
        row.groupId !== expectedGroupId ||
        typeof row.state !== 'string' ||
        !states.has(row.state) ||
        row.runtimeValidated !== false
    )
        invalid('operation binding')
    const targets = rawTargets.map(item => target(item, selector.operationId))
    const cleanupConfirmed = bool(row.cleanupConfirmed, 'operation cleanup')
    const state = row.state as NCCLReplacementOperation['state']
    if (
        (cleanupConfirmed && targets.some(item => !item.cleanupConfirmed)) ||
        (state === 'completed' &&
            (!cleanupConfirmed || targets.some(item => item.state !== 'built' || !item.plan)))
    )
        invalid('operation completion evidence')
    return {
        ...selector,
        groupId: expectedGroupId,
        state,
        stage: text(row.stage, 'operation stage', 128),
        revision: number(row.revision, 'operation revision', 1),
        startedAt: number(row.startedAt, 'operation start', 1),
        ...(row.finishedAt === undefined
            ? {}
            : { finishedAt: number(row.finishedAt, 'operation finish', 1) }),
        targets,
        cleanupConfirmed,
        adopted: bool(row.adopted, 'operation adoption'),
        retrySourceStatus:
            row.retrySourceStatus === 'current' || row.retrySourceStatus === 'changed'
                ? row.retrySourceStatus
                : 'unknown'
    }
}

export function parseReplacementStatus(
    value: unknown,
    selector: NCCLReplacementSelector,
    groupId: string,
    trusted: { nodeId: string; principal: string }[]
): NCCLReplacementStatus {
    const row = object(value, 'status')
    const operation =
        row.operation === null
            ? null
            : parseReplacementOperation(row.operation, selector, groupId, trusted)
    const recoveryRequired = bool(row.recoveryRequired, 'recovery state')
    if (row.reviewClosed !== undefined && typeof row.reviewClosed !== 'boolean')
        invalid('review closure')
    if (row.reviewClosed === true && (operation || recoveryRequired))
        invalid('review closure evidence')
    return {
        operation,
        recoveryRequired,
        ...(row.reviewClosed === undefined ? {} : { reviewClosed: row.reviewClosed as boolean })
    }
}

export function parseReplacementAdoption(
    value: unknown,
    selector: NCCLReplacementSelector,
    groupId: string,
    trusted: { nodeId: string; principal: string }[],
    expectedRevision: number
): NCCLReplacementAdoption {
    const row = object(value, 'adoption')
    const operation = parseReplacementOperation(row.operation, selector, groupId, trusted)
    const record = object(row.record, 'new managed record')
    const adopted = parseReplacementManagedRecord({ record }, selector.operationId)
    if (
        !operation.adopted ||
        operation.state !== 'completed' ||
        !operation.cleanupConfirmed ||
        record.reviewId !== selector.reviewId ||
        record.groupId !== groupId ||
        record.approvedRevision !== expectedRevision ||
        adopted.targets.some(
            (item, index) =>
                item.nodeId !== trusted[index].nodeId || item.principal !== trusted[index].principal
        )
    )
        invalid('adoption binding')
    return { operation, adoptedAt: number(record.adoptedAt, 'adoption time', 1) }
}
