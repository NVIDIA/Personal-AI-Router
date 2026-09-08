// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine command and initial state types for the frontend API.
 *
 * Commands are fire-and-forget (void). Errors arrive via errors:update push events.
 * All state flows via push events, not command responses.
 */
import type { EngineStatusData, EngineType } from '@/shared/types/engines'
import type { EngineModels, EngineProgress, EngineUpdateAvailable } from '@/shared/types/engines'

/**
 * One normalized model row returned by an engine-owned hub source (Ollama
 * library scrape, LM Studio community catalog). The Electron-main model-hub
 * module normalizes each upstream registry into this shape so the renderer
 * maps it to a display row without per-engine JSON parsing. `id`/`name` carry
 * the pull-ready identifier the engine's `pull_model` action expects.
 */
export interface EngineHubModel {
    id: string
    name: string
    author: string
    url: string
    size?: number
    downloads: number
    likes: number
    /** ISO timestamp of the model's last update. */
    updatedAt: string
    tags: string[]
    family?: string
    parameterSize?: string
}

/** Response for the `engine:search-hub` channel. */
export interface EngineHubSearchResponse {
    models: EngineHubModel[]
}

/** Engine command discriminator. */
export type EngineCommandType =
    | 'toggle'
    | 'install'
    | 'installAll'
    | 'uninstall'
    | 'update'
    | 'setPorts'
    | 'pullModel'
    | 'loadModel'
    | 'unloadModel'
    | 'deleteModel'
    | 'setModelExpiry'
    | 'setServedModel'

/** Payload for engine commands sent from the UI. */
export interface EngineCommandPayload {
    command: EngineCommandType
    engineType: EngineType
    nodeId: string
    /**
     * The model an operation acts on. For `setServedModel` it is the model the
     * engine should serve from its next start; an empty string clears the
     * choice, leaving an engine that requires one unable to start until it is
     * set again.
     */
    model?: string
    /**
     * `setPorts` only. The engine HTTP server port to apply. Omitted when the
     * server port did not change so the bridge sends only what the user edited.
     */
    enginePort?: number
    /**
     * `setPorts` only. The proxy listen port to apply. Omitted when the proxy
     * port did not change. When both ports change the bridge orders the
     * transaction so the engine and proxy never fight over a port (incl. swaps).
     */
    proxyPort?: number
    expiry?: string
}

/** Canonical engine state snapshot sent on connect/reconnect. */
export interface EngineStateSnapshot {
    statuses: EngineStatusData[]
    models: EngineModels[]
    activeProgress: EngineProgress[]
    updateAvailable: EngineUpdateAvailable[]
}

/** Patch stream for durable engine state keyed by node and engine. */
export interface EngineStatePatch {
    nodeId: string
    engineType: EngineType
    status?: EngineStatusData
    models?: EngineModels
    updateAvailable?: EngineUpdateAvailable | null
}

export type EngineInitialState = EngineStateSnapshot
