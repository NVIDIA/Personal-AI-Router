// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app, Menu } from 'electron'
import { APP_DISPLAY_NAME, APP_EXIT_ARGUMENT } from '@/shared/constants/app'
import { currentPlatform } from '@/shared/utils/platform'

function getWindowsTaskArguments(): string {
    if (app.isPackaged) return APP_EXIT_ARGUMENT
    return `"${app.getAppPath()}" ${APP_EXIT_ARGUMENT}`
}

export function registerApplicationIconMenu(): void {
    const platform = currentPlatform()

    if (platform === 'darwin') {
        const menu = Menu.buildFromTemplate([
            {
                label: `Exit ${APP_DISPLAY_NAME}`,
                click: () => app.quit()
            }
        ])
        app.dock?.setMenu(menu)
        return
    }

    if (platform === 'win32') {
        const executablePath = app.getPath('exe')
        app.setUserTasks([
            {
                program: executablePath,
                arguments: getWindowsTaskArguments(),
                iconPath: executablePath,
                iconIndex: 0,
                title: `Exit ${APP_DISPLAY_NAME}`,
                description: `Exit ${APP_DISPLAY_NAME}`
            }
        ])
    }
}
