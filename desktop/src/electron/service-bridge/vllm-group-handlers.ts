// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Managed vLLM serving-group bridge. Every handler forwards a typed product
 * request built from current node identities and exact PAIR-issued
 * identifiers. One-use per-node administrator input may cross only Start or
 * Reconcile and is cleared after dispatch; commands, paths and caller-asserted
 * authority remain forbidden. Responses are parsed strictly, and every
 * status-bearing response refreshes the bridge's hold state so ordinary vLLM
 * mutations stay fenced while a group is reserved or unknown.
 */
import type { WsInvokeRequest } from '@/shared/types/ws-channels'
import type { VllmGroupStatus } from '@/shared/types/vllm-group-status'
import type {
    VllmGroupElevation,
    VllmGroupOperation,
    VllmGroupReconcileRequest,
    VllmGroupStartRequest
} from '@/shared/types/vllm-group'
import { parseVllmGroupRun, parseVllmGroupStatus } from '@/shared/utils/vllm-group-status'
import {
    exactKeys,
    parseVllmGroupCheck,
    parseVllmGroupOperation,
    parseVllmGroupReconcileRequest,
    parseVllmGroupReview,
    parseVllmGroupReviewRequest,
    parseVllmGroupSelection,
    parseVllmGroupStartRequest
} from '@/shared/utils/vllm-group'
import {
    bindVllmGroupCleanup,
    bindVllmGroupCleanupStatus,
    parseVllmGroupCleanupRequest
} from '@/shared/utils/vllm-group-cleanup'
import { VLLM_QWEN38_MODEL } from '@/shared/constants/vllm'
import { parseVllmModelSelectionRequest } from '@/shared/utils/vllm-model-selection'
import type { VllmModelSelectionResult } from '@/shared/types/vllm-model-selection'
import { object, text } from '@/shared/utils/vllm-group-status'
import { getModularSupervisor } from './modular-supervisor'
import { getModularBridgeState } from './modular-state'
import { JsonRpcResponseError, type JsonObject, type JsonValue } from './json-rpc-subprocess'

type Method =
    | 'engine:vllm-group-review'
    | 'engine:vllm-group-check'
    | 'engine:vllm-group-start'
    | 'engine:vllm-group-status'
    | 'engine:vllm-group-stop'
    | 'engine:vllm-group-reconcile'
    | 'engine:vllm-group-cleanup'
    | 'engine:vllm-select-model'

/** Review may probe three participants' content; cleanup covers three serial 30 s participant caps plus owner overhead. */
const VLLM_GROUP_TIMEOUTS: Record<Method, number> = {
    'engine:vllm-group-review': 125_000,
    'engine:vllm-group-check': 30_000,
    'engine:vllm-group-start': 30_000,
    'engine:vllm-group-status': 30_000,
    'engine:vllm-group-stop': 110_000,
    'engine:vllm-group-reconcile': 110_000,
    'engine:vllm-group-cleanup': 110_000,
    // Model selection re-verifies retained model content on disk before binding.
    'engine:vllm-select-model': 10 * 60_000
}
const VLLM_GROUP_QWEN_REVIEW_TIMEOUT = 36 * 60_000

async function call(method: Method, params: JsonObject, timeout = VLLM_GROUP_TIMEOUTS[method]) {
    const supervisor = getModularSupervisor()
    if (!supervisor.ready) throw new Error('PAIR service is unavailable for serving groups.')
    try {
        return await supervisor.callProcess('broker', method, params, timeout)
    } catch (error) {
        // PAIR refusals are shown verbatim: drop only the transport's numeric
        // JSON-RPC code prefix, never any of the backend's own words.
        if (error instanceof JsonRpcResponseError)
            throw new Error(error.message.replace(/^-?\d+: /, ''))
        throw error
    }
}

/** Fresh owner read. Failure holds every vLLM mutation until the next successful read. */
async function readStatus(): Promise<VllmGroupStatus> {
    const state = getModularBridgeState()
    try {
        const status = parseVllmGroupStatus(await call('engine:vllm-group-status', {}))
        state.setVllmGroupStatus(status)
        return status
    } catch (error) {
        state.holdVllmGroupStatus()
        throw error
    }
}

/** A status-bearing operation reply must name the exact operation it acted on. */
function bindStatus(
    status: VllmGroupStatus,
    operation: VllmGroupOperation,
    verb: string
): VllmGroupStatus {
    if (
        !status.run ||
        status.run.runId !== operation.runId ||
        status.run.generation !== operation.generation
    )
        throw new Error(`${verb} returned a different operation; cleanup is unconfirmed.`)
    return status
}

async function operationCall(
    method: 'engine:vllm-group-stop' | 'engine:vllm-group-reconcile',
    payload:
        | WsInvokeRequest<'engine:vllm-group-stop'>
        | WsInvokeRequest<'engine:vllm-group-reconcile'>
        | undefined,
    verb: string
): Promise<VllmGroupStatus> {
    const reconcile =
        method === 'engine:vllm-group-reconcile' ? parseVllmGroupReconcileRequest(payload) : null
    const request = reconcile ?? parseVllmGroupOperation(payload)
    const operation: VllmGroupOperation = {
        runId: request.runId,
        generation: request.generation
    }
    const params: JsonObject = { ...operation }
    if (reconcile?.elevation) params.elevation = elevationParams(reconcile.elevation)
    const state = getModularBridgeState()
    try {
        const status = bindStatus(parseVllmGroupStatus(await call(method, params)), operation, verb)
        state.setVllmGroupStatus(status)
        return status
    } catch (error) {
        state.holdVllmGroupStatus()
        throw error
    } finally {
        if (reconcile) clearElevation(reconcile)
        clearElevationParams(params)
    }
}

function elevationParams(elevation: VllmGroupElevation[]): JsonObject[] {
    return elevation.map(entry => {
        const value: JsonObject = { nodeId: entry.nodeId }
        if (entry.elevationPassword !== undefined) value.elevationPassword = entry.elevationPassword
        if (entry.nonInteractive === true) value.nonInteractive = true
        return value
    })
}

function clearElevation(request: VllmGroupStartRequest | VllmGroupReconcileRequest): void {
    for (const elevation of request.elevation ?? []) {
        if (elevation.elevationPassword !== undefined) elevation.elevationPassword = ''
    }
}

function clearElevationParams(params: JsonObject): void {
    const elevation = params.elevation
    if (!Array.isArray(elevation)) return
    for (const value of elevation) {
        if (value === null || typeof value !== 'object' || Array.isArray(value)) continue
        if (value.elevationPassword !== undefined) value.elevationPassword = ''
    }
}

export const vllmGroupHandlers = {
    'engine:vllm-group-review': async (payload?: WsInvokeRequest<'engine:vllm-group-review'>) => {
        const selection = parseVllmGroupSelection(exactKeys(payload, ['selection']).selection)
        const review = parseVllmGroupReview(
            await call(
                'engine:vllm-group-review',
                { selection: { ...selection } },
                selection.model === VLLM_QWEN38_MODEL ? VLLM_GROUP_QWEN_REVIEW_TIMEOUT : undefined
            )
        )
        if (
            review.plan.model !== selection.model ||
            review.plan.members.map(member => member.nodeId).join('\n') !==
                selection.nodeIds.join('\n')
        )
            throw new Error('PAIR returned a review for a different selection.')
        return review
    },

    'engine:vllm-group-check': async (payload?: WsInvokeRequest<'engine:vllm-group-check'>) => {
        const request = parseVllmGroupReviewRequest(payload)
        const check = parseVllmGroupCheck(await call('engine:vllm-group-check', { ...request }))
        if (check.reviewId !== request.reviewId)
            throw new Error('PAIR returned a check for a different review.')
        return check
    },

    'engine:vllm-group-start': async (payload?: WsInvokeRequest<'engine:vllm-group-start'>) => {
        const request = parseVllmGroupStartRequest(payload)
        const state = getModularBridgeState()
        // Any Start outcome, including a lost reply, leaves ownership unknown
        // until a fresh status read; hold first, then read back.
        state.holdVllmGroupStatus()
        let run
        const params: JsonObject = { reviewId: request.reviewId }
        if (request.elevation) params.elevation = elevationParams(request.elevation)
        try {
            run = parseVllmGroupRun(await call('engine:vllm-group-start', params), {
                ownerFields: 'required'
            })
        } finally {
            clearElevation(request)
            clearElevationParams(params)
            await readStatus().catch(() => undefined)
        }
        return run
    },

    'engine:vllm-group-status': async (payload?: WsInvokeRequest<'engine:vllm-group-status'>) => {
        if (payload !== undefined) exactKeys(payload, [])
        return readStatus()
    },

    'engine:vllm-group-stop': (payload?: WsInvokeRequest<'engine:vllm-group-stop'>) =>
        operationCall('engine:vllm-group-stop', payload, 'Stop'),

    'engine:vllm-group-reconcile': (payload?: WsInvokeRequest<'engine:vllm-group-reconcile'>) =>
        operationCall('engine:vllm-group-reconcile', payload, 'Reconcile'),

    'engine:vllm-group-cleanup': async (payload?: WsInvokeRequest<'engine:vllm-group-cleanup'>) => {
        const requested = parseVllmGroupCleanupRequest(payload)
        // Fresh read first; a stale or cached identity is refused here and never
        // reaches PAIR. The fresh binding, not the displayed one, is sent.
        const binding = bindVllmGroupCleanup(await readStatus(), requested)
        const state = getModularBridgeState()
        try {
            // PAIR runs its own noninteractive fixed-helper reconcile and closure
            // and answers with the refreshed status of the exact operation.
            // Unresolved ranks arrive as an error and leave the group held.
            const status = bindVllmGroupCleanupStatus(
                parseVllmGroupStatus(await call('engine:vllm-group-cleanup', { ...binding })),
                binding
            )
            state.setVllmGroupStatus(status)
            return status
        } catch (error) {
            state.holdVllmGroupStatus()
            throw error
        }
    },

    'engine:vllm-select-model': async (
        payload?: WsInvokeRequest<'engine:vllm-select-model'>
    ): Promise<VllmModelSelectionResult> => {
        const request = parseVllmModelSelectionRequest(payload)
        const state = getModularBridgeState()
        // Selection binds this controller's owned vLLM library; remote engines
        // have no selection route, and a held or unknown serving group, an
        // adopted process, or a running engine refuse exactly as Start would.
        if (request.nodeId !== state.getSelfId())
            throw new Error(
                "Model selection is available only for this controller's PAIR-managed vLLM."
            )
        if (!state.isEngineCommandAllowed(request.nodeId, 'vllm', 'toggle'))
            throw new Error('vLLM is held; model selection is refused until ownership is clear.')
        const initial = state.getEngineInitialState()
        const status = initial.statuses.find(
            item => item.nodeId === request.nodeId && item.engineType === 'vllm'
        )
        if (!status || status.managed !== true || status.processStatus !== 'stopped')
            throw new Error('Stop the PAIR-managed vLLM before selecting a model.')
        const library = initial.models.find(
            item => item.nodeId === request.nodeId && item.engineType === 'vllm'
        )
        if (!library?.models.some(item => item.downloaded && item.name === request.model))
            throw new Error(
                'The model is not in the retained vLLM library PAIR reports for this node.'
            )
        const reply = object(
            await call('engine:vllm-select-model', { model: request.model }),
            'model selection result'
        )
        const replyStatus = object(reply.status, 'model selection status')
        if (
            reply.engine !== 'vllm' ||
            reply.model !== request.model ||
            reply.selected !== true ||
            text(replyStatus.selected_model, 'selected model') !== request.model
        )
            throw new Error('PAIR returned a selection for a different model.')
        // PAIR's reply carries the authoritative refreshed engine status; publish
        // it through the same parser the state-changed push uses.
        state.applyEngineManagerStatus(reply.status as JsonValue)
        return { nodeId: request.nodeId, model: request.model, selectedModel: request.model }
    }
}
