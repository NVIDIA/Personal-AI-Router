// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineType, ModelExpiry } from '@/shared/types/engines'

// The engines `nvpair-engine-manager` ships a manifest for, and therefore the
// only ones PAIR can install, run or route to.
export const EngineTypes = ['ollama', 'lm-studio', 'llama-cpp'] as const

// Kept as a distinct export so a future engine can ship behind it rather than
// appearing the moment its type exists.
export const EnabledEngineTypes: EngineType[] = ['ollama', 'lm-studio', 'llama-cpp']

/**
 * How `nvpair-engine-manager` spells each engine on the wire. This is the
 * single translation table — use `engineManagerName()` and
 * `engineTypeFromManagerName()` rather than re-deriving it from a literal.
 */
export const EngineManagerNames = {
    ollama: 'ollama',
    'lm-studio': 'lmstudio',
    'llama-cpp': 'llamacpp'
} as const satisfies Record<EngineType, string>

export const EngineSources = ['bundled', 'detected', 'installed'] as const

export const EngineDisplayNames: Record<EngineType, string> = {
    ollama: 'Ollama',
    'lm-studio': 'LM Studio',
    'llama-cpp': 'llama.cpp'
} as const

/** Default docs/install URLs for built-in backends. Single source of truth for UI and adapter buildInfo(). */
export const EngineDefaultLinks: Record<EngineType, { docsUrl: string; installUrl: string }> = {
    ollama: { docsUrl: 'https://docs.ollama.com/', installUrl: 'https://ollama.com/download' },
    'lm-studio': { docsUrl: 'https://lmstudio.ai/docs', installUrl: 'https://lmstudio.ai/' },
    'llama-cpp': {
        docsUrl: 'https://github.com/ggml-org/llama.cpp/tree/master/docs',
        installUrl: 'https://github.com/ggml-org/llama.cpp/releases'
    }
} as const

export const ModelItemStatuses = ['idle', 'loading', 'loaded', 'ejecting', 'pulling'] as const

export const ModelExpiries = ['0', '1s', '10s', '1m', '10m', '-1'] as const

export const ModelExpiryLabels: Record<ModelExpiry, string> = {
    '0': 'Immediately',
    '1s': '1 second',
    '10s': '10 seconds',
    '1m': '1 minute',
    '10m': '10 minutes',
    '-1': 'Never'
} as const

export const EngineProcessStatuses = [
    'running',
    'stopped',
    'not-installed',
    'installing',
    'uninstalling',
    'starting',
    'stopping',
    'initializing'
] as const

/**
 * Engine progress domain -- delivered from the backend as progress push events.
 * Unified progress channel for all operations: install, pull, load, etc.
 * Keyed by ${nodeId}:${engineType}:${operation}:${model?} for concurrent ops.
 */

export const EngineOperationTypes = [
    'install',
    'uninstall',
    'pull',
    'load',
    'unload',
    'delete'
] as const
