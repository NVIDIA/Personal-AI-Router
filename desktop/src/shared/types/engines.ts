// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import {
    EngineManagerNames,
    EngineOperationTypes,
    EngineProcessStatuses,
    EngineSources,
    EngineTypes,
    ModelExpiries,
    ModelItemStatuses
} from '@/shared/constants/engines'

/**
 * Canonical engine identifier — a closed literal union of every engine PAIR
 * ships. This codebase has no concept of "custom" or "user-registered"
 * engines: every engine is built in to the binary and listed in the
 * `EngineTypes` const tuple. Always prefer this type over `string` for
 * engine identifiers so the compiler flags typos like `'olllama'` or stale
 * literal values. Strings arriving from service bridge payloads or config JSON
 * must be narrowed via `isEngineType()` at the boundary.
 */
export type EngineType = (typeof EngineTypes)[number]

/**
 * An engine identifier in `nvpair-engine-manager`'s spelling, as it appears in
 * JSON-RPC params and notification payloads. Convert at the boundary with
 * `engineManagerName()` / `engineTypeFromManagerName()`; keep `EngineType`
 * everywhere inside PAIR.
 */
export type EngineManagerName = (typeof EngineManagerNames)[EngineType]

export type EngineSource = (typeof EngineSources)[number]

export interface EngineStatusData {
    engineType: EngineType
    nodeId: string
    processStatus: EngineProcessStatus
    /**
     * Configured bind port for the engine HTTP server when installed (same value when stopped vs running).
     * Null when not installed, or for an engine with no server port.
     */
    enginePort: number | null
    /** PAIR's proxy port for this engine (e.g. Ollama 54301). Always set when the engine is installed. */
    proxyPort: number | null
    /**
     * Installed engine binary version reported by the owning node, when the
     * current status source exposes it. Undefined for nodes that have not
     * reported version data or engines that are not installed.
     */
    installedVersion?: string
    /**
     * True when the owning node's engine manager could install this engine on
     * request: that platform declares a way to obtain it, and whatever that way
     * needs is on that machine. Only the service can answer it, so it travels with
     * the state instead of being guessed from a per-engine list of operating
     * systems. The engine card's Install button comes from this, which is how a
     * card avoids offering an install its own service would refuse.
     */
    canInstall?: boolean
    /**
     * Who owns the copy of the engine that is on the owning node: `'pair'` for an
     * install PAIR made and can take back out, a package manager's name
     * (`'Homebrew'`) for its install, `'other'` for a copy that was merely found,
     * and absent when nothing is installed. Uninstall appears only for `'pair'`,
     * because it is refused for everything else. Read it with
     * `isPairManagedInstall` / `engineOwnerTool`.
     */
    owner?: string
}

export type ModelItemStatus = (typeof ModelItemStatuses)[number]

export type ModelExpiry = (typeof ModelExpiries)[number]

/** nodeId → engineType → status (sparse: only types reported by the service). */
export type EngineStatusByNode = Map<string, Map<EngineType, EngineStatusData>>

/**
 * Engine status domain -- broadcast per engine per node when process state changes.
 * Replaces the process/port/autoStart portion of the monolithic BackendInfo.
 */

export type EngineProcessStatus = (typeof EngineProcessStatuses)[number]

export type EngineOperationType = (typeof EngineOperationTypes)[number]

export interface EngineProgress {
    engineType: EngineType
    nodeId: string
    nodeName: string
    operation: EngineOperationType
    /** Set for model operations (pull, load, unload, delete). */
    model?: string
    /** 'started' | 'downloading' | 'complete' | 'error' | engine-specific status string. */
    status: string
    percent?: number
    completed?: number
    total?: number
    error?: string
}

/**
 * Available engine-update signal for one engine on one node. The backend
 * reports it per node; PAIR receives it through `engines:state-changed` and
 * the renderer's `engine-update-available.store`, so the Edit Node UI can
 * surface an Update affordance for any node — local or remote — the user can
 * reach in the cluster.
 */
export interface EngineUpdateAvailable {
    nodeId: string
    engineType: EngineType
    currentVersion: string
    latestVersion: string
    releaseUrl: string
    installType: 'managed' | 'external'
}

export interface EngineModels {
    engineType: EngineType
    nodeId: string
    models: ModelItem[]
    /**
     * Whether the model list was read. False when it could not be, such as a
     * peer's stopped vLLM whose downloaded models never arrived, so an empty
     * list must not be shown as "nothing downloaded".
     */
    inventoryKnown: boolean
}

export interface ModelItem {
    name: string
    size: number
    downloaded: boolean
    status: ModelItemStatus
    parameterSize: string
    quantization: string
    family: string
    digest: string
    /** VRAM consumed by the model when loaded (bytes). */
    sizeVram: number | null
    /** Timestamp when the engine will auto-unload this model. */
    expiresAt: string | null
    /** User-configured keep_alive duration sent when loading the model. */
    expiry: ModelExpiry
    /**
     * Original key used to pull/download this model (e.g. HuggingFace `author/repo/file.gguf`).
     * Stored alongside the model so sync can reproduce the pull on other nodes.
     * Absent for models whose `name` is already a valid pull key (e.g. Ollama tags).
     */
    pullKey?: string
    /**
     * Engine-reported model capabilities (lowercase). Known values include
     * `'vision'`, `'tools'`, `'embedding'`, `'completion'`, `'thinking'`.
     * Empty array when the engine does not expose capability metadata for
     * this model. Consumers that don't care can ignore; the chat UI uses
     * `'vision'` to gate image attachment affordances.
     */
    capabilities: string[]
}
