// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { EngineType, ModelExpiry } from '@/shared/types/engines'

// The engines `nvpair-engine-manager` ships a manifest for, and therefore the
// only ones PAIR can install, run or route to.
export const EngineTypes = ['ollama', 'lm-studio', 'llama-cpp', 'vllm'] as const

// Kept as a distinct export so a future engine can ship behind it rather than
// appearing the moment its type exists.
export const EnabledEngineTypes: EngineType[] = ['ollama', 'lm-studio', 'llama-cpp', 'vllm']

/**
 * Engines whose model list is read from disk, so it is available while the engine
 * is stopped. For every other engine the list comes from the running engine and is
 * cleared when it stops. The `modelOpsWhenStopped` capability must agree.
 */
export const EnginesListingModelsWhenStopped: readonly EngineType[] = ['vllm']

/**
 * Engines whose expiry setting is how long the engine may sit idle before it
 * unloads its model, kept by the engine manager and not sent with each load. The
 * `hasExpiry` capability must agree.
 */
export const EnginesWithIdleTimeout: readonly EngineType[] = ['vllm']

/** The idle limits offered, as expiry values. Idle limits are never shorter than a minute: reloading takes longer than that to pay off. */
export const IdleTimeoutExpiries: readonly ModelExpiry[] = ['1m', '5m', '10m', '30m', '1h', '-1']

/**
 * How `nvpair-engine-manager` spells each engine on the wire. This is the
 * single translation table — use `engineManagerName()` and
 * `engineTypeFromManagerName()` rather than re-deriving it from a literal.
 */
export const EngineManagerNames = {
    ollama: 'ollama',
    'lm-studio': 'lmstudio',
    'llama-cpp': 'llamacpp',
    vllm: 'vllm'
} as const satisfies Record<EngineType, string>

export const EngineSources = ['bundled', 'detected', 'installed'] as const

export const EngineDisplayNames: Record<EngineType, string> = {
    ollama: 'Ollama',
    'lm-studio': 'LM Studio',
    'llama-cpp': 'llama.cpp',
    vllm: 'vLLM'
} as const

/** Default docs/install URLs for built-in backends. Single source of truth for UI and adapter buildInfo(). */
export const EngineDefaultLinks: Record<EngineType, { docsUrl: string; installUrl: string }> = {
    ollama: { docsUrl: 'https://docs.ollama.com/', installUrl: 'https://ollama.com/download' },
    'lm-studio': { docsUrl: 'https://lmstudio.ai/docs', installUrl: 'https://lmstudio.ai/' },
    'llama-cpp': {
        docsUrl: 'https://github.com/ggml-org/llama.cpp/tree/master/docs',
        installUrl: 'https://github.com/ggml-org/llama.cpp/releases'
    },
    vllm: {
        docsUrl: 'https://docs.vllm.ai/',
        installUrl: 'https://docs.vllm.ai/en/latest/getting_started/installation.html'
    }
} as const

export const ModelItemStatuses = ['idle', 'loading', 'loaded', 'ejecting', 'pulling'] as const

export const ModelExpiries = ['0', '1s', '10s', '1m', '5m', '10m', '30m', '1h', '-1'] as const

export const ModelExpiryLabels: Record<ModelExpiry, string> = {
    '0': 'Immediately',
    '1s': '1 second',
    '10s': '10 seconds',
    '1m': '1 minute',
    '5m': '5 minutes',
    '10m': '10 minutes',
    '30m': '30 minutes',
    '1h': '1 hour',
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
