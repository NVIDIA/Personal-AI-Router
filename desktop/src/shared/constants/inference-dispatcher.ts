// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { SupportedPlatform } from '@/shared/types/platform'

/**
 * Packaging constants for the `inference-dispatcher` Go client.
 *
 * The dispatcher is deliberately not part of the services binary inventory in
 * `modular-binaries.ts`: it speaks no JSON-RPC, is absent from
 * `services/versions.json`, and is never supervised by the broker. Its source
 * lives in the monorepo's `scripts/inference-dispatcher` module and it ships in
 * its own `extraResources` directory so `cli-bin/` can keep asserting an exact
 * match against the services inventory.
 *
 * Shared by `scripts/build-inference-dispatcher.ts` (producer),
 * `electron-builder.config.ts` (packaging assertion), and
 * `src/electron/inference-demo.ts` (runtime resolution).
 */

export const INFERENCE_DISPATCHER_BASE_NAME = 'inference-dispatcher'

/**
 * Resource directory holding the dispatcher, relative to `resourcesPath` in a
 * packaged build and to the desktop project root in development.
 */
export const INFERENCE_DISPATCHER_RESOURCE_DIR = 'tools'

export function inferenceDispatcherFileName(platform: SupportedPlatform): string {
    return platform === 'win32'
        ? `${INFERENCE_DISPATCHER_BASE_NAME}.exe`
        : INFERENCE_DISPATCHER_BASE_NAME
}
