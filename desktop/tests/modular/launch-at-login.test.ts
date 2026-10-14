// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { App } from 'electron'
import type { currentPlatform } from '@/shared/utils/platform'

/**
 * The OS side of launching at login, checked against what actually consumes it:
 * the registered argument list and the startup decision that keeps the window
 * closed.
 */

const mocks = vi.hoisted(() => ({
    app: {
        isPackaged: true,
        setLoginItemSettings: vi.fn<App['setLoginItemSettings']>(),
        getLoginItemSettings: vi.fn<App['getLoginItemSettings']>()
    },
    currentPlatform: vi.fn<typeof currentPlatform>()
}))

vi.mock('electron', () => ({ app: mocks.app }))
vi.mock('@/shared/utils/platform', () => ({ currentPlatform: mocks.currentPlatform }))

import { APP_HIDDEN_ARGUMENT } from '@/shared/constants/app'
import {
    applyLoginItem,
    isLoginItemSupported,
    readLoginItem,
    shouldOpenOverviewOnStart
} from '@/electron/login-item'

describe('login item registration', () => {
    beforeEach(() => {
        mocks.currentPlatform.mockReturnValue('win32')
        mocks.app.isPackaged = true
    })

    it('registers the executable with the hidden argument', () => {
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith({
            openAtLogin: true,
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
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith({
            openAtLogin: true,
            args: []
        })
    })

    it('removes the entry when launch at login is turned off', () => {
        applyLoginItem({ launchAtLogin: false, startHidden: true })
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith(
            expect.objectContaining({ openAtLogin: false })
        )
    })

    it('registers macOS without the removed openAsHidden option', () => {
        mocks.currentPlatform.mockReturnValue('darwin')
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(isLoginItemSupported()).toBe(true)
        const [written] = mocks.app.setLoginItemSettings.mock.calls[0]
        expect(written).not.toHaveProperty('openAsHidden')
    })

    it('writes nothing from an unpackaged build', () => {
        // `process.execPath` there is the Electron binary under node_modules, so
        // registering it would start a bare Electron at the next sign-in.
        mocks.app.isPackaged = false
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(mocks.app.setLoginItemSettings).not.toHaveBeenCalled()
    })

    it('writes nothing on a platform Electron cannot register', () => {
        mocks.currentPlatform.mockReturnValue('linux')
        applyLoginItem({ launchAtLogin: true, startHidden: true })
        expect(isLoginItemSupported()).toBe(false)
        expect(mocks.app.setLoginItemSettings).not.toHaveBeenCalled()
    })

    it('reads nothing on a platform Electron cannot register', () => {
        mocks.currentPlatform.mockReturnValue('linux')
        expect(readLoginItem({ launchAtLogin: true, startHidden: true })).toBeNull()
        expect(mocks.app.getLoginItemSettings).not.toHaveBeenCalled()
    })

    it('reads the entry back under the arguments it registered', () => {
        // Windows reports openAtLogin per argument list, so reading with the
        // wrong one describes a registered login item as missing.
        readLoginItem({ launchAtLogin: true, startHidden: true })
        expect(mocks.app.getLoginItemSettings).toHaveBeenCalledWith({ args: ['--hidden'] })
    })

    it('reads the entry back with no arguments when it registered none', () => {
        readLoginItem({ launchAtLogin: true, startHidden: false })
        expect(mocks.app.getLoginItemSettings).toHaveBeenCalledWith({ args: [] })
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
