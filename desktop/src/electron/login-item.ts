// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { app } from 'electron'
import { APP_HIDDEN_ARGUMENT } from '@/shared/constants/app'
import type { StartupSettings } from '@/shared/types/ipc-channels'
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
 * macOS: a login item carries no arguments, so `openAsHidden` is the only way to
 * ask for a hidden start. It is deprecated and has no effect on macOS 13 and up,
 * where the app opens its window at login as if it had been started by hand —
 * a platform limitation, not a configuration error.
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
        openAsHidden: settings.startHidden,
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
 * Whether a process started with `argv` should open the Overview window.
 *
 * Only the login item passes {@link APP_HIDDEN_ARGUMENT}, so its absence means a
 * person started the app and expects to see it. Kept pure and separate from the
 * `app.whenReady()` body so the startup decision is testable without Electron.
 */
export function shouldOpenOverviewOnStart(argv: readonly string[]): boolean {
    return !argv.includes(APP_HIDDEN_ARGUMENT)
}
