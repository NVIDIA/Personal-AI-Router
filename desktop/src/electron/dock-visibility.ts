// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { getHideDockIconInBackground } from '@/electron/config/ui-config'
import { currentPlatform } from '@/shared/utils/platform'
import { createStructuredLogger } from '@/shared/utils/log'

const log = createStructuredLogger('dock')

// Electron ignores dock.hide() for one second after dock.show().
const HIDE_INTERVAL_MS = 1100

export function createDockVisibilityController(isOverviewVisible: () => boolean): () => void {
    let lastShownAt = Number.NEGATIVE_INFINITY
    let hideTimer: ReturnType<typeof setTimeout> | undefined
    let showing = false

    const refresh = (): void => {
        if (currentPlatform() !== 'darwin') return
        const dock = app.dock
        if (!dock) return
        clearTimeout(hideTimer)
        hideTimer = undefined

        if (!getHideDockIconInBackground() || isOverviewVisible()) {
            if (showing || dock.isVisible()) return
            showing = true
            lastShownAt = Date.now()
            void dock.show().then(
                () => {
                    showing = false
                    refresh()
                },
                error => {
                    showing = false
                    log.error({
                        sublevel: 'visibility',
                        message: `Could not restore Dock icon: ${String(error)}`
                    })
                }
            )
            return
        }

        if (showing || !dock.isVisible()) return
        const delay = HIDE_INTERVAL_MS - (Date.now() - lastShownAt)
        if (delay > 0) {
            hideTimer = setTimeout(refresh, delay)
            return
        }
        dock.hide()
    }

    return refresh
}
