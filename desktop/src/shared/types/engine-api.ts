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
 * One normalized model row returned by an engine-owned hub source (a committed
 * Ollama library list, the LM Studio community catalog, llama.cpp's Hugging Face
 * GGUF catalog). `nvpair-engine-manager` owns every source and normalizes each
 * upstream registry into this shape, so the desktop app and the terminal
 * interface serve the same models and the renderer maps a row for display
 * without per-engine JSON parsing. `id`/`name` carry the pull-ready identifier
 * the engine's `pull_model` action expects.
 */
/**
 * One engine's outcome from `engine:uninstall-managed`, the backend sweep that
 * removes the engines PAIR installed.
 *
 * Neither `removed` nor `error` means the engine was left alone because PAIR has
 * no record of installing it — the expected result for a user's own install, and
 * not a failure.
 */
export interface ManagedEngineUninstall {
    engine: string
    removed: boolean
    error: string
}

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
    | 'pullModel'
    | 'loadModel'
    | 'unloadModel'
    | 'deleteModel'
    | 'setModelExpiry'

/** Payload for engine commands sent from the UI. */
export interface EngineCommandPayload {
    command: EngineCommandType
    engineType: EngineType
    nodeId: string
    model?: string
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
