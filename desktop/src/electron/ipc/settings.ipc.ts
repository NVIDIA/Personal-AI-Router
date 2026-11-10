// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { safeHandle } from '@/electron/ipc/safe-handle'
import {
    completeFirstRun,
    isFirstRun,
    getHideDockIconInBackground,
    setHideDockIconInBackground
} from '@/electron/config/ui-config'
import { refreshDockVisibility } from '@/electron/window'
import { currentPlatform } from '@/shared/utils/platform'

export function registerSettingsIpc(): void {
    safeHandle('settings:get-hide-dock-icon', () => getHideDockIconInBackground())
    safeHandle('settings:set-hide-dock-icon', (_event, { enabled }) => {
        if (currentPlatform() !== 'darwin' || typeof enabled !== 'boolean') {
            throw new Error('Dock visibility requires macOS and a boolean preference')
        }
        setHideDockIconInBackground(enabled)
        refreshDockVisibility()
    })
    safeHandle('settings:is-first-run', () => isFirstRun())
    safeHandle('settings:complete-first-run', () => completeFirstRun())
}
