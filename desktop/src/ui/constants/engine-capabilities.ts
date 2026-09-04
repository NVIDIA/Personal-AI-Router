// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineType } from '@/shared/types/engines'
import type { EngineCaps } from '@/ui/types/engine-manifest'

export const EngineCapabilities: Record<EngineType, EngineCaps> = {
    ollama: {
        // Ollama has no model keep-alive/expiry UI; unload uses unload_model
        // (POST /api/generate with keep_alive: 0). See docs/services-parity.md#models.
        hasExpiry: false,
        // Ollama unloads via POST /api/generate with keep_alive: 0 (unload_model).
        hasEject: true,
        hasInstall: ['win32', 'darwin', 'linux'],
        hasEnginePort: true,
        hasInstallPath: false,
        hasProxyWebUI: false,
        hasPreferredNode: false,
        hasCrashAlert: false,
        hasModelSearchOnlyWhenRunning: true,
        modelOpsWhenStopped: false,
        hasDeleteModel: true,
        hasServedModel: false,
        engineHub: { label: 'Ollama', url: 'https://ollama.com/library' }
    },
    'lm-studio': {
        hasExpiry: false,
        // LM Studio's `unload_model` action (`lms unload`) is a real eject
        // path. The backend reports which models are loaded in memory, so
        // ModelRow.tsx offers Eject only for a model whose `status` is
        // `'loaded'`.
        hasEject: true,
        hasInstall: ['win32', 'darwin', 'linux'],
        hasEnginePort: true,
        hasInstallPath: false,
        hasProxyWebUI: false,
        hasPreferredNode: false,
        hasCrashAlert: false,
        hasModelSearchOnlyWhenRunning: true,
        modelOpsWhenStopped: false,
        hasDeleteModel: true,
        // LM Studio answers /v1/models from an index it builds at startup and
        // exposes no rescan, so nvpair-engine-manager's delete_model restarts the
        // server. Deleting therefore interrupts inference and needs a warning.
        restartsOnModelDelete: true,
        hasServedModel: false,
        engineHub: { label: 'LM Studio', url: 'https://lmstudio.ai/models' }
    },
    vllm: {
        hasExpiry: false,
        // vLLM keeps its one served model resident for the life of the process.
        // There is nothing to eject short of stopping the engine.
        hasEject: false,
        // vLLM publishes no Windows build and no macOS GPU build, so PAIR only
        // offers to install it on Linux. Its manifest ships Linux platforms only,
        // so the engine reports unavailable everywhere else.
        hasInstall: ['linux'],
        hasEnginePort: true,
        hasInstallPath: false,
        hasProxyWebUI: false,
        hasPreferredNode: false,
        hasCrashAlert: false,
        hasModelSearchOnlyWhenRunning: true,
        // vLLM has no model-management surface: weights are fetched by the engine
        // itself when it starts, from the model id configured below.
        modelOpsWhenStopped: false,
        hasDeleteModel: false,
        hasServedModel: true
    }
}
