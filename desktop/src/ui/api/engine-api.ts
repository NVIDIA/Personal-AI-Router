// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine command API -- fire-and-forget commands + push event subscriptions.
 *
 * Commands are void (fire-and-forget). Errors arrive via errors:update push events.
 * All state flows via push events, not command responses.
 *
 * The managed serving-group methods are the exception: they are typed
 * request/response calls whose results are strictly parsed by the bridge. They
 * are state-free here; the serving-group store is the single writer of hold
 * truth and drives them through the transport bound below.
 */
import type { PreloadServiceTransport as ServiceTransport } from '@/shared/types/service-bridge'
import type {
    EngineHubSearchResponse,
    EngineInitialState,
    EngineLogSnapshot,
    EngineStatePatch,
    EngineCommandPayload
} from '@/shared/types/engine-api'
import type { EngineProgress, EngineType } from '@/shared/types/engines'
import type { VllmGroupRunStatus, VllmGroupStatus } from '@/shared/types/vllm-group-status'
import type {
    VllmGroupCheck,
    VllmGroupElevation,
    VllmGroupOperation,
    VllmGroupReconcileRequest,
    VllmGroupReview,
    VllmGroupSelection
} from '@/shared/types/vllm-group'
import type { VllmGroupCleanupRequest } from '@/shared/types/vllm-group-cleanup'
import { parseVllmGroupCleanupRequest } from '@/shared/utils/vllm-group-cleanup'
import type { VllmModelSelectionResult } from '@/shared/types/vllm-model-selection'
import { parseVllmModelSelectionRequest } from '@/shared/utils/vllm-model-selection'
import { bindVllmModelSelectionApi } from '@/ui/stores/vllm-model-selection.store'
import type {
    VllmDistributionRequest,
    VllmExactModelRequest,
    VllmModelReceipt,
    VllmOperationCancelResult,
    VllmRuntimePrepareRequest,
    VllmRuntimePrepareResult
} from '@/shared/types/vllm-model-journey'
import {
    parseVllmGroupOperation,
    parseVllmGroupReconcileRequest,
    parseVllmGroupReviewRequest,
    parseVllmGroupSelection,
    parseVllmGroupStartRequest
} from '@/shared/utils/vllm-group'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import {
    bindVllmGroupApi,
    useVllmGroupStore,
    vllmMutationBlockReason,
    vllmMutationsBlocked
} from '@/ui/stores/vllm-group.store'
import type {
    EngineSettingsTarget,
    EngineSettingsRequest,
    EngineSettingsSnapshot,
    EngineSettingsPreview,
    EngineSettingsReceipt
} from '@/shared/types/engine-settings'

export interface IEngineApi {
    /** Read the owning node's authoritative settings snapshot for one engine. */
    getSettings(target: EngineSettingsTarget): Promise<EngineSettingsSnapshot>
    /**
     * Validate a draft against the owner without persisting or touching any
     * process: returns the normalized settings, per-field errors, any port
     * conflict, and whether applying would restart or rebind.
     */
    previewSettings(request: EngineSettingsRequest): Promise<EngineSettingsPreview>
    /**
     * Commit a draft. The receipt only acknowledges that the owner accepted the
     * operation — completion arrives on `onSettingsChanged`, because a stop,
     * rebind, and restart outlive the call.
     */
    applySettings(request: EngineSettingsRequest): Promise<EngineSettingsReceipt>
    /** A settings snapshot changed on some node — the only source of applied state. */
    onSettingsChanged(callback: (snapshot: EngineSettingsSnapshot) => void): () => void
    /** A node's settings authority became unreachable; its cached snapshot is stale. */
    onSettingsDisconnected(callback: (target: { nodeId: string }) => void): () => void
    /** Fetch initial engine state: statuses, models, progress, and update availability. */
    getInitialState(): Promise<EngineInitialState>
    /** Read the bounded local Engine Manager process-output ring for diagnostics. */
    getLogs(engineType: EngineType): Promise<EngineLogSnapshot>

    /** Start or stop an engine process on a node. */
    toggle(engineType: EngineType, nodeId: string): void
    /** Install an engine on a node. */
    install(engineType: EngineType, nodeId: string): void
    /** Uninstall an engine from a node without removing downloaded models. */
    uninstall(engineType: EngineType, nodeId: string): void
    /** Pull (download) a model on a node. */
    pullModel(engineType: EngineType, nodeId: string, model: string): void
    cancelPull(engineType: EngineType, nodeId: string, model: string): void
    importModel(engineType: EngineType, nodeId: string, path: string): void
    /** Load a model into memory on a node. */
    loadModel(engineType: EngineType, nodeId: string, model: string): void
    /** Unload a model from memory on a node. */
    unloadModel(engineType: EngineType, nodeId: string, model: string): void
    /** Delete a downloaded model from a node. */
    deleteModel(engineType: EngineType, nodeId: string, model: string): void
    /** Set the model keep-alive expiry duration on a node. */
    setModelExpiry(engineType: EngineType, nodeId: string, model: string, expiry: string): void
    /** Search the model registry/hub for available models. */
    searchHub(engineType: EngineType): Promise<EngineHubSearchResponse>

    /** Read-only retained vLLM serving-group ownership and cleanup state. State-free. */
    getServingGroupStatus(): Promise<VllmGroupStatus>
    /** Ask PAIR to review a group of current nodes for one exact model. A review is never execution. */
    reviewServingGroup(selection: VllmGroupSelection): Promise<VllmGroupReview>
    /** Ask PAIR whether every reviewed participant is admitted. Readiness is never execution. */
    checkServingGroup(reviewId: string): Promise<VllmGroupCheck>
    /** Consume one unexpired review. PAIR may refuse; the outcome is bound only by a status read. */
    startServingGroup(
        reviewId: string,
        elevation?: VllmGroupElevation[]
    ): Promise<VllmGroupRunStatus>
    /** Stop the exact retained operation; the reply is the refreshed status. */
    stopServingGroup(operation: VllmGroupOperation): Promise<VllmGroupStatus>
    /** Reconcile the exact retained operation; the reply is the refreshed status. */
    reconcileServingGroup(operation: VllmGroupReconcileRequest): Promise<VllmGroupStatus>
    /**
     * Ask PAIR to reconcile and close the exact retained operation. The reply is
     * PAIR refreshed status; cleanup is confirmed only when that status shows
     * every attempted rank cleaned. Unresolved ranks reject and stay held.
     */
    requestServingGroupCleanup(request: VllmGroupCleanupRequest): Promise<VllmGroupStatus>
    /**
     * Select one already-downloaded retained model, by canonical id, for the next
     * owned start of this controller's stopped PAIR-managed vLLM. PAIR verifies
     * the library and replies with the refreshed engine status.
     */
    selectVllmModel(nodeId: string, model: string): Promise<VllmModelSelectionResult>
    pullVllmExact(request: VllmExactModelRequest): Promise<VllmModelReceipt>
    cancelVllmPull(request: VllmExactModelRequest): Promise<VllmOperationCancelResult>
    prepareVllmRuntime(request: VllmRuntimePrepareRequest): Promise<VllmRuntimePrepareResult>
    cancelVllmPrepare(request: VllmRuntimePrepareRequest): Promise<VllmOperationCancelResult>
    distributeVllmModel(request: VllmDistributionRequest): Promise<VllmModelReceipt>
    cancelVllmDistribution(request: VllmDistributionRequest): Promise<VllmOperationCancelResult>

    /** Durable engine state changed. Prefer this for new renderer state. */
    onStateChanged(callback: (patch: EngineStatePatch) => void): () => void
    /** Engine operation progress update (pull, install, etc.). */
    onProgress(callback: (progress: EngineProgress) => void): () => void
    /** An engine progress entry was removed (operation completed). */
    onProgressRemove(callback: (key: string) => void): () => void
    /** Trigger a one-click update (managed installs only). Fire-and-forget. */
    update(engineType: EngineType, nodeId: string): void
}

function fireCommand(transport: ServiceTransport, payload: EngineCommandPayload): void {
    const selfId = useConnectionStore.getState().selfId
    const localVllmSupported =
        typeof window !== 'undefined' && window.windowApi?.platform === 'Linux'
    if (
        payload.engineType === 'vllm' &&
        vllmMutationsBlocked(payload.nodeId, selfId, localVllmSupported)
    ) {
        useErrorsStore.getState().addLocalError(`vLLM action held: ${vllmMutationBlockReason()}`)
        return
    }
    // Optimistically flip the clicked control to a "working"/disabled state so
    // the user gets immediate feedback even when the backend push lags (esp.
    // remote nodes). The entry clears itself on the superseding push / timeout.
    usePendingActionsStore.getState().begin(payload)
    transport.invoke('engine:command', payload).catch(() => {})
}

export function createEngineApi(transport: ServiceTransport): IEngineApi {
    // Pure transport: every method validates its typed request and returns the
    // bridge-parsed reply. The serving-group store owns all state changes.
    const group = {
        getServingGroupStatus: () => transport.invoke('engine:vllm-group-status'),
        reviewServingGroup: (selection: VllmGroupSelection) =>
            transport.invoke('engine:vllm-group-review', {
                selection: parseVllmGroupSelection(selection)
            }),
        checkServingGroup: (reviewId: string) =>
            transport.invoke('engine:vllm-group-check', parseVllmGroupReviewRequest({ reviewId })),
        startServingGroup: (reviewId: string, elevation?: VllmGroupElevation[]) =>
            transport.invoke(
                'engine:vllm-group-start',
                parseVllmGroupStartRequest({ reviewId, ...(elevation ? { elevation } : {}) })
            ),
        stopServingGroup: (operation: VllmGroupOperation) =>
            transport.invoke('engine:vllm-group-stop', parseVllmGroupOperation(operation)),
        reconcileServingGroup: (operation: VllmGroupReconcileRequest) =>
            transport.invoke(
                'engine:vllm-group-reconcile',
                parseVllmGroupReconcileRequest(operation)
            ),
        requestServingGroupCleanup: (request: VllmGroupCleanupRequest) =>
            transport.invoke('engine:vllm-group-cleanup', parseVllmGroupCleanupRequest(request))
    }
    bindVllmGroupApi(group)
    const selectVllmModel = (nodeId: string, model: string) =>
        transport.invoke(
            'engine:vllm-select-model',
            parseVllmModelSelectionRequest({ nodeId, model })
        )
    bindVllmModelSelectionApi({ selectVllmModel })
    return {
        getSettings: target => transport.invoke('engines:get-settings', target),
        previewSettings: request => transport.invoke('engines:preview-settings', request),
        applySettings: request => transport.invoke('engines:apply-settings', request),
        onSettingsChanged: cb => transport.subscribePush('engines:settings-changed', cb),
        onSettingsDisconnected: cb => transport.subscribePush('engines:settings-disconnected', cb),
        getInitialState: async () => {
            // Unknown group ownership intentionally keeps vLLM mutations held
            // while unrelated engine inventory remains available; the store
            // records the read outcome either way.
            await useVllmGroupStore.getState().refresh()
            return transport.invoke('engines:get-initial')
        },
        getLogs: engineType => transport.invoke('engine:logs', { engineType }),

        toggle: (engineType, nodeId) =>
            fireCommand(transport, { command: 'toggle', engineType, nodeId }),
        install: (engineType, nodeId) =>
            fireCommand(transport, { command: 'install', engineType, nodeId }),
        uninstall: (engineType, nodeId) =>
            fireCommand(transport, { command: 'uninstall', engineType, nodeId }),
        pullModel: (engineType, nodeId, model) =>
            fireCommand(transport, { command: 'pullModel', engineType, nodeId, model }),
        cancelPull: (engineType, nodeId, model) =>
            fireCommand(transport, { command: 'cancelPull', engineType, nodeId, model }),
        importModel: (engineType, nodeId, path) =>
            fireCommand(transport, { command: 'importModel', engineType, nodeId, model: path }),
        loadModel: (engineType, nodeId, model) =>
            fireCommand(transport, { command: 'loadModel', engineType, nodeId, model }),
        unloadModel: (engineType, nodeId, model) =>
            fireCommand(transport, { command: 'unloadModel', engineType, nodeId, model }),
        deleteModel: (engineType, nodeId, model) =>
            fireCommand(transport, { command: 'deleteModel', engineType, nodeId, model }),
        setModelExpiry: (engineType, nodeId, model, expiry) =>
            fireCommand(transport, {
                command: 'setModelExpiry',
                engineType,
                nodeId,
                model,
                expiry
            }),
        searchHub: engineType => transport.invoke('engine:search-hub', { engineType }),
        ...group,
        selectVllmModel,
        pullVllmExact: request => transport.invoke('engine:vllm-pull-exact', request),
        cancelVllmPull: request => transport.invoke('engine:vllm-cancel-pull', request),
        prepareVllmRuntime: request => transport.invoke('engine:vllm-prepare-runtime', request),
        cancelVllmPrepare: request => transport.invoke('engine:vllm-cancel-prepare', request),
        distributeVllmModel: request => transport.invoke('engine:vllm-distribute-model', request),
        cancelVllmDistribution: request =>
            transport.invoke('engine:vllm-cancel-distribution', request),

        onStateChanged: cb => transport.subscribePush('engines:state-changed', cb),
        onProgress: cb => transport.subscribePush('engines:progress-changed', cb),
        onProgressRemove: cb =>
            transport.subscribePush('engines:progress-cleared', ({ key }) => cb(key)),
        update: (engineType, nodeId) =>
            fireCommand(transport, { command: 'update', engineType, nodeId })
    }
}
