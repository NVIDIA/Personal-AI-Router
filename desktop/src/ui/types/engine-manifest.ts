// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine manifest -- the single, immutable declaration of what an engine is.
 * Never broadcast over the wire; loaded locally from the engine registry.
 * Replaces BackendManifest, BackendCapabilities, BackendDisplayNames, BackendDefaultLinks.
 */
import type { SupportedPlatform } from '@/shared/types/platform'

export interface EngineHubConfig {
    /** Display label shown in the hub source selector (e.g. 'Ollama'). */
    label: string
    /** Public catalog URL that returns a JSON model list. */
    url: string
}

export interface EngineCaps {
    hasExpiry: boolean
    hasEject: boolean
    /** Platforms where PAIR can auto-install this engine. Empty = no auto-install. */
    hasInstall: SupportedPlatform[]
    /** Whether the engine has its own server port (vs CLI-per-request). */
    hasEnginePort: boolean
    /** Whether the engine requires a user-configured install path. */
    hasInstallPath: boolean
    /** When true, show an "open in browser" button that opens the proxy URL when running. */
    hasProxyWebUI: boolean
    /** When true, show a preferred node picker to pin proxy routing to a specific cluster node. */
    hasPreferredNode: boolean
    /** When true, surface a global error alert when the engine process crashes. */
    hasCrashAlert: boolean
    /** When true, show the model search/pull dropdown only while the engine process is running. */
    hasModelSearchOnlyWhenRunning: boolean
    /** When true, model operations work while the engine process is stopped (file-based weights). */
    modelOpsWhenStopped: boolean
    /** When true, show the Delete action in the model action menu. */
    hasDeleteModel: boolean
    /**
     * When true, the engine serves exactly one model per process and the model is
     * chosen before it starts, so Engine settings offers a "Model to serve" field.
     * The engine manager persists the choice and restarts the engine onto it.
     */
    hasServedModel: boolean
    /**
     * When true, the engine restarts as part of deleting a model, so Delete asks
     * for confirmation first. The engine manager owns the restart; this flag only
     * tells the UI that deleting has that side effect.
     */
    restartsOnModelDelete?: boolean
    /**
     * If the engine has its own public model catalog (e.g. ollama.com), provide the
     * hub config here. Presence doubles as the truthy flag for the hub source selector.
     */
    engineHub?: EngineHubConfig
}
