// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Typed main-process owner for vLLM model/runtime preparation. Renderer input
 * contains identities only; credentials, commands, URLs, and filesystem paths
 * cannot cross this boundary. PAIR's local/remote Engine Manager remains the
 * sole effect and receipt owner.
 */
import type { WsInvokeRequest } from '@/shared/types/ws-channels'
import type {
    VllmDistributionRequest,
    VllmExactModelRequest,
    VllmModelReceipt,
    VllmOperationCancelResult,
    VllmRuntimePrepareRequest
} from '@/shared/types/vllm-model-journey'
import {
    parseVllmCancelResult,
    parseVllmDistributionRequest,
    parseVllmExactModelRequest,
    parseVllmModelReceipt,
    parseVllmRuntimePrepareRequest,
    parseVllmRuntimePrepareResult
} from '@/shared/utils/vllm-model-journey'
import { object } from '@/shared/utils/vllm-group-status'
import { getModularSupervisor } from './modular-supervisor'
import { getModularBridgeState } from './modular-state'
import type { JsonObject, JsonValue } from './json-rpc-subprocess'

const PULL_TIMEOUT = 5 * 60 * 60_000 + 59 * 60_000
const DISTRIBUTION_TIMEOUT = PULL_TIMEOUT
// Engine Manager owns a six-hour bounded prepare (download, verify, offline
// install). Keep the typed renderer call bound until that terminal response.
const PREPARE_TIMEOUT = 6 * 60 * 60_000 + 60_000

function rendererSafeJourneyError(error: unknown): Error {
    const raw = error instanceof Error ? error.message : String(error)
    const withoutCode = raw.replace(/^-?\d+: /, '')
    const withoutSecrets = withoutCode
        .replace(
            /(["']?)(password|passphrase|token|secret|private[_-]?key|api[_-]?key|access[_-]?token|refresh[_-]?token|authorization)\1\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;}]+)/gi,
            (_match, quote: string, key: string) => `${quote}${key}${quote}=[redacted]`
        )
        .replace(/\bBearer\s+[A-Za-z0-9._~+/-]+=*/gi, 'Bearer [redacted]')
    const withoutHostPaths = withoutSecrets
        // Quote-aware passes run first so paths containing spaces are removed as
        // one token instead of exposing everything after the first space.
        .replace(
            /(["'`])(?:[A-Za-z]:[\\/]|\\\\|\/(?:home|opt|tmp|usr|var|Users)\/)[^"'`\r\n]*\1/g,
            '[host path]'
        )
        .replace(/(["'`])\/(?!\/)[^"'`\r\n]*\1/g, '[host path]')
        .replace(/\\\\[^\s"'`]+/g, '[host path]')
        .replace(/[A-Za-z]:[\\/][^\s"'`]+/g, '[host path]')
        .replace(/\/(?:home|opt|tmp|usr|var|Users)\/[^\s"'`]+/g, '[host path]')
        .replace(/(^|[\s([])\/(?!\/)[^\s"'`\])]+/g, '$1[host path]')
    return new Error(withoutHostPaths.slice(0, 2048))
}

async function call(
    method: string,
    params: JsonObject,
    timeout: number
): Promise<JsonValue | undefined> {
    const supervisor = getModularSupervisor()
    if (!supervisor.ready) throw new Error('PAIR service is unavailable for vLLM preparation.')
    try {
        return await supervisor.callProcess('broker', method, params, timeout)
    } catch (error) {
        throw rendererSafeJourneyError(error)
    }
}

function requireManagedNode(nodeId: string, verb: string): void {
    const state = getModularBridgeState()
    if (!state.isEngineCommandAllowed(nodeId, 'vllm', 'toggle'))
        throw new Error(`${verb} is held until this node reports a stopped PAIR-managed vLLM.`)
    const status = state
        .getEngineInitialState()
        .statuses.find(item => item.nodeId === nodeId && item.engineType === 'vllm')
    if (!status || status.managed !== true || status.processStatus !== 'stopped')
        throw new Error(`Stop the PAIR-managed vLLM on ${nodeId} before ${verb.toLowerCase()}.`)
}

async function pullExact(request: VllmExactModelRequest) {
    requireManagedNode(request.nodeId, 'Model acquisition')
    const state = getModularBridgeState()
    const supervisor = getModularSupervisor()
    const local = request.nodeId === state.getSelfId()
    if (local) state.beginModelPull('vllm', request.model)
    else state.beginRemoteModelPull(request.nodeId, 'vllm', request.model)
    try {
        const raw = local
            ? await call(
                  'engine:action',
                  {
                      engine: 'vllm',
                      action: 'pull_model',
                      params: { model: request.model, operationId: request.operationId }
                  },
                  PULL_TIMEOUT
              )
            : await call(
                  'engine:remote-pull-model',
                  {
                      node: request.nodeId,
                      engine: 'vllm',
                      model: request.model,
                      operationId: request.operationId,
                      params: { model: request.model, operationId: request.operationId }
                  },
                  PULL_TIMEOUT
              )
        const result = local ? raw : object(raw, 'remote model acquisition').result
        const receipt = parseVllmModelReceipt(result)
        if (receipt.id !== request.model)
            throw new Error('PAIR returned a receipt for a different model.')
        if (local) await supervisor.refreshEngineModels('vllm', 'vllm')
        else await supervisor.refreshRemoteEngineStatus(request.nodeId)
        return receipt
    } finally {
        if (local) state.finishModelPull('vllm', request.model)
        else state.finishRemoteModelPull(request.nodeId, 'vllm', request.model)
    }
}

async function cancelPull(request: VllmExactModelRequest): Promise<VllmOperationCancelResult> {
    const state = getModularBridgeState()
    const local = request.nodeId === state.getSelfId()
    const raw = local
        ? await call(
              'engine:action',
              {
                  engine: 'vllm',
                  action: 'cancel_pull',
                  params: { model: request.model, operationId: request.operationId }
              },
              30_000
          )
        : await call(
              'engine:remote-cancel-pull',
              {
                  node: request.nodeId,
                  engine: 'vllm',
                  model: request.model,
                  operationId: request.operationId
              },
              30_000
          )
    const row = object(raw, 'model acquisition cancellation')
    return parseVllmCancelResult({ ...row, operationId: request.operationId }, 'model acquisition')
}

async function prepareRuntime(
    request: VllmRuntimePrepareRequest,
    cancel: false
): Promise<ReturnType<typeof parseVllmRuntimePrepareResult>>
async function prepareRuntime(
    request: VllmRuntimePrepareRequest,
    cancel: true
): Promise<VllmOperationCancelResult>
async function prepareRuntime(request: VllmRuntimePrepareRequest, cancel: boolean) {
    if (!cancel) requireManagedNode(request.nodeId, 'Runtime preparation')
    const state = getModularBridgeState()
    const local = request.nodeId === state.getSelfId()
    if (!cancel) state.beginVllmJourney(request.nodeId, 'prepare', request.operationId)
    try {
        const raw = await call(
            local ? 'engine:vllm-qwen38-prepare' : 'engine:remote-vllm-qwen38-prepare',
            local
                ? { operationId: request.operationId, ...(cancel ? { cancel: true } : {}) }
                : {
                      node: request.nodeId,
                      engine: 'vllm',
                      operationId: request.operationId,
                      ...(cancel ? { cancel: true } : {})
                  },
            cancel ? 30_000 : PREPARE_TIMEOUT
        )
        if (cancel) {
            const row = object(raw, 'runtime preparation cancellation')
            return {
                accepted: row.state === 'cancel-requested',
                operationId: request.operationId
            }
        }
        const result = parseVllmRuntimePrepareResult(raw, request.nodeId, request.operationId)
        if (!local) await getModularSupervisor().refreshRemoteEngineStatus(request.nodeId)
        return result
    } finally {
        state.finishVllmJourney(request.nodeId, 'prepare')
    }
}

async function distribute(
    request: VllmDistributionRequest,
    cancel: false
): Promise<VllmModelReceipt>
async function distribute(
    request: VllmDistributionRequest,
    cancel: true
): Promise<VllmOperationCancelResult>
async function distribute(
    request: VllmDistributionRequest,
    cancel: boolean
): Promise<VllmModelReceipt | VllmOperationCancelResult> {
    if (!cancel) {
        requireManagedNode(request.sourceNodeId, 'Model distribution')
        requireManagedNode(request.nodeId, 'Model distribution')
    }
    const state = getModularBridgeState()
    const localTarget = request.nodeId === state.getSelfId()
    if (!cancel)
        state.beginVllmJourney(request.nodeId, 'distribute', request.operationId, request.model)
    try {
        const params = {
            engine: 'vllm',
            operationId: request.operationId,
            sourceNode: request.sourceNodeId,
            model: request.model,
            ...(cancel ? { cancel: true } : {})
        }
        const raw = await call(
            localTarget ? 'engine:vllm-distribute-model' : 'engine:remote-distribute-model',
            localTarget ? params : { node: request.nodeId, ...params },
            cancel ? 2 * 60_000 : DISTRIBUTION_TIMEOUT
        )
        const terminal = localTarget ? raw : object(raw, 'remote model distribution').result
        if (cancel) {
            const row = object(terminal, 'model distribution cancellation')
            return parseVllmCancelResult(
                { ...row, operationId: request.operationId },
                'model distribution'
            )
        }
        const receipt = parseVllmModelReceipt(terminal)
        if (receipt.id !== request.model)
            throw new Error('PAIR distributed a different model than requested.')
        return receipt
    } finally {
        state.finishVllmJourney(request.nodeId, 'distribute')
    }
}

export const vllmModelJourneyHandlers = {
    'engine:vllm-pull-exact': async (payload?: WsInvokeRequest<'engine:vllm-pull-exact'>) =>
        pullExact(parseVllmExactModelRequest(payload)),
    'engine:vllm-cancel-pull': async (payload?: WsInvokeRequest<'engine:vllm-cancel-pull'>) =>
        cancelPull(parseVllmExactModelRequest(payload)),
    'engine:vllm-prepare-runtime': async (
        payload?: WsInvokeRequest<'engine:vllm-prepare-runtime'>
    ) => prepareRuntime(parseVllmRuntimePrepareRequest(payload), false),
    'engine:vllm-cancel-prepare': async (payload?: WsInvokeRequest<'engine:vllm-cancel-prepare'>) =>
        prepareRuntime(parseVllmRuntimePrepareRequest(payload), true),
    'engine:vllm-distribute-model': async (
        payload?: WsInvokeRequest<'engine:vllm-distribute-model'>
    ) => distribute(parseVllmDistributionRequest(payload), false),
    'engine:vllm-cancel-distribution': async (
        payload?: WsInvokeRequest<'engine:vllm-cancel-distribution'>
    ) => distribute(parseVllmDistributionRequest(payload), true)
}
