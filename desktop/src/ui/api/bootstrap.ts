// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createPairApi, type IPairApi } from '@/ui/api/pair-api'
import type { IWindowApi } from '@/preload/api/index'
import type { PreloadServiceTransport } from '@/shared/types/service-bridge'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

declare global {
    interface Window {
        windowApi: IWindowApi
        pairApi: IPairApi
        __PAIR_TRANSPORT__?: PreloadServiceTransport
    }
}

/**
 * True when running inside Electron with the preload bridge active.
 *
 * `typeof` only guards the outermost identifier, so `typeof window.windowApi`
 * still throws when `window` itself is undefined. Checking `window` first keeps
 * this module importable outside a DOM — without it, any unit test that pulls in
 * a renderer store transitively fails at import time under the node environment.
 */
export const isElectron: boolean =
    typeof window !== 'undefined' && typeof window.windowApi !== 'undefined'

let serviceTransport: PreloadServiceTransport | null = null

export function getPairTransport(): PreloadServiceTransport | null {
    return serviceTransport
}

/**
 * Synchronous API setup — called once before React mounts.
 *
 * The modular app is Electron-only. The preload exposes a service transport
 * that maps renderer invoke/push semantics onto Electron IPC and stdio
 * JSON-RPC subprocesses.
 */
export function setupApis(): void {
    if (isElectron && window.__PAIR_TRANSPORT__) {
        serviceTransport = window.__PAIR_TRANSPORT__
        window.pairApi = createPairApi(serviceTransport)
        return
    }

    console.error(
        `${APP_DISPLAY_NAME} preload transport is unavailable. Browser mode is not supported.`
    )
}
