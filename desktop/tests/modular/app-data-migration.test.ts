// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import os from 'os'
import path from 'path'
import { afterEach, describe, expect, it } from 'vitest'
import { migrateAppDataDirectory, withAppDataMigrationLock } from '@/electron/app-data-migration'
import {
    APP_DATA_DIR_NAME,
    APP_DATA_MIGRATION_LOCK_NAME,
    APP_DATA_MIGRATION_SKIP_ENTRIES,
    APP_ORG,
    APP_PREVIOUS_DATA_DIRS
} from '@/shared/constants/app'

const roots: string[] = []

function createRoot(): string {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'nvpair-app-data-migration-'))
    roots.push(root)
    return root
}

afterEach(() => {
    for (const root of roots.splice(0)) {
        fs.rmSync(root, { recursive: true, force: true })
    }
})

describe('app data migration', () => {
    it('moves the previous directory when the destination does not exist', () => {
        const root = createRoot()
        const previousRoot = path.join(root, 'NVIDIA Corporation')
        const sharedRoot = path.join(root, 'Nvidia Corporation')
        const source = path.join(previousRoot, 'PAIR')
        const destination = path.join(sharedRoot, 'NVIDIA PAIR')
        fs.mkdirSync(path.join(source, 'logs'), { recursive: true })
        fs.writeFileSync(path.join(source, 'logs', 'app.log'), 'existing log')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'NVIDIA PAIR')

        expect(fs.existsSync(source)).toBe(false)
        expect(fs.readFileSync(path.join(destination, 'logs', 'app.log'), 'utf8')).toBe(
            'existing log'
        )
    })

    it('merges into backend data without overwriting destination files', () => {
        const root = createRoot()
        const previousRoot = path.join(root, 'NVIDIA Corporation')
        const sharedRoot = path.join(root, 'Nvidia Corporation')
        const source = path.join(previousRoot, 'PAIR')
        const destination = path.join(sharedRoot, 'NVIDIA PAIR')
        fs.mkdirSync(path.join(source, 'config'), { recursive: true })
        fs.mkdirSync(path.join(destination, 'config'), { recursive: true })
        fs.mkdirSync(path.join(destination, 'engine-bin'))
        fs.writeFileSync(path.join(source, 'config', 'ui.json'), 'previous')
        fs.writeFileSync(path.join(source, 'config', 'shared.json'), 'previous conflict')
        fs.writeFileSync(path.join(destination, 'config', 'shared.json'), 'current conflict')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'NVIDIA PAIR')

        expect(fs.readFileSync(path.join(destination, 'config', 'ui.json'), 'utf8')).toBe(
            'previous'
        )
        expect(fs.readFileSync(path.join(destination, 'config', 'shared.json'), 'utf8')).toBe(
            'current conflict'
        )
        expect(fs.readFileSync(path.join(source, 'config', 'shared.json'), 'utf8')).toBe(
            'previous conflict'
        )
        expect(fs.existsSync(path.join(destination, 'engine-bin'))).toBe(true)
    })

    it('creates the destination parent when it does not exist yet', () => {
        // Distinctly named parents so the case-insensitive Windows FS cannot
        // alias them: this exercises the mkdir on every platform.
        const root = createRoot()
        const previousRoot = path.join(root, 'old-vendor')
        const sharedRoot = path.join(root, 'new-vendor')
        const source = path.join(previousRoot, 'PAIR')
        const destination = path.join(sharedRoot, 'NVIDIA PAIR')
        fs.mkdirSync(path.join(source, 'logs'), { recursive: true })
        fs.writeFileSync(path.join(source, 'logs', 'app.log'), 'existing log')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'NVIDIA PAIR')

        expect(fs.existsSync(sharedRoot)).toBe(true)
        expect(fs.existsSync(source)).toBe(false)
        expect(fs.readFileSync(path.join(destination, 'logs', 'app.log'), 'utf8')).toBe(
            'existing log'
        )
    })

    it('leaves skipped entries in the previous directory', () => {
        const root = createRoot()
        const previousRoot = path.join(root, 'old-vendor')
        const sharedRoot = path.join(root, 'new-vendor')
        const source = path.join(previousRoot, 'PAIR')
        const destination = path.join(sharedRoot, 'NVIDIA PAIR')
        fs.mkdirSync(path.join(source, 'bin'), { recursive: true })
        fs.mkdirSync(path.join(source, 'config'))
        fs.writeFileSync(path.join(source, 'bin', 'nvpair.cmd'), 'launcher')
        fs.writeFileSync(path.join(source, 'config', 'ui.json'), 'previous')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'NVIDIA PAIR', ['bin'])

        expect(fs.readFileSync(path.join(destination, 'config', 'ui.json'), 'utf8')).toBe(
            'previous'
        )
        expect(fs.existsSync(path.join(destination, 'bin'))).toBe(false)
        // The launcher stays put so its baked-in PATH entry keeps working.
        expect(fs.readFileSync(path.join(source, 'bin', 'nvpair.cmd'), 'utf8')).toBe('launcher')
        // The previous directory survives because it still holds the skipped bin.
        expect(fs.existsSync(source)).toBe(true)
    })

    it('is a no-op after a successful migration', () => {
        const root = createRoot()
        const previousRoot = path.join(root, 'NVIDIA Corporation')
        const sharedRoot = path.join(root, 'Nvidia Corporation')
        const source = path.join(previousRoot, 'PAIR')
        const destinationFile = path.join(sharedRoot, 'NVIDIA PAIR', 'state.json')
        fs.mkdirSync(source, { recursive: true })
        fs.writeFileSync(path.join(source, 'state.json'), 'state')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'NVIDIA PAIR')
        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'NVIDIA PAIR')

        expect(fs.readFileSync(destinationFile, 'utf8')).toBe('state')
        expect(fs.existsSync(source)).toBe(false)
    })

    it('does nothing when the previous and current paths are one directory', () => {
        const root = createRoot()
        const directory = path.join(root, 'vendor', 'NVIDIA PAIR')
        fs.mkdirSync(directory, { recursive: true })
        fs.writeFileSync(path.join(directory, 'state.json'), 'state')

        migrateAppDataDirectory(
            path.join(root, 'vendor'),
            'NVIDIA PAIR',
            path.join(root, 'vendor', '.'),
            'NVIDIA PAIR'
        )

        expect(fs.readFileSync(path.join(directory, 'state.json'), 'utf8')).toBe('state')
    })
})

describe('app data migration lock', () => {
    it('runs the migration and releases the lock', () => {
        const lockPath = path.join(createRoot(), 'vendor', APP_DATA_MIGRATION_LOCK_NAME)
        let ran = false

        const acquired = withAppDataMigrationLock(lockPath, () => {
            ran = fs.existsSync(lockPath)
        })

        expect(acquired).toBe(true)
        expect(ran).toBe(true)
        expect(fs.existsSync(lockPath)).toBe(false)
    })

    it('breaks a lock left by a crashed holder', () => {
        const lockPath = path.join(createRoot(), APP_DATA_MIGRATION_LOCK_NAME)
        fs.writeFileSync(lockPath, '')
        const stale = new Date(Date.now() - 5 * 60_000)
        fs.utimesSync(lockPath, stale, stale)
        let ran = false

        const acquired = withAppDataMigrationLock(lockPath, () => {
            ran = true
        })

        expect(acquired).toBe(true)
        expect(ran).toBe(true)
    })
})

describe('app data directory constants', () => {
    // The broker migrates the same directories under the same lock for launches
    // that never start Electron; a drift would split one node's state in two.
    const appdirSource = fs.readFileSync(
        path.join(__dirname, '..', '..', '..', 'services', 'shared', 'appdir', 'appdir.go'),
        'utf8'
    )

    function goStringConstant(name: string): string | undefined {
        return new RegExp(`\\b${name}\\s*=\\s*"([^"]*)"`).exec(appdirSource)?.[1]
    }

    it('matches the Go data directory and lock file names', () => {
        expect(goStringConstant('orgDir')).toBe(APP_ORG)
        expect(goStringConstant('appDir')).toBe(APP_DATA_DIR_NAME)
        expect(goStringConstant('migrationLockName')).toBe(APP_DATA_MIGRATION_LOCK_NAME)
    })

    it('matches the Go previous directories in order', () => {
        const block = /previousDirs = \[\]\[2\]string\{([\s\S]*?)\n\}/.exec(appdirSource)?.[1] ?? ''
        const goPrevious = [...block.matchAll(/\{"([^"]*)", "([^"]*)"\}/g)].map(match => ({
            org: match[1],
            name: match[2]
        }))

        expect(goPrevious).toEqual(APP_PREVIOUS_DATA_DIRS)
    })

    it('matches the Go preserved launcher entries', () => {
        const block = /preservedEntries = \[\]string\{([^}]*)\}/.exec(appdirSource)?.[1] ?? ''
        const goPreserved = [...block.matchAll(/"([^"]*)"/g)].map(match => match[1])

        expect(goPreserved).toEqual(APP_DATA_MIGRATION_SKIP_ENTRIES)
    })
})
