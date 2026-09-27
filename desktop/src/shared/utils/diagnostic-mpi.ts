// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    DiagnosticMPIApproveRequest,
    DiagnosticMPIFabricRecipe,
    DiagnosticMPIManagedInventory,
    DiagnosticMPIManagedRuntime,
    DiagnosticMPINetwork,
    DiagnosticMPIOperation,
    DiagnosticMPIOperationBinding,
    DiagnosticMPIOperationState,
    DiagnosticMPIRecipe,
    DiagnosticMPIRecovery,
    DiagnosticMPIRecoveryReference,
    DiagnosticMPIReview,
    DiagnosticMPIReviewClosure,
    DiagnosticMPIReviewFabric,
    DiagnosticMPIReviewRequest,
    DiagnosticMPISelection
} from '@/shared/types/diagnostic-mpi'

const idPattern = /^[a-f0-9]{32}$/
const digestPattern = /^[a-f0-9]{64}$/
// OpenSSH SHA-256 fingerprints are one 32-byte digest in canonical,
// unpadded base64. The final character carries four data bits.
const sshFingerprintPattern = /^SHA256:[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]$/
const tokenPattern = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/
const ipv4Pattern = /^(?:0|[1-9][0-9]{0,2})(?:\.(?:0|[1-9][0-9]{0,2})){3}$/
const groupPattern = /^[A-Za-z0-9][A-Za-z0-9_./:-]{0,255}$/
const recipes = new Set<DiagnosticMPIRecipe>([
    'pair-two-spark-nccl-socket-smoke-v1',
    'pair-two-spark-nccl-socket-correctness-v2',
    'pair-three-spark-nccl-socket-correctness-v3'
])
const states = new Set<DiagnosticMPIOperationState>([
    'preparing',
    'running',
    'cancelling',
    'passed',
    'failed',
    'cancelled'
])

function row(value: unknown, label: string): Record<string, unknown> {
    if (!value || typeof value !== 'object' || Array.isArray(value))
        throw new Error(`Invalid managed NCCL ${label}.`)
    return value as Record<string, unknown>
}

function exact(value: Record<string, unknown>, allowed: readonly string[], label: string): void {
    if (Object.keys(value).some(key => !allowed.includes(key)))
        throw new Error(`Invalid managed NCCL ${label} fields.`)
}

function required(value: Record<string, unknown>, fields: readonly string[], label: string): void {
    if (fields.some(field => !(field in value)))
        throw new Error(`Incomplete managed NCCL ${label}.`)
}

function identity(value: unknown, label: string, pattern = tokenPattern): string {
    if (typeof value !== 'string' || !pattern.test(value))
        throw new Error(`Invalid managed NCCL ${label}.`)
    return value
}

function ipv4(value: unknown, label: string): string {
    const result = identity(value, label, ipv4Pattern)
    if (result.split('.').some(part => Number(part) > 255))
        throw new Error(`Invalid managed NCCL ${label}.`)
    return result
}

function integer(value: unknown, label: string, minimum = 0): number {
    if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum)
        throw new Error(`Invalid managed NCCL ${label}.`)
    return value
}

function finite(value: unknown, label: string): number {
    if (typeof value !== 'number' || !Number.isFinite(value) || value < 0)
        throw new Error(`Invalid managed NCCL ${label}.`)
    return value
}

function boolean(value: unknown, label: string): boolean {
    if (typeof value !== 'boolean') throw new Error(`Invalid managed NCCL ${label}.`)
    return value
}

function operationMessage(state: DiagnosticMPIOperationState, cleanupConfirmed: boolean): string {
    switch (state) {
        case 'preparing':
            return 'PAIR is checking the selected managed NCCL participants.'
        case 'running':
            return 'PAIR is running the bounded Socket NCCL correctness smoke.'
        case 'cancelling':
            return 'PAIR is cancelling owned NCCL ranks and confirming cleanup.'
        case 'passed':
            return 'NCCL correctness smoke passed; owned MPI cleanup is confirmed.'
        case 'cancelled':
            return 'NCCL correctness smoke was cancelled; owned MPI cleanup is confirmed.'
        case 'failed':
            return cleanupConfirmed
                ? 'NCCL correctness smoke failed; owned MPI cleanup is confirmed.'
                : 'NCCL correctness smoke failed; owned MPI cleanup is not confirmed and recovery is required.'
    }
}

function recipe(value: unknown): DiagnosticMPIRecipe {
    if (value === undefined) return 'pair-two-spark-nccl-socket-smoke-v1'
    if (typeof value !== 'string' || !recipes.has(value as DiagnosticMPIRecipe))
        throw new Error('Invalid managed NCCL recipe.')
    return value as DiagnosticMPIRecipe
}

function participants(value: unknown, label: string): string[] {
    if (!Array.isArray(value) || value.length < 2 || value.length > 3)
        throw new Error(`Managed NCCL ${label} requires exactly two or three nodes.`)
    const result = value.map(nodeId => identity(nodeId, `${label} node`)).sort()
    if (new Set(result).size !== result.length)
        throw new Error(`Managed NCCL ${label} contains duplicate nodes.`)
    return result
}

export function parseDiagnosticMPISelection(value: unknown): DiagnosticMPISelection {
    const input = row(value, 'selection')
    exact(input, ['buildOperationId', 'nodeIds'], 'selection')
    required(input, ['buildOperationId', 'nodeIds'], 'selection')
    return {
        buildOperationId: identity(input.buildOperationId, 'build identity', idPattern),
        nodeIds: participants(input.nodeIds, 'selection')
    }
}

function network(value: unknown): DiagnosticMPINetwork {
    if (value === 'management' || value === 'fabric') return value
    throw new Error('Invalid managed NCCL network.')
}

export function parseDiagnosticMPIReviewRequest(value: unknown): DiagnosticMPIReviewRequest {
    const input = row(value, 'review request')
    exact(input, ['buildOperationId', 'nodeIds', 'network'], 'review request')
    required(input, ['buildOperationId', 'nodeIds', 'network'], 'review request')
    return {
        ...parseDiagnosticMPISelection({
            buildOperationId: input.buildOperationId,
            nodeIds: input.nodeIds
        }),
        network: network(input.network)
    }
}

function fabricRecipe(value: unknown, nodes: number): DiagnosticMPIFabricRecipe {
    if (value === 'spark-two-node-temporary-addresses-v1' && nodes === 2) return value
    if (value === 'spark-three-node-ring-routed-v2' && nodes === 3) return value
    throw new Error('Managed NCCL fabric recipe does not match the reviewed node count.')
}

function parseReviewFabric(value: unknown, nodes: number): DiagnosticMPIReviewFabric {
    const fabric = row(value, 'review fabric')
    exact(fabric, ['operationId', 'qualificationDigest', 'recipeId'], 'review fabric')
    required(fabric, ['operationId', 'qualificationDigest', 'recipeId'], 'review fabric')
    return {
        operationId: identity(fabric.operationId, 'fabric operation identity', idPattern),
        qualificationDigest: identity(
            fabric.qualificationDigest,
            'fabric qualification',
            digestPattern
        ),
        recipeId: fabricRecipe(fabric.recipeId, nodes)
    }
}

function parseManagedTarget(value: unknown): { nodeId: string; local: boolean } {
    const target = row(value, 'managed target')
    exact(
        target,
        [
            'nodeId',
            'principal',
            'local',
            'address',
            'clusterPinSha256',
            'candidateId',
            'sshHostKeySha256',
            'planDigest',
            'attempt',
            'artifactObservedAt',
            'registration'
        ],
        'managed target'
    )
    required(
        target,
        [
            'nodeId',
            'principal',
            'local',
            'address',
            'clusterPinSha256',
            'planDigest',
            'attempt',
            'artifactObservedAt',
            'registration'
        ],
        'managed target'
    )
    const nodeId = identity(target.nodeId, 'managed target identity')
    identity(target.principal, 'managed target principal')
    identity(target.clusterPinSha256, 'managed target pin', digestPattern)
    identity(target.planDigest, 'managed target plan', digestPattern)
    integer(target.attempt, 'managed target attempt', 1)
    if (typeof target.address !== 'string' || target.address.length > 512)
        throw new Error('Invalid managed NCCL target address.')
    if (typeof target.artifactObservedAt !== 'string' || target.artifactObservedAt.length > 128)
        throw new Error('Invalid managed NCCL target observation time.')
    row(target.registration, 'managed target registration')
    if (target.candidateId !== undefined) identity(target.candidateId, 'managed candidate')
    if (target.sshHostKeySha256 !== undefined)
        identity(target.sshHostKeySha256, 'managed SSH host key', sshFingerprintPattern)
    return { nodeId, local: boolean(target.local, 'managed target locality') }
}

function parseManagedRuntime(value: unknown): DiagnosticMPIManagedRuntime {
    const record = row(value, 'managed runtime')
    exact(
        record,
        [
            'schemaVersion',
            'owner',
            'operationId',
            'reviewId',
            'groupId',
            'approvedRevision',
            'adoptedAt',
            'adopted',
            'runtimeValidated',
            'runAvailable',
            'targets'
        ],
        'managed runtime'
    )
    required(
        record,
        [
            'schemaVersion',
            'owner',
            'operationId',
            'reviewId',
            'groupId',
            'approvedRevision',
            'adoptedAt',
            'adopted',
            'runtimeValidated',
            'runAvailable',
            'targets'
        ],
        'managed runtime'
    )
    if (record.schemaVersion !== 1 || record.owner !== 'pair-managed-nccl-registry-v1')
        throw new Error('Invalid managed NCCL registry ownership.')
    identity(record.reviewId, 'managed build review identity', idPattern)
    identity(record.groupId, 'managed build group identity', groupPattern)
    integer(record.approvedRevision, 'managed build revision', 1)
    if (!Array.isArray(record.targets)) throw new Error('Invalid managed NCCL target roster.')
    const targets = record.targets.map(parseManagedTarget)
    participants(
        targets.map(target => target.nodeId),
        'managed roster'
    )
    return {
        buildOperationId: identity(record.operationId, 'managed build identity', idPattern),
        adoptedAt: integer(record.adoptedAt, 'managed build adoption time', 1),
        adopted: boolean(record.adopted, 'managed build adoption'),
        runtimeValidated: boolean(record.runtimeValidated, 'managed runtime validation'),
        runAvailable: boolean(record.runAvailable, 'managed runtime availability'),
        targets
    }
}

export function parseDiagnosticMPIManagedInventory(value: unknown): DiagnosticMPIManagedInventory {
    const inventory = row(value, 'managed inventory')
    exact(inventory, ['records', 'recoveryRequired'], 'managed inventory')
    required(inventory, ['records', 'recoveryRequired'], 'managed inventory')
    if (!Array.isArray(inventory.records) || inventory.records.length > 64)
        throw new Error('Invalid managed NCCL registry inventory.')
    const records = inventory.records.map(parseManagedRuntime)
    if (new Set(records.map(record => record.buildOperationId)).size !== records.length)
        throw new Error('Managed NCCL inventory contains duplicate builds.')
    return {
        records,
        recoveryRequired: boolean(inventory.recoveryRequired, 'inventory recovery state')
    }
}

function parseReviewTarget(value: unknown): DiagnosticMPIReview['targets'][number] {
    const target = row(value, 'review target')
    exact(target, ['nodeId', 'interface', 'address', 'sshAddress'], 'review target')
    required(target, ['nodeId', 'interface', 'address', 'sshAddress'], 'review target')
    const address = ipv4(target.address, 'review address')
    ipv4(target.sshAddress, 'review SSH address')
    return {
        nodeId: identity(target.nodeId, 'review target identity'),
        interface: identity(target.interface, 'review interface'),
        address
    }
}

export function parseDiagnosticMPIReview(value: unknown): DiagnosticMPIReview {
    const review = row(value, 'review')
    exact(
        review,
        [
            'reviewId',
            'buildOperationId',
            'operationId',
            'groupId',
            'ownerNodeId',
            'network',
            'transport',
            'recipeId',
            'fabric',
            'expiresAt',
            'targets'
        ],
        'review'
    )
    required(
        review,
        [
            'reviewId',
            'buildOperationId',
            'operationId',
            'groupId',
            'ownerNodeId',
            'network',
            'transport',
            'recipeId',
            'expiresAt',
            'targets'
        ],
        'review'
    )
    const reviewNetwork = network(review.network)
    if (review.transport !== 'socket')
        throw new Error('Managed NCCL review changed its fixed Socket transport.')
    if ((reviewNetwork === 'fabric') !== (review.fabric !== undefined))
        throw new Error('Managed NCCL review changed its fabric binding.')
    if (!Array.isArray(review.targets)) throw new Error('Invalid managed NCCL review roster.')
    const targets = review.targets.map(parseReviewTarget)
    const operationId = identity(review.operationId, 'operation identity', idPattern)
    const memberNodeIds = participants(
        targets.map(target => target.nodeId),
        'review roster'
    )
    const ownerNodeId = identity(review.ownerNodeId, 'owner identity')
    if (ownerNodeId !== memberNodeIds[0] || review.groupId !== `pair-smoke-${operationId}`)
        throw new Error('Managed NCCL review changed its owner binding.')
    const recipeId = recipe(review.recipeId)
    if ((memberNodeIds.length === 3) !== recipeId.includes('three-spark'))
        throw new Error('Managed NCCL recipe changed its participant count.')
    return {
        reviewId: identity(review.reviewId, 'review identity', idPattern),
        buildOperationId: identity(review.buildOperationId, 'build identity', idPattern),
        operationId,
        groupId: review.groupId as string,
        ownerNodeId,
        network: reviewNetwork,
        transport: 'socket',
        recipeId,
        ...(reviewNetwork === 'fabric'
            ? { fabric: parseReviewFabric(review.fabric, memberNodeIds.length) }
            : {}),
        expiresAt: integer(review.expiresAt, 'review expiry', 1),
        targets
    }
}

function parseSample(value: unknown) {
    const sample = row(value, 'sample')
    exact(sample, ['bytes', 'algorithmGBps', 'busGBps', 'latencyUs', 'wrong'], 'sample')
    required(sample, ['bytes', 'algorithmGBps', 'busGBps', 'latencyUs', 'wrong'], 'sample')
    return {
        bytes: integer(sample.bytes, 'sample size', 1),
        algorithmGBps: finite(sample.algorithmGBps, 'sample algorithm rate'),
        busGBps: finite(sample.busGBps, 'sample bus rate'),
        latencyUs: finite(sample.latencyUs, 'sample latency'),
        wrong: integer(sample.wrong, 'sample correctness count')
    }
}

export function parseDiagnosticMPIOperation(value: unknown): DiagnosticMPIOperation {
    const operation = row(value, 'operation')
    exact(
        operation,
        [
            'operationId',
            'groupId',
            'ownerNodeId',
            'preset',
            'recipeId',
            'state',
            'startedAt',
            'finishedAt',
            'message',
            'cleanupConfirmed',
            'memberNodeIds',
            'samples'
        ],
        'operation'
    )
    required(
        operation,
        [
            'operationId',
            'groupId',
            'ownerNodeId',
            'preset',
            'state',
            'startedAt',
            'message',
            'cleanupConfirmed',
            'memberNodeIds'
        ],
        'operation'
    )
    const operationId = identity(operation.operationId, 'operation identity', idPattern)
    const memberNodeIds = participants(operation.memberNodeIds, 'operation roster')
    const ownerNodeId = identity(operation.ownerNodeId, 'operation owner')
    if (
        ownerNodeId !== memberNodeIds[0] ||
        operation.groupId !== `pair-smoke-${operationId}` ||
        operation.preset !== 'nccl-smoke' ||
        typeof operation.state !== 'string' ||
        !states.has(operation.state as DiagnosticMPIOperationState)
    )
        throw new Error('Managed NCCL operation changed its fixed binding.')
    const recipeId = recipe(operation.recipeId)
    if ((memberNodeIds.length === 3) !== recipeId.includes('three-spark'))
        throw new Error('Managed NCCL operation recipe changed its participant count.')
    const samples = operation.samples === undefined ? [] : operation.samples
    if (!Array.isArray(samples) || samples.length > 64)
        throw new Error('Invalid managed NCCL sample set.')
    const state = operation.state as DiagnosticMPIOperationState
    const cleanupConfirmed = boolean(operation.cleanupConfirmed, 'cleanup confirmation')
    const terminal = ['passed', 'failed', 'cancelled'].includes(state)
    if (typeof operation.message !== 'string' || operation.message.length > 2048)
        throw new Error('Invalid managed NCCL backend message shape.')
    if ((operation.finishedAt !== undefined) !== terminal)
        throw new Error('Managed NCCL operation has an inconsistent terminal receipt.')
    if ((terminal && state !== 'failed' && !cleanupConfirmed) || (!terminal && cleanupConfirmed))
        throw new Error('Managed NCCL operation has an inconsistent cleanup receipt.')
    return {
        operationId,
        groupId: operation.groupId as string,
        ownerNodeId,
        preset: 'nccl-smoke',
        recipeId,
        state,
        startedAt: integer(operation.startedAt, 'operation start time', 1),
        ...(operation.finishedAt === undefined
            ? {}
            : { finishedAt: integer(operation.finishedAt, 'operation finish time', 1) }),
        message: operationMessage(state, cleanupConfirmed),
        cleanupConfirmed,
        memberNodeIds,
        samples: samples.map(parseSample)
    }
}

export function parseDiagnosticMPIOperationBinding(value: unknown): DiagnosticMPIOperationBinding {
    const binding = row(value, 'operation selector')
    exact(
        binding,
        ['operationId', 'groupId', 'ownerNodeId', 'memberNodeIds', 'recipeId'],
        'operation selector'
    )
    required(
        binding,
        ['operationId', 'groupId', 'ownerNodeId', 'memberNodeIds', 'recipeId'],
        'operation selector'
    )
    const operationId = identity(binding.operationId, 'operation selector identity', idPattern)
    const memberNodeIds = participants(binding.memberNodeIds, 'operation selector')
    const ownerNodeId = identity(binding.ownerNodeId, 'operation selector owner')
    const recipeId = recipe(binding.recipeId)
    if (binding.groupId !== `pair-smoke-${operationId}` || ownerNodeId !== memberNodeIds[0])
        throw new Error('Managed NCCL operation selector changed its owner binding.')
    if ((memberNodeIds.length === 3) !== recipeId.includes('three-spark'))
        throw new Error('Managed NCCL operation selector changed its recipe binding.')
    return {
        operationId,
        groupId: binding.groupId as string,
        ownerNodeId,
        memberNodeIds,
        recipeId
    }
}

export function parseDiagnosticMPIApproveRequest(value: unknown): DiagnosticMPIApproveRequest {
    const request = row(value, 'approval')
    exact(
        request,
        ['reviewId', 'operationId', 'groupId', 'ownerNodeId', 'memberNodeIds', 'recipeId'],
        'approval'
    )
    required(
        request,
        ['reviewId', 'operationId', 'groupId', 'ownerNodeId', 'memberNodeIds', 'recipeId'],
        'approval'
    )
    return {
        reviewId: identity(request.reviewId, 'approval review identity', idPattern),
        ...parseDiagnosticMPIOperationBinding({
            operationId: request.operationId,
            groupId: request.groupId,
            ownerNodeId: request.ownerNodeId,
            memberNodeIds: request.memberNodeIds,
            recipeId: request.recipeId
        })
    }
}

export function parseDiagnosticMPIRecoveryReference(
    value: unknown
): DiagnosticMPIRecoveryReference {
    const reference = row(value, 'recovery reference')
    exact(
        reference,
        [
            'reviewId',
            'buildOperationId',
            'operationId',
            'groupId',
            'ownerNodeId',
            'memberNodeIds',
            'recipeId'
        ],
        'recovery reference'
    )
    required(
        reference,
        ['reviewId', 'buildOperationId', 'operationId', 'groupId', 'ownerNodeId', 'memberNodeIds'],
        'recovery reference'
    )
    const binding = parseDiagnosticMPIApproveRequest({
        reviewId: reference.reviewId,
        operationId: reference.operationId,
        groupId: reference.groupId,
        ownerNodeId: reference.ownerNodeId,
        memberNodeIds: reference.memberNodeIds,
        recipeId: reference.recipeId
    })
    const recipeId = recipe(reference.recipeId)
    if ((binding.memberNodeIds.length === 3) !== recipeId.includes('three-spark'))
        throw new Error('Managed NCCL recovery recipe changed its participant count.')
    return {
        ...binding,
        buildOperationId: identity(
            reference.buildOperationId,
            'recovery build identity',
            idPattern
        ),
        recipeId
    }
}

export function parseDiagnosticMPIRecovery(value: unknown): DiagnosticMPIRecovery {
    const recovery = row(value, 'recovery result')
    exact(recovery, ['reference', 'operation', 'recoveryRequired'], 'recovery result')
    required(recovery, ['reference', 'operation', 'recoveryRequired'], 'recovery result')
    const reference =
        recovery.reference === null ? null : parseDiagnosticMPIRecoveryReference(recovery.reference)
    const operation =
        recovery.operation === null ? null : parseDiagnosticMPIOperation(recovery.operation)
    if (!reference && operation)
        throw new Error('Managed NCCL recovery operation lacks its original reference.')
    if (
        reference &&
        operation &&
        (operation.operationId !== reference.operationId ||
            operation.groupId !== reference.groupId ||
            operation.ownerNodeId !== reference.ownerNodeId ||
            operation.memberNodeIds.join('\n') !== reference.memberNodeIds.join('\n') ||
            operation.recipeId !== reference.recipeId)
    )
        throw new Error('Managed NCCL recovery operation changed its original reference.')
    return {
        reference,
        operation,
        recoveryRequired: boolean(recovery.recoveryRequired, 'recovery certainty')
    }
}

export function parseDiagnosticMPIReviewClosure(value: unknown): DiagnosticMPIReviewClosure {
    const closure = row(value, 'review closure')
    exact(closure, ['operation', 'reviewClosed'], 'review closure')
    required(closure, ['operation', 'reviewClosed'], 'review closure')
    const operation =
        closure.operation === null ? null : parseDiagnosticMPIOperation(closure.operation)
    const reviewClosed = boolean(closure.reviewClosed, 'review closure confirmation')
    if ((operation !== null) === reviewClosed)
        throw new Error('Managed NCCL review closure is not definitive.')
    return { operation, reviewClosed }
}

export function bindDiagnosticMPIOperation(
    operation: DiagnosticMPIOperation,
    binding: DiagnosticMPIOperationBinding
): DiagnosticMPIOperation {
    if (
        operation.operationId !== binding.operationId ||
        operation.groupId !== binding.groupId ||
        operation.ownerNodeId !== binding.ownerNodeId ||
        operation.memberNodeIds.join('\n') !== binding.memberNodeIds.join('\n') ||
        operation.recipeId !== binding.recipeId
    )
        throw new Error('PAIR returned a managed NCCL operation for a different selection.')
    return operation
}
