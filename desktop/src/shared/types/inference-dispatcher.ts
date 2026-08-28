// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Types describing the standalone `inference-dispatcher` Go binary's surface.
 *
 * The binary is a third-party-style HTTP client: one backend, one model, N
 * requests per invocation. PAIR drives it only from the Inference Demo
 * (`@/electron/inference-demo`), which spawns it once per scheduled request and
 * reads its `--list-models` inventory.
 */

export type DispatcherBackend = 'ollama' | 'lmstudio'

/** One entry from the binary's `--list-models` JSON inventory. */
export interface DispatcherModel {
    name: string
    capabilities?: string[]
    type?: string
}
