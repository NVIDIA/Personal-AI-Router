// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    VllmDistributionRequest,
    VllmExactModelRequest,
    VllmModelReceipt,
    VllmOperationCancelResult,
    VllmProviderMismatch,
    VllmProviderObservation,
    VllmRuntimePrepareRequest,
    VllmRuntimePrepareResult
} from '@/shared/types/vllm-model-journey'
import { exactKeys } from '@/shared/utils/vllm-group'
import { count, digest, flag, identity, object, text } from '@/shared/utils/vllm-group-status'

const operationPattern = /^[0-9a-f]{32}$/
const exactModelPattern =
    /^[A-Za-z0-9][A-Za-z0-9._-]{0,95}\/[A-Za-z0-9][A-Za-z0-9._-]{0,95}@[0-9a-f]{40}$/

async function stableVllmOperationId(domain: string, values: string[]): Promise<string> {
    const encoded = new TextEncoder().encode([`nvpair-vllm-${domain}-v1`, ...values].join('\0'))
    const digest = new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', encoded))
    return Array.from(digest.slice(0, 16), byte => byte.toString(16).padStart(2, '0')).join('')
}

/** Stable pull identity lets the same visible source be cancelled after UI restart. */
export function vllmPullOperationId(nodeId: string, model: string): Promise<string> {
    return stableVllmOperationId('pull', [nodeId, exactVllmModel(model)])
}

/** Stable preparation identity for the fixed Qwen3.8 runtime owner. */
export function vllmPrepareOperationId(nodeId: string): Promise<string> {
    return stableVllmOperationId('qwen38-prepare', [nodeId])
}

/** Stable receive-stage identity: the same source/target/model resumes after UI restart. */
export function vllmDistributionOperationId(
    sourceNodeId: string,
    targetNodeId: string,
    model: string
): Promise<string> {
    return stableVllmOperationId('distribution', [
        sourceNodeId,
        targetNodeId,
        exactVllmModel(model)
    ])
}

function operationId(value: unknown): string {
    if (typeof value !== 'string' || !operationPattern.test(value))
        throw new Error('A 32-character lowercase hexadecimal operationId is required.')
    return value
}

export function exactVllmModel(value: unknown): string {
    if (typeof value !== 'string' || !exactModelPattern.test(value))
        throw new Error('Use an exact public owner/repository@40-character-commit model.')
    return value
}

export function parseVllmExactModelRequest(value: unknown): VllmExactModelRequest {
    const row = exactKeys(value, ['nodeId', 'model', 'operationId'])
    return {
        nodeId: identity(row.nodeId, 'model target node'),
        model: exactVllmModel(row.model),
        operationId: operationId(row.operationId)
    }
}

export function parseVllmDistributionRequest(value: unknown): VllmDistributionRequest {
    const row = exactKeys(value, ['nodeId', 'sourceNodeId', 'model', 'operationId'])
    const request = parseVllmExactModelRequest({
        nodeId: row.nodeId,
        model: row.model,
        operationId: row.operationId
    })
    const sourceNodeId = identity(row.sourceNodeId, 'model source node')
    if (sourceNodeId === request.nodeId)
        throw new Error('Model source and target must be different nodes.')
    return { ...request, sourceNodeId }
}

export function parseVllmRuntimePrepareRequest(value: unknown): VllmRuntimePrepareRequest {
    const row = exactKeys(value, ['nodeId', 'operationId'])
    return {
        nodeId: identity(row.nodeId, 'runtime target node'),
        operationId: operationId(row.operationId)
    }
}

export function parseVllmModelReceipt(value: unknown): VllmModelReceipt {
    const row = object(value, 'retained vLLM model receipt')
    const id = exactVllmModel(row.id)
    const revision = text(row.revision, 'model revision', 40)
    if (id.slice(id.length - 40) !== revision || row.source !== 'huggingface')
        throw new Error('PAIR returned a model receipt for a different immutable source.')
    return {
        id,
        source: 'huggingface',
        revision,
        license: text(row.license, 'model license', 128),
        metadataSha256: digest(row.metadataSha256, 'model metadata digest'),
        digest: digest(row.digest, 'model manifest digest'),
        bytes: count(row.bytes, 'model byte count')
    }
}

export function parseVllmCancelResult(
    value: unknown,
    operation: string
): VllmOperationCancelResult {
    const row = object(value, `${operation} cancellation`)
    const accepted = row.accepted === undefined ? row.cancelled : row.accepted
    return {
        accepted: flag(accepted, `${operation} cancellation state`),
        operationId: operationId(row.operationId)
    }
}

function parseProviderMismatch(value: unknown): VllmProviderMismatch {
    const row = object(value, 'provider mismatch')
    const result: VllmProviderMismatch = {
        name: text(row.name, 'provider name', 128),
        reason: text(row.reason, 'provider mismatch reason', 512)
    }
    if (row.expectedSha256 !== undefined)
        result.expectedSha256 = digest(row.expectedSha256, 'expected provider digest')
    if (row.observedSha256 !== undefined)
        result.observedSha256 = digest(row.observedSha256, 'observed provider digest')
    if (row.expectedBytes !== undefined)
        result.expectedBytes = count(row.expectedBytes, 'expected provider bytes')
    if (row.observedBytes !== undefined)
        result.observedBytes = count(row.observedBytes, 'observed provider bytes')
    return result
}

function parseVllmProviderObservation(value: unknown): VllmProviderObservation {
    const row = object(value, 'provider observation')
    const allowed = row.allowedClosureSha256
    const mismatches = row.mismatches
    if (!Array.isArray(allowed) || !Array.isArray(mismatches))
        throw new Error('PAIR returned an incomplete provider observation.')
    if (allowed.length > 16 || mismatches.length > 64)
        throw new Error('PAIR returned an oversized provider observation.')
    const result: VllmProviderObservation = {
        qualified: flag(row.qualified, 'provider qualification'),
        ...(row.profileId === undefined
            ? {}
            : { profileId: text(row.profileId, 'provider profile', 128) }),
        expectedClosureSha256:
            row.expectedClosureSha256 === ''
                ? ''
                : digest(row.expectedClosureSha256, 'expected provider closure'),
        ...(row.observedClosureSha256 === undefined || row.observedClosureSha256 === ''
            ? {}
            : {
                  observedClosureSha256: digest(
                      row.observedClosureSha256,
                      'observed provider closure'
                  )
              }),
        allowedClosureSha256: allowed.map(value => digest(value, 'allowed provider closure')),
        mismatches: mismatches.map(parseProviderMismatch)
    }
    if (
        result.qualified &&
        (!result.observedClosureSha256 ||
            result.expectedClosureSha256 !== result.observedClosureSha256 ||
            !result.allowedClosureSha256.includes(result.observedClosureSha256) ||
            result.mismatches.length !== 0)
    )
        throw new Error('PAIR returned inconsistent provider qualification evidence.')
    return result
}

export function parseVllmRuntimePrepareResult(
    value: unknown,
    nodeId: string,
    requestedOperationId: string
): VllmRuntimePrepareResult {
    const row = object(value, 'Qwen3.8 runtime preparation')
    const returnedOperationId = operationId(row.operationId)
    if (returnedOperationId !== requestedOperationId)
        throw new Error('PAIR returned a runtime result for a different operation.')
    return {
        nodeId,
        operationId: returnedOperationId,
        state: text(row.state, 'runtime preparation state', 128),
        recipeId: text(row.recipeId, 'runtime recipe', 256),
        recipeSha256: digest(row.recipeSha256, 'runtime recipe digest'),
        runtimeVersion: text(row.runtimeVersion, 'runtime version', 128),
        artifactCount: count(row.artifactCount, 'runtime artifact count'),
        artifactBytes: count(row.artifactBytes, 'runtime artifact bytes'),
        resumed: row.resumed === undefined ? false : flag(row.resumed, 'runtime resume state'),
        ...(row.activeRuntime === undefined
            ? {}
            : { activeRuntime: text(row.activeRuntime, 'active runtime', 256) }),
        ...(row.previousRuntime === undefined
            ? {}
            : { previousRuntime: text(row.previousRuntime, 'previous runtime', 256) }),
        ...(row.message === undefined
            ? {}
            : { message: text(row.message, 'runtime preparation message', 1024) }),
        provider: parseVllmProviderObservation(row.provider)
    }
}
