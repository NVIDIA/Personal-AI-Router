// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { windowActionApi, IWindowActionApi } from '@/preload/api/window.api'
import { serviceApi, IServiceApi } from '@/preload/api/service.api'
import { updateApi, IUpdateApi } from '@/preload/api/update.api'
import { invokeAndUnwrap } from '@/preload/api/unwrap'
import { ipcRenderer } from 'electron'
import { PlatformDisplayName } from '@/shared/types/platform'
import { platformDisplayName, currentPlatform } from '@/shared/utils/platform'
import { inferenceDemoApi, type IInferenceDemoApi } from '@/preload/api/inference-demo.api'

export type { IWindowActionApi } from '@/preload/api/window.api'
export type { IServiceApi } from '@/preload/api/service.api'
export type { IUpdateApi } from '@/preload/api/update.api'
export type { IInferenceDemoApi } from '@/preload/api/inference-demo.api'

/**
 * Electron-native API exposed via contextBridge.
 * Service communication (nodes, engines, models, etc.) goes through `pairApi`
 * over the `service-bridge:*` IPC envelope. This interface only covers
 * operations that require Electron main process access.
 */
export interface IWindowApi {
    /** OS of the machine running the service ('Windows' | 'Linux' | 'MacOS'). */
    platform: PlatformDisplayName
    /** Electron window management, clipboard, external links, tray. */
    window: IWindowActionApi
    /** CLI process lifecycle (start/stop/restart, open terminal, ui-config settings). */
    service: IServiceApi
    /** Packaged-app auto-update (electron-updater). */
    update: IUpdateApi
    /** Node-local Inference Demo: synthetic load sent through PAIR's proxies. */
    inferenceDemo: IInferenceDemoApi
    /** Whether first-run onboarding still needs to be completed or explicitly dismissed. */
    isFirstRun(): Promise<boolean>
    /** Persist that first-run onboarding completed or was explicitly dismissed. */
    completeFirstRun(): Promise<void>
    /**
     * Whether wipe will auto-relaunch (packaged) or quit and require a manual
     * restart (unpackaged / `electron-vite dev`).
     */
    getWipePlan(): Promise<{ willRelaunch: boolean }>
    /**
     * Stop the app and delete all PAIR-owned data. Packaged builds relaunch into
     * first-run; unpackaged builds quit only. Requires UI confirmation first.
     */
    wipeAppData(): Promise<void>
}

export const windowApi: IWindowApi = {
    platform: platformDisplayName(currentPlatform()),
    window: windowActionApi,
    service: serviceApi,
    update: updateApi,
    inferenceDemo: inferenceDemoApi,
    isFirstRun: () => invokeAndUnwrap<boolean>(ipcRenderer.invoke('settings:is-first-run')),
    completeFirstRun: () =>
        invokeAndUnwrap<void>(ipcRenderer.invoke('settings:complete-first-run')),
    getWipePlan: () =>
        invokeAndUnwrap<{ willRelaunch: boolean }>(ipcRenderer.invoke('app:get-wipe-plan')),
    wipeAppData: () => invokeAndUnwrap<void>(ipcRenderer.invoke('app:wipe-data'))
}
