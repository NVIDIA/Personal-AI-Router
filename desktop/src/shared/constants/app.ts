// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

interface AppDataDirectory {
    readonly org: string
    readonly name: string
}

export const APP_DISPLAY_NAME = 'NVIDIA PAIR'
export const APP_EXECUTABLE_NAME = 'PAIR'
export const APP_ID = 'com.nvidia.nvpair'
export const APP_ORG = 'Nvidia Corporation'
export const APP_EXIT_ARGUMENT = '--exit-application'

// The data directory, earlier locations, launcher entry, and lock file must match
// services/shared/appdir/appdir.go: the broker migrates the same directories
// under the same lock for launches that never start Electron.
export const APP_DATA_DIR_NAME = 'NVIDIA PAIR'
/** Earlier data locations, newest first, so the newer copy wins a conflict. */
export const APP_PREVIOUS_DATA_DIRS: readonly AppDataDirectory[] = [
    { org: 'Nvidia Corporation', name: 'Personal AI Router' },
    { org: 'NVIDIA Corporation', name: 'PAIR' }
]
/**
 * The generated `nvpair` launcher directory (see `src/electron/nvpair-command.ts`)
 * is referenced by absolute path from the user's PATH on Windows, so it stays in
 * its previous directory; the app regenerates it in the current one.
 */
export const APP_DATA_MIGRATION_SKIP_ENTRIES: readonly string[] = ['bin']
export const APP_DATA_MIGRATION_LOCK_NAME = '.nvpair-data-migration.lock'
