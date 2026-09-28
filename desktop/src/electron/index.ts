// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { initPlatform } from '@/electron/globals'
import {
    ElectronPathProvider,
    logAppDataMigration,
    migrateAppData,
    setPaths
} from '@/electron/path'
import { initFileLogger } from '@/shared/utils/log'
import { APP_DISPLAY_NAME, APP_EXIT_ARGUMENT } from '@/shared/constants/app'

const init = async (): Promise<void> => {
    try {
        app.setName(APP_DISPLAY_NAME)
        await setPaths()

        // Taken before migrating so only the instance that will run migrates:
        // an Exit or focus launch hands off to the running app instead, and
        // Chromium's per-run Singleton* files already exist here, so a crashed
        // session's stale copies collide and stay behind instead of moving in.
        const primary = app.requestSingleInstanceLock()
        const migration =
            primary && !process.argv.includes(APP_EXIT_ARGUMENT) ? migrateAppData() : null

        const paths = new ElectronPathProvider(app)
        initPlatform(paths)
        initFileLogger(paths)
        if (migration) logAppDataMigration(migration)

        await import('./main')
    } catch (error) {
        console.error('Error initializing app:', error)
        process.exit(1)
    }
}

init()
