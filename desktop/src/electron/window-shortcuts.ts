// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { BrowserWindow } from 'electron'

/**
 * Explicit reload + DevTools shortcuts for all windows. (The old
 * @electron-toolkit `watchWindowShortcuts` blocked these in production.)
 *
 * - Ctrl/Cmd+R — reload
 * - Ctrl+Shift+I — toggle DevTools (Windows / Linux)
 * - Cmd+Option+I — toggle DevTools (macOS)
 */
export function registerWindowShortcuts(window: BrowserWindow): void {
    const { webContents } = window

    webContents.on('before-input-event', (event, input) => {
        if (input.type !== 'keyDown') return

        if (input.code === 'KeyR' && (input.control || input.meta)) {
            event.preventDefault()
            webContents.reload()
            return
        }

        const devToolsShortcut =
            input.code === 'KeyI' && ((input.control && input.shift) || (input.meta && input.alt))

        if (devToolsShortcut) {
            event.preventDefault()
            if (webContents.isDevToolsOpened()) {
                webContents.closeDevTools()
            } else {
                webContents.openDevTools({ mode: 'detach' })
            }
        }
    })
}
