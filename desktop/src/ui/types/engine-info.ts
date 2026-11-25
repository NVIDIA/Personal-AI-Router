// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    EngineType,
    EngineSource,
    EngineUpdateAvailable,
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
    /**
     * Whether {@link models} was read. False when the engine's list could not
     * be, so an empty list is not shown as "nothing downloaded".
     */
    modelsKnown: boolean
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
    /** Present when the engine manager reports a supported update. */
    updateAvailable?: EngineUpdateAvailable
}
