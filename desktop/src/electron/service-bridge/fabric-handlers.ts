// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Cable/fabric bridge. It forwards only fixed product inputs, injects approval
 * flags here (never credentials or commands), strictly parses every reply, and
 * binds replies to the exact server-issued review/run identity requested.
 */
import type { WsInvokeRequest } from '@/shared/types/ws-channels'
import type { FabricSelection } from '@/shared/types/fabric'
import {
    exactFabricKeys,
    parseCableCleanupReview,
    parseCableCleanupReviewRequest,
    parseCableCleanupVerifyResult,
    parseCableRetainedRuns,
    parseCableReview,
    parseCableRun,
    parseCableStartResponse,
    parseCableStatusRequest,
    parseFabricOperation,
    parseFabricInventoryRequest,
    parseFabricInventorySnapshot,
    parseFabricRetainedOperations,
    parseFabricReview,
    parseFabricSelection,
    parseOperationIdRequest,
    parseReviewIdRequest
} from '@/shared/utils/fabric'
import { getModularSupervisor } from './modular-supervisor'
import { JsonRpcResponseError, type JsonObject } from './json-rpc-subprocess'

type Method =
    | 'engine:fabric-inventory'
    | 'engine:cable-review'
    | 'engine:cable-start'
    | 'engine:cable-status'
    | 'engine:cable-cancel'
    | 'engine:cable-retained-runs'
    | 'engine:cable-cleanup-review'
    | 'engine:cable-cleanup-verify'
    | 'engine:cable-cleanup-cancel'
    | 'engine:fabric-review'
    | 'engine:fabric-approve'
    | 'engine:fabric-status'
    | 'engine:fabric-cancel'
    | 'engine:fabric-recover'
    | 'engine:fabric-retained-operations'

const TIMEOUTS: Record<Method, number> = {
    'engine:fabric-inventory': 20_000,
    'engine:cable-review': 35_000,
    'engine:cable-start': 75_000,
    'engine:cable-status': 30_000,
    'engine:cable-cancel': 30_000,
    'engine:cable-retained-runs': 30_000,
    'engine:cable-cleanup-review': 35_000,
    'engine:cable-cleanup-verify': 75_000,
    'engine:cable-cleanup-cancel': 30_000,
    'engine:fabric-review': 35_000,
    'engine:fabric-approve': 90_000,
    'engine:fabric-status': 35_000,
    'engine:fabric-cancel': 90_000,
    'engine:fabric-recover': 90_000,
    'engine:fabric-retained-operations': 30_000
}

async function call(method: Method, params: JsonObject) {
    const supervisor = getModularSupervisor()
    if (!supervisor.ready)
        throw new Error('PAIR service is unavailable for cable and fabric setup.')
    try {
        return await supervisor.callProcess('broker', method, params, TIMEOUTS[method])
    } catch (error) {
        if (error instanceof JsonRpcResponseError)
            throw new Error(error.message.replace(/^-?\d+: /, ''))
        throw error
    }
}

function selectionParams(selection: FabricSelection): JsonObject {
    return {
        nodeIds: selection.nodeIds,
        ports: selection.ports.map(port => ({ ...port })),
        ...(selection.acceptedHostKeys
            ? { acceptedHostKeys: selection.acceptedHostKeys.map(key => ({ ...key })) }
            : {})
    }
}

function bindReviewTargets(selection: FabricSelection, nodeIds: string[], label: string): void {
    if (nodeIds.length === 0) return // blocked reviews may stop before inventory resolves
    if (
        [...nodeIds].sort().join('\n') !== [...selection.nodeIds].sort().join('\n') ||
        new Set(nodeIds).size !== nodeIds.length
    )
        throw new Error(`PAIR returned a ${label} for a different node selection.`)
}

export const fabricHandlers = {
    'engine:fabric-inventory': async (payload?: WsInvokeRequest<'engine:fabric-inventory'>) => {
        const request = parseFabricInventoryRequest(payload)
        const result = parseFabricInventorySnapshot(
            await call('engine:fabric-inventory', { nodeIds: request.nodeIds })
        )
        if (result.nodes.map(node => node.nodeId).join('\n') !== request.nodeIds.join('\n'))
            throw new Error('PAIR returned fabric inventory for a different node selection.')
        return result
    },

    'engine:cable-review': async (payload?: WsInvokeRequest<'engine:cable-review'>) => {
        const selection = parseFabricSelection(payload)
        const review = parseCableReview(
            await call('engine:cable-review', selectionParams(selection))
        )
        bindReviewTargets(
            selection,
            review.targets.map(target => target.nodeId),
            'cable review'
        )
        return review
    },

    'engine:cable-start': async (payload?: WsInvokeRequest<'engine:cable-start'>) => {
        const request = parseReviewIdRequest(payload)
        const response = parseCableStartResponse(
            await call('engine:cable-start', { ...request, approveAdmin: true })
        )
        if (response.reviewId !== request.reviewId)
            throw new Error('PAIR returned a cable outcome for a different review.')
        return response
    },

    'engine:cable-status': async (payload?: WsInvokeRequest<'engine:cable-status'>) => {
        const request = parseCableStatusRequest(payload)
        const run = parseCableRun(await call('engine:cable-status', { ...request }))
        if (
            (request.runId && run.runId !== request.runId) ||
            (request.reviewId && run.reviewId !== request.reviewId)
        )
            throw new Error('PAIR returned a different cable operation.')
        return run
    },

    'engine:cable-cancel': async (payload?: WsInvokeRequest<'engine:cable-cancel'>) => {
        const requestRow = exactFabricKeys(payload, ['runId'], 'cable cancellation')
        const request = parseCableStatusRequest({ runId: requestRow.runId })
        const run = parseCableRun(await call('engine:cable-cancel', { runId: request.runId! }))
        if (run.runId !== request.runId)
            throw new Error('PAIR returned a different cable operation after cancellation.')
        return run
    },

    'engine:cable-retained-runs': async (
        payload?: WsInvokeRequest<'engine:cable-retained-runs'>
    ) => {
        if (payload !== undefined) exactFabricKeys(payload, [], 'retained cable request')
        return parseCableRetainedRuns(await call('engine:cable-retained-runs', {}))
    },

    'engine:cable-cleanup-review': async (
        payload?: WsInvokeRequest<'engine:cable-cleanup-review'>
    ) => {
        const request = parseCableCleanupReviewRequest(payload)
        const review = parseCableCleanupReview(
            await call('engine:cable-cleanup-review', {
                runId: request.runId,
                ...(request.acceptedHostKeys
                    ? { acceptedHostKeys: request.acceptedHostKeys.map(key => ({ ...key })) }
                    : {})
            })
        )
        if (review.runId !== request.runId)
            throw new Error('PAIR returned a cleanup review for a different cable operation.')
        return review
    },

    'engine:cable-cleanup-verify': async (
        payload?: WsInvokeRequest<'engine:cable-cleanup-verify'>
    ) => {
        const requestRow = exactFabricKeys(payload, ['runId', 'reviewId'], 'cable cleanup approval')
        const request = parseCableCleanupReviewRequest({ runId: requestRow.runId })
        const review = parseReviewIdRequest({ reviewId: requestRow.reviewId })
        const result = parseCableCleanupVerifyResult(
            await call('engine:cable-cleanup-verify', {
                runId: request.runId,
                reviewId: review.reviewId,
                approveAdmin: true
            })
        )
        if (result.reviewId !== review.reviewId || result.run.runId !== request.runId)
            throw new Error('PAIR returned a cleanup result for a different cable operation.')
        return result
    },

    'engine:cable-cleanup-cancel': async (
        payload?: WsInvokeRequest<'engine:cable-cleanup-cancel'>
    ) => {
        const requestRow = exactFabricKeys(payload, ['runId'], 'cable cleanup cancellation')
        const request = parseCableCleanupReviewRequest({ runId: requestRow.runId })
        const run = parseCableRun(
            await call('engine:cable-cleanup-cancel', { runId: request.runId })
        )
        if (run.runId !== request.runId)
            throw new Error('PAIR returned a different cable operation after cleanup cancellation.')
        return run
    },

    'engine:fabric-review': async (payload?: WsInvokeRequest<'engine:fabric-review'>) => {
        const requestRow = exactFabricKeys(
            payload,
            ['selection', 'inspectSelectedProfiles'],
            'fabric review request'
        )
        const selection = parseFabricSelection(requestRow.selection)
        if (
            requestRow.inspectSelectedProfiles !== undefined &&
            typeof requestRow.inspectSelectedProfiles !== 'boolean'
        )
            throw new Error('PAIR received an invalid profile-inspection choice.')
        const review = parseFabricReview(
            await call('engine:fabric-review', {
                ...selectionParams(selection),
                ...(requestRow.inspectSelectedProfiles === true
                    ? { inspectSelectedProfiles: true }
                    : {})
            })
        )
        bindReviewTargets(
            selection,
            review.targets.map(target => target.nodeId),
            'fabric review'
        )
        return review
    },

    'engine:fabric-approve': async (payload?: WsInvokeRequest<'engine:fabric-approve'>) => {
        const requestRow = exactFabricKeys(
            payload,
            ['reviewId', 'selectedPortPauseApproved'],
            'fabric approval'
        )
        const request = {
            reviewId: parseOperationIdRequest({ operationId: requestRow.reviewId }).operationId
        }
        if (typeof requestRow.selectedPortPauseApproved !== 'boolean')
            throw new Error('PAIR received an invalid selected-port interruption choice.')
        const operation = parseFabricOperation(
            await call('engine:fabric-approve', {
                ...request,
                administratorApproved: true,
                selectedPortPauseApproved: requestRow.selectedPortPauseApproved
            })
        )
        if (operation.reviewId !== request.reviewId || operation.operationId !== request.reviewId)
            throw new Error('PAIR returned a fabric operation for a different review.')
        return operation
    },

    'engine:fabric-status': (payload?: WsInvokeRequest<'engine:fabric-status'>) =>
        operationCall('engine:fabric-status', payload),
    'engine:fabric-cancel': (payload?: WsInvokeRequest<'engine:fabric-cancel'>) =>
        operationCall('engine:fabric-cancel', payload, true),
    'engine:fabric-recover': (payload?: WsInvokeRequest<'engine:fabric-recover'>) =>
        operationCall('engine:fabric-recover', payload, true),

    'engine:fabric-retained-operations': async (
        payload?: WsInvokeRequest<'engine:fabric-retained-operations'>
    ) => {
        if (payload !== undefined) exactFabricKeys(payload, [], 'retained fabric request')
        return parseFabricRetainedOperations(await call('engine:fabric-retained-operations', {}))
    }
}

async function operationCall(
    method: 'engine:fabric-status' | 'engine:fabric-cancel' | 'engine:fabric-recover',
    payload: unknown,
    administratorApproved = false
) {
    const request = parseOperationIdRequest(payload)
    const operation = parseFabricOperation(
        await call(method, {
            ...request,
            ...(administratorApproved ? { administratorApproved: true } : {})
        })
    )
    if (operation.operationId !== request.operationId)
        throw new Error('PAIR returned a different fabric operation.')
    return operation
}
