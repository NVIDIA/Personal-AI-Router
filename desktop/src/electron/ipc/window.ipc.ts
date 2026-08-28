// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app, BrowserWindow, clipboard, Menu, nativeImage } from 'electron'
import { safeHandle } from '@/electron/ipc/safe-handle'
import { openExternalSafe } from '@/electron/open-external'
import { createOverviewWindow, focusNodeInOverview, markOverviewReady } from '@/electron/window'
import { warmEngineHubs } from '@/electron/model-hub'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import { resizeTrayWindow } from '@/electron/tray'
import { saveDebugLogs } from './debug-log-export'

export function registerWindowIpc(): void {
    safeHandle('window:close', event => {
        const win = BrowserWindow.fromWebContents(event.sender)
        if (win && !win.isDestroyed()) win.close()
    })

    safeHandle('window:open-overview', () => {
        createOverviewWindow()
    })

    safeHandle('window:focus-node', (_event, { nodeId }) => {
        focusNodeInOverview(nodeId)
    })

    // The renderer has mounted and subscribed, so first paint is already done.
    // Warming the hub caches here rather than on service connect keeps a slow or
    // hanging catalog fetch off the startup path the window's first paint shares.
    safeHandle('overview:ready', () => {
        markOverviewReady()
        warmEngineHubs()
    })

    safeHandle('window:open-external', (_event, url) => {
        openExternalSafe(url)
    })

    safeHandle('window:quit', () => {
        app.quit()
    })

    safeHandle('window:is-maximized', event => {
        const win = BrowserWindow.fromWebContents(event.sender)
        if (!win || win.isDestroyed()) return false
        return win.isMaximized()
    })

    safeHandle('window:maximize', event => {
        const win = BrowserWindow.fromWebContents(event.sender)
        if (win && !win.isDestroyed()) win.maximize()
    })

    safeHandle('window:unmaximize', event => {
        const win = BrowserWindow.fromWebContents(event.sender)
        if (win && !win.isDestroyed()) win.unmaximize()
    })

    safeHandle('window:minimize', event => {
        const win = BrowserWindow.fromWebContents(event.sender)
        if (win && !win.isDestroyed()) win.minimize()
    })

    safeHandle('tray:resize', (_event, height) => {
        return resizeTrayWindow(height)
    })

    safeHandle('tray:show-menu', event => {
        const menu = Menu.buildFromTemplate([
            { label: 'Overview', click: () => createOverviewWindow() },
            { type: 'separator' },
            { label: `Exit ${APP_DISPLAY_NAME}`, click: () => app.quit() }
        ])
        const win = BrowserWindow.fromWebContents(event.sender)
        if (win) menu.popup({ window: win })
    })

    safeHandle('window:copy-to-clipboard', (_event, text) => {
        clipboard.writeText(text)
    })

    safeHandle('window:copy-image-to-clipboard', (_event, payload) => {
        const buf = Buffer.from(payload.base64, 'base64')
        if (buf.length === 0) {
            throw new Error('Empty image data')
        }
        const img = nativeImage.createFromBuffer(buf)
        if (img.isEmpty()) {
            throw new Error('Could not decode image for clipboard')
        }
        clipboard.writeImage(img)
    })

    safeHandle('window:save-debug-logs', event => {
        const win = BrowserWindow.fromWebContents(event.sender)
        return saveDebugLogs(win)
    })
}
