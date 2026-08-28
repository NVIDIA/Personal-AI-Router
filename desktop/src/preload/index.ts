// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { contextBridge, ipcRenderer } from 'electron'
import { windowApi } from '@/preload/api/index'
import { pairTransport } from '@/preload/api/pair-transport.api'
import type { BootstrapPayload } from '@/shared/types/ipc-channels'

function parseBootstrap(value: unknown): BootstrapPayload {
    if (!value || typeof value !== 'object') return null
    const v = value as { port?: unknown; token?: unknown }
    if (typeof v.port !== 'number' || typeof v.token !== 'string') return null
    return { port: v.port, token: v.token }
}

const bootstrap: BootstrapPayload = parseBootstrap(ipcRenderer.sendSync('bootstrap:get'))

if (process.contextIsolated) {
    try {
        contextBridge.exposeInMainWorld('windowApi', windowApi)
        contextBridge.exposeInMainWorld('__PAIR_TRANSPORT__', pairTransport)
        contextBridge.exposeInMainWorld('__PAIR_BOOTSTRAP__', bootstrap)
    } catch (error) {
        console.error(error)
    }
} else {
    // @ts-ignore (define in dts)
    window.windowApi = windowApi
    // @ts-ignore (define in dts)
    window.__PAIR_TRANSPORT__ = pairTransport
    // @ts-ignore (define in dts)
    window.__PAIR_BOOTSTRAP__ = bootstrap
}
