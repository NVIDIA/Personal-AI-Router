// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import os from 'os'
import path from 'path'
import { afterEach, describe, expect, it } from 'vitest'
import { migrateAppDataDirectory } from '@/electron/app-data-migration'

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
        const destination = path.join(sharedRoot, 'Personal AI Router')
        fs.mkdirSync(path.join(source, 'logs'), { recursive: true })
        fs.writeFileSync(path.join(source, 'logs', 'app.log'), 'existing log')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'Personal AI Router')

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
        const destination = path.join(sharedRoot, 'Personal AI Router')
        fs.mkdirSync(path.join(source, 'config'), { recursive: true })
        fs.mkdirSync(path.join(destination, 'config'), { recursive: true })
        fs.mkdirSync(path.join(destination, 'engine-bin'))
        fs.writeFileSync(path.join(source, 'config', 'ui.json'), 'previous')
        fs.writeFileSync(path.join(source, 'config', 'shared.json'), 'previous conflict')
        fs.writeFileSync(path.join(destination, 'config', 'shared.json'), 'current conflict')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'Personal AI Router')

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
        const destination = path.join(sharedRoot, 'Personal AI Router')
        fs.mkdirSync(path.join(source, 'logs'), { recursive: true })
        fs.writeFileSync(path.join(source, 'logs', 'app.log'), 'existing log')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'Personal AI Router')

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
        const destination = path.join(sharedRoot, 'Personal AI Router')
        fs.mkdirSync(path.join(source, 'bin'), { recursive: true })
        fs.mkdirSync(path.join(source, 'config'))
        fs.writeFileSync(path.join(source, 'bin', 'nvpair.cmd'), 'launcher')
        fs.writeFileSync(path.join(source, 'config', 'ui.json'), 'previous')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'Personal AI Router', ['bin'])

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
        const destinationFile = path.join(sharedRoot, 'Personal AI Router', 'state.json')
        fs.mkdirSync(source, { recursive: true })
        fs.writeFileSync(path.join(source, 'state.json'), 'state')

        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'Personal AI Router')
        migrateAppDataDirectory(previousRoot, 'PAIR', sharedRoot, 'Personal AI Router')

        expect(fs.readFileSync(destinationFile, 'utf8')).toBe('state')
        expect(fs.existsSync(source)).toBe(false)
    })
})
