// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { APP_HIDDEN_ARGUMENT } from '@/shared/constants/app'
import type { StartupSettings } from '@/shared/types/ipc-channels'
import type { SupportedPlatform } from '@/shared/types/platform'
import { currentPlatform } from '@/shared/utils/platform'

/**
 * Whether this platform has an OS login item Electron can write.
 *
 * `setLoginItemSettings` / `getLoginItemSettings` are documented for darwin and
 * win32 only. On Linux autostart is a per-desktop `.desktop` file Electron does
 * not manage, so both calls are skipped there and Settings reports the toggles
 * as unsupported instead of appearing to work while doing nothing.
 */
export function isLoginItemSupported(): boolean {
    const platform = currentPlatform()
    return platform === 'win32' || platform === 'darwin'
}

/**
 * Arguments the login item passes to the app it starts.
 *
 * This is the whole "was I started at login?" signal: nothing else adds
 * {@link APP_HIDDEN_ARGUMENT}, so a launch by hand still opens the window even
 * while `startHidden` is on.
 */
function loginItemArgs(settings: StartupSettings): string[] {
    return settings.startHidden ? [APP_HIDDEN_ARGUMENT] : []
}

/**
 * Write the OS login item so it matches `settings`. No-op where unsupported.
 *
 * Windows: Electron writes the `HKCU\...\Run` entry itself, defaulting the
 * executable to `process.execPath` and the value name to the AppUserModelId, so
 * neither `path` nor `name` is passed — pinning them would freeze the entry on a
 * path an app update moves. `args` is the only field that must be supplied.
 *
 * macOS: a login item carries no arguments, so there is nothing to pass there.
 * `openAsHidden` is deprecated and ignored on macOS 13 and up, and Electron 44
 * removes it, so it is not written at all.
 *
 * `openAtLogin: false` removes the entry, so this is also the "turn it off" path
 * and is safe to call unconditionally.
 *
 * An unpackaged build writes nothing. There `process.execPath` is the Electron
 * binary inside `node_modules` and the AppUserModelId is that same path, so a
 * `npm start` on a machine where the installed app already launches at login
 * would add a second entry that starts a bare Electron at sign-in. The
 * preference is still persisted, and the installed build applies it.
 */
export function applyLoginItem(settings: StartupSettings): void {
    if (!isLoginItemSupported() || !app.isPackaged) return

    app.setLoginItemSettings({
        openAtLogin: settings.launchAtLogin,
        args: loginItemArgs(settings)
    })
}

/**
 * The login item as the OS currently reports it, for diagnostics. `null` where
 * unsupported.
 *
 * `settings` must be the same values {@link applyLoginItem} was given: on
 * Windows `openAtLogin` is only true when the registered arguments match the
 * ones asked about, so reading with the wrong arguments reports a login item
 * that is in fact registered as missing.
 */
export function readLoginItem(settings: StartupSettings): Electron.LoginItemSettings | null {
    if (!isLoginItemSupported()) return null
    return app.getLoginItemSettings({ args: loginItemArgs(settings) })
}

/**
 * Whether the OS started this process as a login item. macOS only: Windows
 * reports it through {@link APP_HIDDEN_ARGUMENT} instead, and the field does not
 * exist there.
 *
 * Read once at launch. It describes how this process was started, so it says
 * nothing about a later second launch that reaches this process as an event.
 */
export function wasOpenedAtLogin(): boolean {
    if (currentPlatform() !== 'darwin') return false
    return app.getLoginItemSettings().wasOpenedAtLogin
}

/**
 * Whether a launch should stay in the tray with no window.
 *
 * Only when the user asked for it (`startHidden`), and only for a launch the
 * login item made, which the platforms report differently:
 *
 * - Windows and Linux: the login item carries {@link APP_HIDDEN_ARGUMENT} and
 *   nothing else adds it, so a launch by hand still opens the window.
 * - macOS: a login item carries no arguments, so the OS reports
 *   `wasOpenedAtLogin` instead.
 *
 * Pure and separate from the `app.whenReady()` body so the decision is testable
 * without Electron.
 */
export function shouldStartHidden({
    argv,
    platform,
    wasOpenedAtLogin: openedAtLogin,
    startHidden
}: {
    argv: readonly string[]
    platform: SupportedPlatform
    wasOpenedAtLogin: boolean
    startHidden: boolean
}): boolean {
    if (!startHidden) return false
    return argv.includes(APP_HIDDEN_ARGUMENT) || (platform === 'darwin' && openedAtLogin)
}
