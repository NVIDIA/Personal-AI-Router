// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'
import { beforeEach, describe, expect, it, vi } from 'vitest'

/**
 * Launching at login is one preference written to three places: the persisted
 * config, the OS login item, and the startup decision that keeps the window
 * closed. Each is checked against what actually consumes it — the registered
 * argument string, not a paraphrase of it.
 */

const electronMock = vi.hoisted(() => ({
    isPackaged: true,
    setLoginItemSettings: vi.fn(),
    getLoginItemSettings: vi.fn(() => ({ openAtLogin: true }))
}))

const platformMock = vi.hoisted(() => ({ current: 'win32' as 'win32' | 'darwin' | 'linux' }))

vi.mock('electron', () => ({ app: electronMock }))

vi.mock('@/shared/utils/platform', async importOriginal => ({
    ...(await importOriginal<typeof import('@/shared/utils/platform')>()),
    currentPlatform: () => platformMock.current
}))

import { APP_HIDDEN_ARGUMENT } from '@/shared/constants/app'
import {
    applyLoginItem,
    isLoginItemSupported,
    readLoginItem,
    shouldOpenOverviewOnStart
} from '@/electron/login-item'
import { initPlatform } from '@/electron/globals'
import { assertIsolated } from '../fixtures/isolation'
import { createTmpUserData } from '../fixtures/tmpdir'

type UiConfigModule = typeof import('@/electron/config/ui-config')

describe('startup preferences in ui-config', () => {
    const tmp = createTmpUserData()
    const configFile = (): string => path.join(tmp.dir, 'configs', 'ui-config.json')

    /**
     * A freshly imported ui-config, standing in for a freshly launched app.
     *
     * `loadUiConfig()` leaves the in-process config alone when there is no file
     * to read — harmless in production, where it runs once per launch — so
     * re-importing the module is the only way to observe a first launch, or a
     * restart, more than once in a suite.
     */
    async function freshUiConfig(): Promise<UiConfigModule> {
        vi.resetModules()
        const mod = await import('@/electron/config/ui-config')
        mod.loadUiConfig()
        return mod
    }

    function writeConfigFile(contents: Record<string, unknown>): void {
        fs.mkdirSync(path.dirname(configFile()), { recursive: true })
        fs.writeFileSync(configFile(), JSON.stringify(contents), 'utf8')
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
            getResourcesPath: () => process.cwd(),
            getAppName: () => 'Personal AI Router'
        })
    })

    it('leaves the login item off and the hidden start on by default', async () => {
        // Off by default is the install story: shipping this must not register a
        // login item on a machine whose owner never asked for one.
        const cfg = await freshUiConfig()
        expect(cfg.getStartupSettings()).toEqual({ launchAtLogin: false, startHidden: true })
    })

    it('persists both keys across a restart', async () => {
        const cfg = await freshUiConfig()
        cfg.setStartupSettings({ launchAtLogin: true, startHidden: false })

        const restarted = await freshUiConfig()
        expect(restarted.getStartupSettings()).toEqual({
            launchAtLogin: true,
            startHidden: false
        })
        expect(JSON.parse(fs.readFileSync(configFile(), 'utf8'))).toMatchObject({
            launchAtLogin: true,
            startHidden: false
        })
    })

    it('keeps the untouched key when only one is sent', async () => {
        const cfg = await freshUiConfig()
        cfg.setStartupSettings({ launchAtLogin: true })
        expect(cfg.getStartupSettings()).toEqual({ launchAtLogin: true, startHidden: true })

        cfg.setStartupSettings({ startHidden: false })
        const restarted = await freshUiConfig()
        expect(restarted.getStartupSettings()).toEqual({
            launchAtLogin: true,
            startHidden: false
        })
    })

    it('ignores a value that is not a boolean', async () => {
        // The payload crosses IPC from the renderer, and a truthy string stored
        // here would register a login item on the next launch.
        const cfg = await freshUiConfig()
        cfg.setStartupSettings({ launchAtLogin: 'yes' as unknown as boolean })
        expect(cfg.getStartupSettings().launchAtLogin).toBe(false)
    })

    it('falls back to the defaults for a hand-edited config', async () => {
        writeConfigFile({ firstRun: false, launchAtLogin: 'true', startHidden: null })
        const cfg = await freshUiConfig()
        expect(cfg.getStartupSettings()).toEqual({ launchAtLogin: false, startHidden: true })
    })

    it('reads a config written before the keys existed as login item off', async () => {
        writeConfigFile({ firstRun: false, modularLogLevel: 'info' })
        const cfg = await freshUiConfig()
        expect(cfg.getStartupSettings()).toEqual({ launchAtLogin: false, startHidden: true })
    })
})

describe('login item registration', () => {
    beforeEach(() => {
        platformMock.current = 'win32'
        electronMock.isPackaged = true
    })

    it('registers the executable with the hidden argument', () => {
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(electronMock.setLoginItemSettings).toHaveBeenCalledWith({
            openAtLogin: true,
            openAsHidden: true,
            args: ['--hidden']
        })
    })

    it('pins the argument string', () => {
        // The flag is written into the Windows Run entry, so renaming it orphans
        // every entry a shipped build already registered.
        expect(APP_HIDDEN_ARGUMENT).toBe('--hidden')
    })

    it('registers no argument when the window should open at login', () => {
        applyLoginItem({ launchAtLogin: true, startHidden: false })
        expect(electronMock.setLoginItemSettings).toHaveBeenCalledWith({
            openAtLogin: true,
            openAsHidden: false,
            args: []
        })
    })

    it('removes the entry when launch at login is turned off', () => {
        applyLoginItem({ launchAtLogin: false, startHidden: true })
        expect(electronMock.setLoginItemSettings).toHaveBeenCalledWith(
            expect.objectContaining({ openAtLogin: false })
        )
    })

    it('asks macOS to open hidden, where a login item carries no arguments', () => {
        platformMock.current = 'darwin'
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(isLoginItemSupported()).toBe(true)
        expect(electronMock.setLoginItemSettings).toHaveBeenCalledWith(
            expect.objectContaining({ openAsHidden: true })
        )
    })

    it('writes nothing from an unpackaged build', () => {
        // `process.execPath` there is the Electron binary under node_modules, so
        // registering it would start a bare Electron at the next sign-in.
        electronMock.isPackaged = false
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(electronMock.setLoginItemSettings).not.toHaveBeenCalled()
    })

    it('touches nothing on a platform Electron cannot register', () => {
        platformMock.current = 'linux'
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(isLoginItemSupported()).toBe(false)
        expect(electronMock.setLoginItemSettings).not.toHaveBeenCalled()
        expect(readLoginItem({ launchAtLogin: true, startHidden: true })).toBeNull()
        expect(electronMock.getLoginItemSettings).not.toHaveBeenCalled()
    })

    it('reads the entry back under the arguments it registered', () => {
        // Windows reports openAtLogin per argument list, so reading with the
        // wrong one describes a registered login item as missing.
        readLoginItem({ launchAtLogin: true, startHidden: true })
        expect(electronMock.getLoginItemSettings).toHaveBeenCalledWith({ args: ['--hidden'] })

        readLoginItem({ launchAtLogin: true, startHidden: false })
        expect(electronMock.getLoginItemSettings).toHaveBeenLastCalledWith({ args: [] })
    })
})

describe('overview window on startup', () => {
    it('opens the window for a launch by hand', () => {
        expect(shouldOpenOverviewOnStart(['/path/to/app'])).toBe(true)
    })

    it('stays in the tray for a launch by the login item', () => {
        expect(shouldOpenOverviewOnStart(['/path/to/app', APP_HIDDEN_ARGUMENT])).toBe(false)
    })

    it('is not fooled by an unrelated switch', () => {
        expect(shouldOpenOverviewOnStart(['/path/to/app', '--enable-logging'])).toBe(true)
    })
})
