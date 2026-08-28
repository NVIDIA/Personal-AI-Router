// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    EngineType,
    EngineSource,
    ModelItemStatus,
    ModelExpiry,
    EngineProcessStatus
} from '@/shared/types/engines'

export interface ModelItem {
    name: string
    size: number
    downloaded: boolean
    status: ModelItemStatus
    parameterSize: string
    quantization: string
    family: string
    digest: string
    /** VRAM consumed by the model when loaded (bytes), from Ollama /api/ps */
    sizeVram: number | null
    /** Timestamp when Ollama will auto-unload this model, from Ollama /api/ps */
    expiresAt: string | null
    /** User-configured keep_alive duration sent when loading the model */
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

export interface BackendPrerequisite {
    name: string
    installed: boolean
    version?: string
    requiredVersion?: string
    installUrl: string
}

export interface BackendInstallProgress {
    status: string
    percent?: number
}

/**
 * Update availability info surfaced by the universal `EngineVersionChecker`.
 *
 * Cluster-replicated: every node runs its own checker against its own
 * installed engines, writes results into `ClusterState`, and broadcasts
 * `EngineUpdateAvailableEvent` / `EngineUpdateClearedEvent` to peers. Each
 * receiving node forwards them to its renderer as
 * `engines:state-changed` patches scoped to
 * the originating `nodeId`. The renderer's `engine-update-available.store`
 * keys entries by `(nodeId, engineType)` so the Edit Node window can show
 * an Update affordance for any node — local **or** remote — that the user
 * has access to. Entries are cleared automatically when the originating
 * node drops out of `ClusterState`.
 *
 * `installType`:
 * - `managed` — PAIR owns the binary (e.g. `{userData}/ollama`), so the UI
 *   shows a one-click "Update" button that dispatches the `update` engine
 *   command on the engine's owning node.
 * - `external` — the binary lives outside `{userData}` (user installed
 *   Ollama themselves, llama.cpp via brew, etc.). The UI shows an info
 *   banner with release notes and an optional CTA to install a PAIR-managed
 *   copy; PAIR never modifies external installs in place.
 */
export interface UpdateAvailableInfo {
    currentVersion: string
    latestVersion: string
    releaseUrl: string
    installType: 'managed' | 'external'
}

export interface BackendInfo {
    type: EngineType
    displayName: string
    source: EngineSource
    /** Whether the inference process (e.g. ollama serve) is running */
    processStatus: EngineProcessStatus
    /** Server port the inference process listens on (null if process is stopped) */
    port: number | null
    /** PAIR proxy port -- always running when the backend is installed */
    proxyPort: number | null
    /** Installed engine binary version reported by the owning node */
    installedVersion?: string
    /** Models on this backend with per-model status */
    models: ModelItem[]
    /** System-level dependencies required before install/run (local node only) */
    prerequisites?: BackendPrerequisite[]
    /** Progress info during install/uninstall (local node only, ephemeral) */
    installProgress?: BackendInstallProgress
    /** URL to the backend's documentation site */
    docsUrl?: string
    /** URL to download/install the backend */
    installUrl?: string
    /** User-configured path to the backend installation directory */
    installPath?: string
    /** True when the backend was stopped by user action (not a crash) */
    manuallyStopped?: boolean
    /** True when this backend should auto-start on app launch */
    autoStart?: boolean
    /**
     * Set when this backend's owning node has detected a newer GitHub
     * release. Replicated cluster-wide so the Edit Node UI can surface an
     * Update affordance for any node the user has access to, not only
     * `selfId`. See `UpdateAvailableInfo` above.
     */
    updateAvailable?: UpdateAvailableInfo
}
