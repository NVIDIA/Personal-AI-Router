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
        engineHub: { label: 'LM Studio', url: 'https://lmstudio.ai/models' }
    },
    mlx: {
        hasExpiry: false,
        // mlx-lm still has no unload of its own -- a model leaves memory only
        // when the process ends. mlx-pool makes that actionable: it runs one
        // model per child process, so ending the child releases the weights.
        // Eject is therefore backed by something real, and is refused (409)
        // while the model is serving a request rather than cutting it off.
        hasEject: true,
        // Apple Silicon only. MLX is a Metal framework; there is no MLX on
        // Windows or Linux to install.
        hasInstall: ['darwin'],
        hasEnginePort: true,
        hasInstallPath: false,
        hasProxyWebUI: false,
        hasPreferredNode: false,
        hasCrashAlert: false,
        hasModelSearchOnlyWhenRunning: true,
        modelOpsWhenStopped: false,
        hasDeleteModel: true,
        // There is no MLX catalogue to list, so the hub search returns nothing
        // and the only way to add a model is to name it: a Hugging Face repo id
        // to download, or an absolute path to a model directory built locally
        // (a quantization has no repo id and is invisible to any catalogue).
        // The engine validates the path before recording it.
        acceptsTypedModelId: true,
        // No restart on delete, unlike LM Studio: mlx-lm rescans the Hugging
        // Face cache on every /v1/models, so a deletion is visible immediately.
        engineHub: { label: 'MLX Community', url: 'https://huggingface.co/mlx-community' }
    }
}
