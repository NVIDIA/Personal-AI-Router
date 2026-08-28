// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { spawnSync } from 'child_process'
import fs from 'fs'
import path from 'path'
import { describe, expect, it } from 'vitest'
import { currentPlatform } from '@/shared/utils/platform'

/**
 * The wipe inventory lives in repo-root shell scripts (no Node required).
 * These smoke checks keep the twins aligned on the critical roots.
 */
describe('repo-root wipe scripts', () => {
    const scriptsDir = path.resolve(process.cwd(), '../scripts')
    const sh = path.join(scriptsDir, 'wipe-app-data.sh')
    const ps1 = path.join(scriptsDir, 'wipe-app-data.ps1')
    const cmd = path.join(scriptsDir, 'wipe-app-data.cmd')

    // Under Git Bash / MSYS the unix script refuses to run at all and points at
    // the Windows twin, so only its static inventory is observable there.
    const itUnix = it.skipIf(currentPlatform() === 'win32')

    it('ships unix and windows entrypoints', () => {
        expect(fs.existsSync(sh), sh).toBe(true)
        expect(fs.existsSync(ps1), ps1).toBe(true)
        expect(fs.existsSync(cmd), cmd).toBe(true)
    })

    it('lists current and legacy app data roots in both inventories', () => {
        const shText = fs.readFileSync(sh, 'utf8')
        const psText = fs.readFileSync(ps1, 'utf8')
        for (const text of [shText, psText]) {
            expect(text).toContain('Nvidia Corporation')
            expect(text).toContain('Personal AI Router')
            expect(text).toContain('NVIDIA Corporation')
            expect(text).toContain('PAIR')
            expect(text).toContain('APPEND-ONLY')
            expect(text).toContain('nvpair-updater')
        }
    })

    it('accepts the app-invoked wait-pid and relaunch options in both twins', () => {
        const shText = fs.readFileSync(sh, 'utf8')
        const psText = fs.readFileSync(ps1, 'utf8')
        for (const text of [shText, psText]) {
            expect(text).toContain('--wait-pid=')
            expect(text).toContain('--relaunch=')
        }
    })

    itUnix('unix script dry-run exits 0 without deleting', () => {
        const result = spawnSync('bash', [sh, '--dry-run'], { encoding: 'utf8' })
        expect(result.status).toBe(0)
        expect(result.stdout).toContain('[dry-run]')
        expect(result.stdout).toContain('Personal AI Router')
    })

    itUnix('unix script aborts when the app process never exits', () => {
        const result = spawnSync(
            'bash',
            [sh, '--confirm', `--wait-pid=${process.pid}`, '--wait-timeout=1'],
            { encoding: 'utf8' }
        )
        expect(result.status).toBe(1)
        expect(result.stderr).toContain('did not exit')
    })
})
