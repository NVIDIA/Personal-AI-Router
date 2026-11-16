// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { beforeEach, describe, expect, it } from 'vitest'
import { getStartupSettings, loadUiConfig, setStartupSettings } from '@/electron/config/ui-config'
import { initPlatform } from '@/electron/globals'
import { assertIsolated } from '../fixtures/isolation'
import { createTmpUserData } from '../fixtures/tmpdir'

/**
 * What `ui-config.json` can hold on disk. The startup keys are wider than their
 * runtime type on purpose: a hand-edited file can carry any JSON scalar there.
 */
interface UiConfigFile {
    firstRun?: boolean
    modularLogLevel?: string
    launchAtLogin?: boolean | string | null
    startHidden?: boolean | string | null
}

const DEFAULT_STARTUP = { launchAtLogin: false, startHidden: true }

describe('startup preferences in ui-config', () => {
    const tmp = createTmpUserData()
    const configFile = (): string => path.join(tmp.dir, 'configs', 'ui-config.json')

    /**
     * Stand in for a launch whose config file holds `contents`.
     *
     * `loadUiConfig()` replaces the in-process config wholesale when a file
     * exists, so writing a file and loading it is also how every case starts from
     * a known state without re-importing the module.
     */
    function launchWith(contents: UiConfigFile): void {
        fs.mkdirSync(path.dirname(configFile()), { recursive: true })
        fs.writeFileSync(configFile(), JSON.stringify(contents), 'utf8')
        loadUiConfig()
    }

    function readConfigFile(): UiConfigFile {
        const parsed: UiConfigFile = JSON.parse(fs.readFileSync(configFile(), 'utf8'))
        return parsed
    }

    beforeEach(() => {
        // This suite writes a real ui-config.json, so it has to resolve to a
        // tmpdir even when a developer has set PAIR_USER_DATA.
        assertIsolated()
        fs.rmSync(tmp.dir, { recursive: true, force: true })
        fs.mkdirSync(tmp.dir, { recursive: true })
        initPlatform({
            getUserData: () => tmp.dir,
            getTemp: () => tmp.dir,
            getResourcesPath: () => process.cwd()
        })
    })

    it('leaves the login item off and the hidden start on for a config without the keys', () => {
        // Off by default is the install story: shipping this must not register a
        // login item on a machine whose owner never asked for one.
        launchWith({ firstRun: false, modularLogLevel: 'info' })
        expect(getStartupSettings()).toEqual(DEFAULT_STARTUP)
    })

    it('writes both keys to ui-config.json', () => {
        launchWith({ firstRun: false })
        setStartupSettings({ launchAtLogin: true, startHidden: false })

        const saved = readConfigFile()
        expect(saved.launchAtLogin).toBe(true)
        expect(saved.startHidden).toBe(false)
    })

    it('reads both keys back on the next launch', () => {
        launchWith({ firstRun: false, launchAtLogin: true, startHidden: false })
        expect(getStartupSettings()).toEqual({ launchAtLogin: true, startHidden: false })
    })

    it('keeps the stored hidden start when only launch at login is sent', () => {
        launchWith({ firstRun: false, startHidden: false })
        const written = setStartupSettings({ launchAtLogin: true })
        expect(written).toEqual({ launchAtLogin: true, startHidden: false })
    })

    it('keeps the stored launch at login when only the hidden start is sent', () => {
        launchWith({ firstRun: false, launchAtLogin: true })
        const written = setStartupSettings({ startHidden: false })
        expect(written).toEqual({ launchAtLogin: true, startHidden: false })
    })

    it('rejects a truthy string for launch at login', () => {
        // A truthy string in the file would register a login item on the next
        // launch if it were coerced instead of rejected.
        launchWith({ firstRun: false, launchAtLogin: 'yes' })
        expect(getStartupSettings().launchAtLogin).toBe(false)
    })

    it('falls back to the defaults for a hand-edited config', () => {
        launchWith({ firstRun: false, launchAtLogin: 'true', startHidden: null })
        expect(getStartupSettings()).toEqual(DEFAULT_STARTUP)
    })
})
