// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { SupportedPlatform } from '@/shared/types/platform'

/**
 * Packaging constants for the `inference-dispatcher` Go client.
 *
 * The dispatcher is deliberately not part of the services binary inventory in
 * `modular-binaries.ts`: it speaks no JSON-RPC, is absent from
 * `services/versions.json`, and is never supervised by the broker. It is an
 * ordinary HTTP client, spawned once per Inference Demo request, whose source
 * lives in the monorepo's `scripts/inference-dispatcher` module.
 *
 * It nonetheless ships **inside `cli-bin/`**, beside the services binaries,
 * because the terminal interface runs the same demo and finds the dispatcher the
 * same way it finds the broker: next to its own executable. A separate resource
 * directory would mean either a second copy of the binary in every package or a
 * second resolution rule in `nvpair-tui`, and neither is worth keeping the
 * inventory assertion free of one named exception.
 *
 * Shared by `scripts/build-modular-binaries.ts` (producer),
 * `electron-builder.config.ts` (packaging assertion), and
 * `src/electron/inference-demo.ts` (runtime resolution).
 */

export const INFERENCE_DISPATCHER_BASE_NAME = 'inference-dispatcher'

export function inferenceDispatcherFileName(platform: SupportedPlatform): string {
    return platform === 'win32'
        ? `${INFERENCE_DISPATCHER_BASE_NAME}.exe`
        : INFERENCE_DISPATCHER_BASE_NAME
}
