// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { spawnSync } from 'child_process'
import fs from 'fs'
import path from 'path'
import os from 'os'
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

    it.skipIf(currentPlatform() !== 'win32')(
        'Windows reset removes app data but retains the sibling model library',
        () => {
            const isolated = fs.mkdtempSync(path.join(os.tmpdir(), 'pair-model-retention-'))
            const config = path.join(isolated, 'config')
            const app = path.join(config, 'Nvidia Corporation', 'Personal AI Router')
            const model = path.join(
                config,
                'Nvidia Corporation',
                'Personal AI Router Models',
                'llamacpp',
                'retained.gguf'
            )
            fs.mkdirSync(app, { recursive: true })
            fs.mkdirSync(path.dirname(model), { recursive: true })
            fs.writeFileSync(path.join(app, 'settings.json'), '{}')
            fs.writeFileSync(model, 'retained-model')
            try {
                // Only process enumeration is stubbed: never stop a real application.
                // The exact script still performs its real filesystem removal.
                const quotedScript = ps1.replaceAll("'", "''")
                const result = spawnSync(
                    'powershell.exe',
                    [
                        '-NoProfile',
                        '-Command',
                        `function tasklist {}; & '${quotedScript}' --confirm`
                    ],
                    {
                        encoding: 'utf8',
                        env: {
                            ...process.env,
                            USERPROFILE: isolated,
                            LOCALAPPDATA: config,
                            TEMP: isolated,
                            TMP: isolated
                        }
                    }
                )
                expect(result.status).toBe(0)
                expect(fs.existsSync(app)).toBe(false)
                expect(fs.readFileSync(model, 'utf8')).toBe('retained-model')
            } finally {
                fs.rmSync(isolated, { recursive: true, force: true })
            }
        }
    )

    it('refuses reset before deleting unmigrated llama models', () => {
        const isolated = fs.mkdtempSync(path.join(os.tmpdir(), 'pair-legacy-models-'))
        const config =
            currentPlatform() === 'darwin'
                ? path.join(isolated, 'Library', 'Application Support')
                : path.join(isolated, 'config')
        const model = path.join(
            config,
            'Nvidia Corporation',
            'Personal AI Router',
            'engine-bin',
            'llamacpp',
            'models',
            'retained.gguf'
        )
        fs.mkdirSync(path.dirname(model), { recursive: true })
        fs.writeFileSync(model, 'retained-model')
        try {
            const windows = currentPlatform() === 'win32'
            const result = spawnSync(
                windows ? 'powershell.exe' : 'bash',
                windows ? ['-NoProfile', '-File', ps1, '--confirm'] : [sh, '--confirm'],
                {
                    encoding: 'utf8',
                    env: {
                        ...process.env,
                        HOME: isolated,
                        USERPROFILE: isolated,
                        LOCALAPPDATA: config,
                        XDG_CONFIG_HOME: config
                    }
                }
            )
            expect(result.status).toBe(1)
            expect(result.stdout + result.stderr).toContain('Open the updated app to migrate')
            expect(fs.readFileSync(model, 'utf8')).toBe('retained-model')
        } finally {
            fs.rmSync(isolated, { recursive: true, force: true })
        }
    })

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
