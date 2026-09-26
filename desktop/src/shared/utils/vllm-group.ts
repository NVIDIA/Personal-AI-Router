// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    VllmGroupCheck,
    VllmGroupCheckParticipant,
    VllmGroupElevation,
    VllmGroupOperation,
    VllmGroupReconcileRequest,
    VllmGroupReview,
    VllmGroupReviewRequest,
    VllmGroupSelection,
    VllmGroupStartRequest
} from '@/shared/types/vllm-group'
import {
    count,
    digest,
    flag,
    identity,
    object,
    operationId,
    parseVllmGroupPlan,
    participants,
    text
} from '@/shared/utils/vllm-group-status'

const reviewIdPattern = /^[0-9a-f]{32}$/

/**
 * Exact-shape request gate. Unknown keys are refused so a renderer can never
 * smuggle credentials, commands, paths or authority alongside a typed request.
 */
export function exactKeys(
    payload: unknown,
    keys: readonly string[],
    optional: readonly string[] = []
): Record<string, unknown> {
    if (
        !payload ||
        typeof payload !== 'object' ||
        Array.isArray(payload) ||
        Object.keys(payload).some(key => !keys.includes(key) && !optional.includes(key)) ||
        keys.some(key => !(key in payload))
    )
        throw new Error('Invalid serving-group request.')
    return payload as Record<string, unknown>
}

/** Two or three distinct current node identities plus one exact model; nothing else. */
export function parseVllmGroupSelection(value: unknown): VllmGroupSelection {
    const row = exactKeys(value, ['nodeIds', 'model'], ['parallelism'])
    const nodeIds = row.nodeIds
    if (
        !Array.isArray(nodeIds) ||
        nodeIds.length < 2 ||
        nodeIds.length > 3 ||
        new Set(nodeIds).size !== nodeIds.length ||
        nodeIds.some(
            id =>
                typeof id !== 'string' ||
                !id ||
                id.length > 128 ||
                Array.from(id).some(char => char.charCodeAt(0) <= 32 || char.charCodeAt(0) === 127)
        ) ||
        typeof row.model !== 'string' ||
        !row.model ||
        row.model.length > 512 ||
        (row.parallelism !== undefined &&
            row.parallelism !== 'tensor' &&
            row.parallelism !== 'pipeline')
    )
        throw new Error('Select two or three nodes and one exact downloaded model.')
    return {
        nodeIds: nodeIds as string[],
        model: row.model,
        ...(row.parallelism === undefined
            ? {}
            : { parallelism: row.parallelism as 'tensor' | 'pipeline' })
    }
}

function parseVllmGroupElevation(value: unknown): VllmGroupElevation[] {
    if (!Array.isArray(value) || value.length < 1 || value.length > 3)
        throw new Error('Choose administrator access for every reviewed participant.')
    const entries = value.map((item): VllmGroupElevation => {
        const row = exactKeys(item, ['nodeId'], ['elevationPassword', 'nonInteractive'])
        const nodeId = identity(row.nodeId, 'administrator target identity')
        const password = row.elevationPassword
        const nonInteractive = row.nonInteractive
        if (
            (password !== undefined &&
                (typeof password !== 'string' ||
                    password.length === 0 ||
                    password.length > 4096 ||
                    password.includes('\0') ||
                    password.includes('\r') ||
                    password.includes('\n'))) ||
            (nonInteractive !== undefined && nonInteractive !== true) ||
            (password !== undefined) === (nonInteractive === true)
        )
            throw new Error('Invalid administrator access selection.')
        const result: VllmGroupElevation = { nodeId }
        if (typeof password === 'string') result.elevationPassword = password
        if (nonInteractive === true) result.nonInteractive = true
        return result
    })
    if (new Set(entries.map(entry => entry.nodeId)).size !== entries.length)
        throw new Error('Duplicate administrator access target.')
    return entries
}

export function parseVllmGroupReviewRequest(value: unknown): VllmGroupReviewRequest {
    const row = exactKeys(value, ['reviewId'])
    if (typeof row.reviewId !== 'string' || !reviewIdPattern.test(row.reviewId))
        throw new Error('An exact serving-group review is required.')
    return { reviewId: row.reviewId }
}

export function parseVllmGroupStartRequest(value: unknown): VllmGroupStartRequest {
    const row = exactKeys(value, ['reviewId'], ['elevation'])
    const review = parseVllmGroupReviewRequest({ reviewId: row.reviewId })
    return {
        ...review,
        ...(row.elevation === undefined
            ? {}
            : { elevation: parseVllmGroupElevation(row.elevation) })
    }
}

export function parseVllmGroupOperation(value: unknown): VllmGroupOperation {
    const row = exactKeys(value, ['runId', 'generation'])
    if (
        typeof row.runId !== 'string' ||
        !reviewIdPattern.test(row.runId) ||
        typeof row.generation !== 'number' ||
        !Number.isSafeInteger(row.generation) ||
        row.generation < 1
    )
        throw new Error('An exact retained serving-group operation is required.')
    return { runId: row.runId, generation: row.generation }
}

export function parseVllmGroupReconcileRequest(value: unknown): VllmGroupReconcileRequest {
    const row = exactKeys(value, ['runId', 'generation'], ['elevation'])
    const operation = parseVllmGroupOperation({
        runId: row.runId,
        generation: row.generation
    })
    return {
        ...operation,
        ...(row.elevation === undefined
            ? {}
            : { elevation: parseVllmGroupElevation(row.elevation) })
    }
}

/** A review is a proposal with an expiry; it is never execution authority. */
export function parseVllmGroupReview(value: unknown): VllmGroupReview {
    const row = object(value, 'serving-group review')
    if (typeof row.reason !== 'string')
        throw new Error('PAIR returned an invalid serving-group review reason.')
    return {
        reviewId: operationId(row.reviewId, 'serving-group review identity'),
        planDigest: digest(row.planDigest, 'serving-group plan digest'),
        plan: parseVllmGroupPlan(row.plan, { ownerFields: 'required' }),
        expiresAt: count(row.expiresAt, 'serving-group review expiry'),
        activationEnabled: flag(row.activationEnabled, 'serving-group activation state'),
        reason: row.reason
    }
}

/** Participant capability results; extra owner fields are ignored, identity fields are strict. */
export function parseVllmGroupCheck(value: unknown): VllmGroupCheck {
    const row = object(value, 'serving-group check')
    return {
        reviewId: operationId(row.reviewId, 'serving-group review identity'),
        activationEnabled: flag(row.activationEnabled, 'serving-group activation state'),
        participants: participants(row.participants, 'serving-group participant list').map(
            (value): VllmGroupCheckParticipant => {
                const participant = object(value, 'serving-group participant')
                if (typeof participant.reason !== 'string')
                    throw new Error('PAIR returned an invalid serving-group participant reason.')
                return {
                    nodeId: identity(participant.nodeId, 'serving-group participant identity'),
                    state: text(participant.state, 'serving-group participant state', 128),
                    activationEnabled: flag(
                        participant.activationEnabled,
                        'serving-group participant activation state'
                    ),
                    reason: participant.reason
                }
            }
        )
    }
}
