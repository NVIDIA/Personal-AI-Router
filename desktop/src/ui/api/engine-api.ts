// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine command API -- fire-and-forget commands + push event subscriptions.
 *
 * Commands are void (fire-and-forget). Errors arrive via errors:update push events.
 * All state flows via push events, not command responses.
 */
import type { PreloadServiceTransport as ServiceTransport } from '@/shared/types/service-bridge'
import type {
    EngineHubSearchResponse,
    EngineInitialState,
    EngineStatePatch,
    EngineCommandPayload
} from '@/shared/types/engine-api'
import type { EngineProgress, EngineType } from '@/shared/types/engines'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
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

    /** Start or stop an engine process on a node. */
    toggle(engineType: EngineType, nodeId: string): void
    /** Install an engine on a node. */
    install(engineType: EngineType, nodeId: string): void
    /** Uninstall an engine from a node without removing downloaded models. */
    uninstall(engineType: EngineType, nodeId: string): void
    /** Pull (download) a model on a node. */
    pullModel(engineType: EngineType, nodeId: string, model: string): void
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
    // Optimistically flip the clicked control to a "working"/disabled state so
    // the user gets immediate feedback even when the backend push lags (esp.
    // remote nodes). The entry clears itself on the superseding push / timeout.
    usePendingActionsStore.getState().begin(payload)
    transport.invoke('engine:command', payload).catch(() => {})
}

export function createEngineApi(transport: ServiceTransport): IEngineApi {
    return {
        getSettings: target => transport.invoke('engines:get-settings', target),
        previewSettings: request => transport.invoke('engines:preview-settings', request),
        applySettings: request => transport.invoke('engines:apply-settings', request),
        onSettingsChanged: cb => transport.subscribePush('engines:settings-changed', cb),
        onSettingsDisconnected: cb => transport.subscribePush('engines:settings-disconnected', cb),
        getInitialState: () => transport.invoke('engines:get-initial'),

        toggle: (engineType, nodeId) =>
            fireCommand(transport, { command: 'toggle', engineType, nodeId }),
        install: (engineType, nodeId) =>
            fireCommand(transport, { command: 'install', engineType, nodeId }),
        uninstall: (engineType, nodeId) =>
            fireCommand(transport, { command: 'uninstall', engineType, nodeId }),
        pullModel: (engineType, nodeId, model) =>
            fireCommand(transport, { command: 'pullModel', engineType, nodeId, model }),
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

        onStateChanged: cb => transport.subscribePush('engines:state-changed', cb),
        onProgress: cb => transport.subscribePush('engines:progress-changed', cb),
        onProgressRemove: cb =>
            transport.subscribePush('engines:progress-cleared', ({ key }) => cb(key)),
        update: (engineType, nodeId) =>
            fireCommand(transport, { command: 'update', engineType, nodeId })
    }
}
