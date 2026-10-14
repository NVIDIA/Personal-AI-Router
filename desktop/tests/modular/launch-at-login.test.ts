// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { App, LaunchItems, LoginItemSettings } from 'electron'
import type { currentPlatform } from '@/shared/utils/platform'

/**
 * The OS side of launching at login, checked against what actually consumes it:
 * the registered argument list, the entry the OS reports back, and the startup
 * decision that keeps the window closed.
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

import { APP_HIDDEN_ARGUMENT, APP_ID } from '@/shared/constants/app'
import {
    isLoginItemSupported,
    planLoginItem,
    reconcileLoginItem,
    shouldStartHidden,
    wasOpenedAtLogin
} from '@/electron/login-item'

/** What the OS reports for an app with no login item, with `overrides` on top. */
function osState(overrides: Partial<LoginItemSettings> = {}): LoginItemSettings {
    return {
        openAtLogin: false,
        openAsHidden: false,
        wasOpenedAtLogin: false,
        wasOpenedAsHidden: false,
        restoreState: false,
        status: 'not-registered',
        executableWillLaunchAtLogin: false,
        launchItems: [],
        ...overrides
    }
}

const WINDOWS_EXE = 'C:\\Program Files\\PAIR\\PAIR.exe'
const MOVED_EXE = 'C:\\Old Location\\PAIR.exe'
const MAC_EXE = '/Applications/PAIR.app/Contents/MacOS/PAIR'

/** This app's per-user Run entry as registered with the hidden argument; a case overrides what it changes. */
function runEntry(overrides: Partial<LaunchItems> = {}): LaunchItems {
    return {
        name: APP_ID,
        path: WINDOWS_EXE,
        args: [APP_HIDDEN_ARGUMENT],
        scope: 'user',
        enabled: true,
        ...overrides
    }
}

const ON_HIDDEN = { launchAtLogin: true, startHidden: true }
const ON_VISIBLE = { launchAtLogin: true, startHidden: false }
const OFF = { launchAtLogin: false, startHidden: true }

describe('planning the login item on Windows', () => {
    type Settings = Parameters<typeof planLoginItem>[0]['settings']

    const plan = (settings: Settings, current: Partial<LoginItemSettings>) =>
        planLoginItem({
            settings,
            current: osState(current),
            platform: 'win32',
            execPath: WINDOWS_EXE
        })

    describe('with launch at login on', () => {
        it('keeps an entry registered with this executable and these arguments', () => {
            const current = { openAtLogin: true, launchItems: [runEntry()] }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'keep' })
        })

        it('keeps an entry the user disabled in Task Manager', () => {
            // Writing it again would clear the disabled flag. That is the user's
            // choice, and a launch must not undo it.
            const current = {
                openAtLogin: true,
                executableWillLaunchAtLogin: false,
                launchItems: [runEntry({ enabled: false })]
            }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'keep' })
        })

        it('keeps a disabled entry even when openAtLogin reads false for it', () => {
            // The documentation does not say whether a disabled entry reports
            // openAtLogin, so the Run entries decide.
            const current = { openAtLogin: false, launchItems: [runEntry({ enabled: false })] }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'keep' })
        })

        it('keeps an entry registered with no arguments when the window should open', () => {
            const current = { openAtLogin: true, launchItems: [runEntry({ args: [] })] }
            expect(plan(ON_VISIBLE, current)).toEqual({ kind: 'keep' })
        })

        it('matches the executable path regardless of case and quoting', () => {
            const current = {
                openAtLogin: false,
                launchItems: [runEntry({ path: '"c:\\program files\\pair\\pair.exe"' })]
            }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'keep' })
        })

        it('registers when there is no entry', () => {
            expect(plan(ON_HIDDEN, {})).toEqual({ kind: 'register', enabled: true })
        })

        it('replaces an entry registered with other arguments', () => {
            const current = { openAtLogin: false, launchItems: [runEntry({ args: [] })] }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'register', enabled: true })
        })

        it('replaces a stale entry without re-enabling it when the user disabled it', () => {
            const current = {
                openAtLogin: false,
                launchItems: [runEntry({ args: [], enabled: false })]
            }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'register', enabled: false })
        })

        it('replaces an entry for an executable an update moved', () => {
            const current = { openAtLogin: false, launchItems: [runEntry({ path: MOVED_EXE })] }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'register', enabled: true })
        })

        it('replaces a moved entry without re-enabling it when the user disabled it', () => {
            const current = {
                openAtLogin: false,
                launchItems: [runEntry({ path: MOVED_EXE, enabled: false })]
            }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'register', enabled: false })
        })

        it('registers a per-user entry beside a machine-wide one', () => {
            const current = { openAtLogin: false, launchItems: [runEntry({ scope: 'machine' })] }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'register', enabled: true })
        })

        it('ignores a disabled entry that belongs to another app', () => {
            const current = {
                openAtLogin: false,
                launchItems: [
                    runEntry({ name: 'other-app', path: 'C:\\Other\\Other.exe', enabled: false })
                ]
            }
            expect(plan(ON_HIDDEN, current)).toEqual({ kind: 'register', enabled: true })
        })
    })

    describe('with launch at login off', () => {
        it('removes an entry registered with these arguments', () => {
            const current = { openAtLogin: true, launchItems: [runEntry()] }
            expect(plan(OFF, current)).toEqual({ kind: 'remove' })
        })

        it('removes an entry registered with other arguments', () => {
            const current = { openAtLogin: false, launchItems: [runEntry({ args: [] })] }
            expect(plan(OFF, current)).toEqual({ kind: 'remove' })
        })

        it('removes an entry the user had disabled', () => {
            const current = { openAtLogin: false, launchItems: [runEntry({ enabled: false })] }
            expect(plan(OFF, current)).toEqual({ kind: 'remove' })
        })

        it('keeps nothing registered when there is no entry', () => {
            expect(plan(OFF, {})).toEqual({ kind: 'keep' })
        })

        it("leaves another app's entry alone", () => {
            const current = {
                openAtLogin: false,
                launchItems: [runEntry({ name: 'other-app', path: 'C:\\Other\\Other.exe' })]
            }
            expect(plan(OFF, current)).toEqual({ kind: 'keep' })
        })
    })
})

describe('planning the login item on macOS', () => {
    type Settings = Parameters<typeof planLoginItem>[0]['settings']

    const plan = (settings: Settings, current: Partial<LoginItemSettings>) =>
        planLoginItem({
            settings,
            current: osState(current),
            platform: 'darwin',
            execPath: MAC_EXE
        })

    it('keeps an enabled login item', () => {
        expect(plan(ON_HIDDEN, { openAtLogin: true, status: 'enabled' })).toEqual({
            kind: 'keep'
        })
    })

    it('keeps a login item waiting for approval in System Settings', () => {
        // Registering again does not approve it, so there is nothing to write.
        expect(plan(ON_HIDDEN, { status: 'requires-approval' })).toEqual({ kind: 'keep' })
    })

    it('registers when there is no login item', () => {
        expect(plan(ON_HIDDEN, { status: 'not-registered' })).toEqual({
            kind: 'register',
            enabled: true
        })
    })

    it('removes an enabled login item when launch at login is off', () => {
        expect(plan(OFF, { openAtLogin: true, status: 'enabled' })).toEqual({ kind: 'remove' })
    })

    it('removes a login item waiting for approval when launch at login is off', () => {
        expect(plan(OFF, { status: 'requires-approval' })).toEqual({ kind: 'remove' })
    })

    it('keeps nothing registered when there is no login item', () => {
        expect(plan(OFF, { status: 'not-registered' })).toEqual({ kind: 'keep' })
    })
})

describe('reconciling the login item', () => {
    beforeEach(() => {
        mocks.currentPlatform.mockReturnValue('win32')
        mocks.app.isPackaged = true
        mocks.app.getLoginItemSettings.mockReturnValue(osState())
    })

    it('registers the executable with the hidden argument', () => {
        expect(reconcileLoginItem(ON_HIDDEN)).toBe('register')
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith({
            openAtLogin: true,
            args: ['--hidden'],
            enabled: true
        })
    })

    it('pins the argument string', () => {
        // The flag is written into the Windows Run entry, so renaming it orphans
        // every entry a shipped build already registered.
        expect(APP_HIDDEN_ARGUMENT).toBe('--hidden')
    })

    it('registers no argument when the window should open at login', () => {
        reconcileLoginItem(ON_VISIBLE)
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith({
            openAtLogin: true,
            args: [],
            enabled: true
        })
    })

    it('registers a replaced entry as disabled when the user had disabled it', () => {
        mocks.app.getLoginItemSettings.mockReturnValue(
            osState({ launchItems: [runEntry({ args: [], enabled: false })] })
        )
        reconcileLoginItem(ON_HIDDEN)
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith(
            expect.objectContaining({ openAtLogin: true, enabled: false })
        )
    })

    it('removes the entry when launch at login is off', () => {
        mocks.app.getLoginItemSettings.mockReturnValue(
            osState({ openAtLogin: true, launchItems: [runEntry()] })
        )
        expect(reconcileLoginItem(OFF)).toBe('remove')
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith({ openAtLogin: false })
    })

    it('writes nothing when the entry already matches', () => {
        mocks.app.getLoginItemSettings.mockReturnValue(
            osState({ openAtLogin: true, launchItems: [runEntry()] })
        )
        expect(reconcileLoginItem(ON_HIDDEN)).toBe('keep')
        expect(mocks.app.setLoginItemSettings).not.toHaveBeenCalled()
    })

    it('writes nothing for an entry the user disabled', () => {
        mocks.app.getLoginItemSettings.mockReturnValue(
            osState({ openAtLogin: true, launchItems: [runEntry({ enabled: false })] })
        )
        reconcileLoginItem(ON_HIDDEN)
        expect(mocks.app.setLoginItemSettings).not.toHaveBeenCalled()
    })

    it('reads the entry under the arguments it would register', () => {
        // Windows reports openAtLogin per argument list, so reading with the
        // wrong one describes a registered login item as missing.
        reconcileLoginItem(ON_HIDDEN)
        expect(mocks.app.getLoginItemSettings).toHaveBeenCalledWith({ args: ['--hidden'] })
    })

    it('reads the entry with no arguments when it would register none', () => {
        reconcileLoginItem(ON_VISIBLE)
        expect(mocks.app.getLoginItemSettings).toHaveBeenCalledWith({ args: [] })
    })

    it('registers macOS without arguments or the removed openAsHidden option', () => {
        mocks.currentPlatform.mockReturnValue('darwin')
        expect(isLoginItemSupported()).toBe(true)
        reconcileLoginItem(ON_HIDDEN)
        expect(mocks.app.getLoginItemSettings).toHaveBeenCalledWith()
        expect(mocks.app.setLoginItemSettings).toHaveBeenCalledWith({ openAtLogin: true })
    })

    it('touches nothing from an unpackaged build', () => {
        // `process.execPath` there is the Electron binary under node_modules, so
        // registering it would start a bare Electron at the next sign-in.
        mocks.app.isPackaged = false
        expect(reconcileLoginItem(ON_HIDDEN)).toBe('skipped')
        expect(mocks.app.getLoginItemSettings).not.toHaveBeenCalled()
        expect(mocks.app.setLoginItemSettings).not.toHaveBeenCalled()
    })

    it('touches nothing on a platform Electron cannot register', () => {
        mocks.currentPlatform.mockReturnValue('linux')
        expect(isLoginItemSupported()).toBe(false)
        expect(reconcileLoginItem(ON_HIDDEN)).toBe('skipped')
        expect(mocks.app.getLoginItemSettings).not.toHaveBeenCalled()
        expect(mocks.app.setLoginItemSettings).not.toHaveBeenCalled()
    })
})

describe('who opened the app at login', () => {
    it('reports a macOS launch by the login item', () => {
        mocks.currentPlatform.mockReturnValue('darwin')
        mocks.app.getLoginItemSettings.mockReturnValue(osState({ wasOpenedAtLogin: true }))
        expect(wasOpenedAtLogin()).toBe(true)
    })

    it('reports a macOS launch by hand', () => {
        mocks.currentPlatform.mockReturnValue('darwin')
        mocks.app.getLoginItemSettings.mockReturnValue(osState({ wasOpenedAtLogin: false }))
        expect(wasOpenedAtLogin()).toBe(false)
    })

    it('does not ask the OS on Windows, where the argument carries the answer', () => {
        mocks.currentPlatform.mockReturnValue('win32')
        expect(wasOpenedAtLogin()).toBe(false)
        expect(mocks.app.getLoginItemSettings).not.toHaveBeenCalled()
    })
})

describe('staying in the tray at startup', () => {
    type StartHiddenInput = Parameters<typeof shouldStartHidden>[0]

    /** A launch by hand on Windows with the preference on; each case changes what it tests. */
    const startsHidden = (overrides: Partial<StartHiddenInput>): boolean =>
        shouldStartHidden({
            argv: ['/path/to/app'],
            platform: 'win32',
            wasOpenedAtLogin: false,
            startHidden: true,
            ...overrides
        })

    it('stays in the tray when the login item passes the hidden argument', () => {
        expect(startsHidden({ argv: ['/path/to/app', APP_HIDDEN_ARGUMENT] })).toBe(true)
    })

    it('stays in the tray on macOS when the OS opened the app at login', () => {
        expect(startsHidden({ platform: 'darwin', wasOpenedAtLogin: true })).toBe(true)
    })

    it('stays in the tray on macOS when the hidden argument is present', () => {
        expect(
            startsHidden({ platform: 'darwin', argv: ['/path/to/app', APP_HIDDEN_ARGUMENT] })
        ).toBe(true)
    })

    it('opens the window for a launch by hand', () => {
        expect(startsHidden({})).toBe(false)
    })

    it('opens the window for a launch by hand on macOS', () => {
        expect(startsHidden({ platform: 'darwin', wasOpenedAtLogin: false })).toBe(false)
    })

    it('ignores an unrelated switch', () => {
        expect(startsHidden({ argv: ['/path/to/app', '--enable-logging'] })).toBe(false)
    })

    it('ignores wasOpenedAtLogin off macOS, where the OS does not report it', () => {
        expect(startsHidden({ platform: 'win32', wasOpenedAtLogin: true })).toBe(false)
        expect(startsHidden({ platform: 'linux', wasOpenedAtLogin: true })).toBe(false)
    })

    it('opens the window at login once start minimized is switched off', () => {
        expect(
            startsHidden({ argv: ['/path/to/app', APP_HIDDEN_ARGUMENT], startHidden: false })
        ).toBe(false)
    })

    it('opens the window on a macOS login launch once start minimized is switched off', () => {
        expect(
            startsHidden({ platform: 'darwin', wasOpenedAtLogin: true, startHidden: false })
        ).toBe(false)
    })
})
