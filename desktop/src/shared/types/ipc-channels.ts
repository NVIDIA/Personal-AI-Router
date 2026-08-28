// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { WsInvokeResponse } from '@/shared/types/ws-channels'
import type { ModularLogLevel } from '@/shared/constants/modular-runtime'
import type { ServiceBridgeInvokeRequest } from './service-bridge'
import type { UpdateStatus } from '@/shared/types/update'
import type { DemoState } from '@/shared/types/inference-demo'

/**
 * IPC contract types for Electron-native operations only.
 *
 * IPC is used for: window management, tray, clipboard, service lifecycle,
 * the first-run flag, and the Electron-hosted service
 * bridge. Service data keeps the existing logical invoke/push channel
 * contract in `@/shared/types/ws-channels.ts`, but Electron transports it
 * through the `service-bridge:*` envelope below.
 *
 * `safeHandle()` in the main process narrows the request/response types per
 * channel; preload calls `invokeAndUnwrap<Ch['response']>(…)` against the same
 * map.
 */

export type IpcResult<T = void> = { success: true; data: T } | { success: false; error: string }

/**
 * Lifecycle of the Electron-owned modular service connector.
 *
 * `disconnected` means no broker is running, whether the user stopped it or it
 * exited on its own; `ServiceStatus.error` tells those two apart.
 */
export type ConnectorStatus = 'disconnected' | 'connecting' | 'connected' | 'reconnecting'

/** Status of the Electron ↔ CLI child process connection. */
export interface ServiceStatus {
    connectorStatus: ConnectorStatus
    weSpawned: boolean
    /** User-displayable reason the most recent startup or runtime attempt failed. */
    error?: string
}

/**
 * Application + bundled backend service binary versions, sourced from
 * `app.getVersion()` and the shipped `cli-bin/manifest.json` (stamped at build
 * time by `scripts/build-modular-binaries.ts`).
 */
export interface ServiceVersions {
    appVersion: string
    modularProduct: string
    binaries: { name: string; version: string }[]
    /** SPDX-ish id parsed at runtime from the shipped LICENSE; '' if unavailable. */
    licenseType: string
}

/**
 * Synchronous preload bootstrap payload.
 *
 * Reserved for preload bootstrap data. The modular Electron bridge currently
 * does not require browser gateway connection details, so this is `null`.
 */
export type BootstrapPayload = { port: number; token: string } | null

export interface IpcChannelMap {
    // -- Window --
    'window:close': { request: void; response: void }
    'window:open-overview': { request: void; response: void }
    /** Tray -> open/focus Overview and expand a node's inline engine settings. */
    'window:focus-node': { request: { nodeId: string }; response: void }
    /** Overview renderer -> main: subscribed and ready to receive `overview:command`. */
    'overview:ready': { request: void; response: void }
    'window:open-external': { request: string; response: void }
    'window:quit': { request: void; response: void }
    'window:is-maximized': { request: void; response: boolean }
    'window:maximize': { request: void; response: void }
    'window:unmaximize': { request: void; response: void }
    'window:minimize': { request: void; response: void }
    'window:copy-to-clipboard': { request: string; response: void }
    'window:copy-image-to-clipboard': { request: { base64: string }; response: void }
    'window:save-debug-logs': { request: void; response: string | null }

    // -- Tray --
    'tray:resize': { request: number; response: number }
    'tray:show-menu': { request: void; response: void }

    // -- Service lifecycle (controls the CLI child process) --
    'service:get-status': { request: void; response: ServiceStatus }
    'service:stop': { request: void; response: void }
    'service:start': { request: void; response: void }
    'service:restart': { request: void; response: void }
    'service:get-log-level': { request: void; response: ModularLogLevel }
    'service:set-log-level': { request: { level: ModularLogLevel }; response: void }
    'service:open-log-file': { request: void; response: void }
    'service:open-log-dir': { request: void; response: void }
    'service:get-versions': { request: void; response: ServiceVersions }
    'service:open-license': { request: void; response: void }
    'service:open-third-party-licenses': { request: void; response: void }

    // -- Inference Demo (node-local synthetic load, scheduled in main) --
    'demo:get-state': { request: void; response: DemoState }
    /** Rejects if a demo is already running locally or no engine responded. */
    'demo:start': { request: void; response: DemoState }
    /** Cancels future submissions only; in-flight work finishes naturally. */
    'demo:stop': { request: void; response: DemoState }

    // -- Settings (Electron-only: ui-config first-run flag) --
    /** Whether first-run onboarding still needs to be completed or explicitly dismissed. */
    'settings:is-first-run': { request: void; response: boolean }
    /** Persist that first-run onboarding completed or was explicitly dismissed. */
    'settings:complete-first-run': { request: void; response: void }

    // -- App data wipe (Electron-only: destructive reset of PAIR-owned storage) --
    /**
     * Whether a wipe will auto-relaunch after quit. Packaged builds relaunch;
     * unpackaged (dev) builds quit only — the UI must tell the user to restart.
     */
    'app:get-wipe-plan': { request: void; response: { willRelaunch: boolean } }
    /**
     * Stop the app and wipe all PAIR-owned data. Packaged builds relaunch into
     * first-run; unpackaged builds quit and require a manual restart.
     */
    'app:wipe-data': { request: void; response: void }

    // -- Auto-update (electron-updater, packaged builds only) --
    'update:get-status': { request: void; response: UpdateStatus }
    'update:check': { request: void; response: void }
    'update:download': { request: void; response: void }
    'update:install': { request: void; response: void }

    // -- Service bridge (renderer pairApi transport, logical channel contract over IPC) --
    'service-bridge:invoke': {
        request: ServiceBridgeInvokeRequest
        response: WsInvokeResponse<ServiceBridgeInvokeRequest['channel']>
    }
}

/**
 * Main → renderer broadcasts. These have no request half; they are one-way
 * pushes sent via `webContents.send`, so they cannot live in `IpcChannelMap`.
 * Declaring them here keeps the payload type checked on both ends rather than
 * relying on a cast in preload.
 */
export interface IpcPushChannelMap {
    /** Node-local Inference Demo progress. Broadcast to every window. */
    'demo:state': DemoState
}

export type IpcPushChannelKey = keyof IpcPushChannelMap

/**
 * Handler signature for a typed IPC channel.
 * `safeHandle` supplies `IpcMainInvokeEvent` for `E`.
 * Accepts sync or async returns so handlers don't need to be `async` when they have nothing to await.
 */
export type IpcHandlerFn<Req, Res, E> = Req extends void
    ? (event: E) => Res | Promise<Res>
    : (event: E, payload: Req) => Res | Promise<Res>

export type IpcChannelKey = keyof IpcChannelMap
