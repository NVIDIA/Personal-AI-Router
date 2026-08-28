// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { initPlatform } from '@/electron/globals'
import { ElectronPathProvider, setPaths } from '@/electron/path'
import { initFileLogger } from '@/shared/utils/log'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'

const init = async (): Promise<void> => {
    try {
        app.setName(APP_DISPLAY_NAME)
        await setPaths()

        const paths = new ElectronPathProvider(app)
        initPlatform(paths)
        initFileLogger(paths)

        await import('./main')
    } catch (error) {
        console.error('Error initializing app:', error)
        process.exit(1)
    }
}

init()
