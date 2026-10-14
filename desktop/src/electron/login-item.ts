// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import path from 'path'
import { app } from 'electron'
import type { LaunchItems, LoginItemSettings } from 'electron'
import { APP_HIDDEN_ARGUMENT, APP_ID } from '@/shared/constants/app'
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
 * Arguments the Windows login item passes to the app it starts.
 *
 * This is the whole "was I started at login?" signal there: nothing else adds
 * {@link APP_HIDDEN_ARGUMENT}, so a launch by hand still opens the window even
 * while `startHidden` is on. macOS login items carry no arguments, and
 * {@link shouldStartHidden} reads `wasOpenedAtLogin` there instead.
 */
function windowsLoginItemArgs(startHidden: boolean): string[] {
    return startHidden ? [APP_HIDDEN_ARGUMENT] : []
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

/** What {@link reconcileLoginItem} does to the OS entry. */
type LoginItemPlan =
    | { kind: 'keep' }
    | { kind: 'remove' }
    // `enabled` only matters on Windows; see planLoginItem.
    | { kind: 'register'; enabled: boolean }

/**
 * Whether a macOS login item exists. One waiting on approval in System Settings
 * counts: it is registered, and writing again does not approve it.
 */
function isMacItemRegistered(current: LoginItemSettings): boolean {
    return current.openAtLogin || current.status === 'requires-approval'
}

/** Windows paths are case-insensitive, and the Run value may keep its quotes. */
function normalizeWindowsPath(value: string): string {
    return path.win32.normalize(value.replace(/^"|"$/g, '')).toLowerCase()
}

function sameArgs(a: readonly string[], b: readonly string[]): boolean {
    return a.length === b.length && a.every((arg, index) => arg === b[index])
}

/**
 * The Windows Run entries this app owns: the per-user ones Electron writes,
 * under this app's value name or pointing at this executable.
 *
 * The name is matched as well as the path because an app update that moves the
 * executable leaves the old entry under the same name, and that entry (and
 * whether the user disabled it) is what the next write replaces. Machine-wide
 * entries are not Electron's to write or remove, so they are never ours.
 */
function ownedWindowsItems(launchItems: readonly LaunchItems[], execPath: string): LaunchItems[] {
    const exe = normalizeWindowsPath(execPath)
    return launchItems.filter(
        item =>
            item.scope === 'user' &&
            (item.name === APP_ID || normalizeWindowsPath(item.path) === exe)
    )
}

/**
 * Decide what the OS login item needs, given what the OS reports.
 *
 * Nothing is written when an entry already matches the preference, because
 * every write is a statement of intent the user may not have made. On Windows,
 * Electron's write defaults `enabled` to true, which clears the "disabled" flag
 * a user sets in Task Manager or Windows Settings. Reconciling at every launch
 * with that default would silently undo their choice, so:
 *
 * - preference on, an entry registered with this executable and these
 *   arguments: keep it, enabled or not. A disabled entry is the user's choice.
 * - preference on, entry missing or stale (other arguments, or an executable an
 *   update moved): register it, passing `enabled: false` when the entry being
 *   replaced is disabled so the rewrite does not re-arm it.
 * - preference off, any entry of this app: remove it.
 *
 * `current` must come from `getLoginItemSettings({ args })` with the arguments
 * the preference would register. `openAtLogin` alone is not trusted to mean
 * "registered": the documentation does not say whether a disabled entry still
 * reports it, so the Run entries in `launchItems` are read as well.
 *
 * macOS has no arguments and no disabled flag. An entry waiting on approval in
 * System Settings (`requires-approval`) counts as registered, since writing
 * again does not approve it.
 */
export function planLoginItem({
    settings,
    current,
    platform,
    execPath
}: {
    settings: StartupSettings
    current: LoginItemSettings
    platform: SupportedPlatform
    execPath: string
}): LoginItemPlan {
    if (platform !== 'win32') {
        const registered = isMacItemRegistered(current)
        if (settings.launchAtLogin) {
            return registered ? { kind: 'keep' } : { kind: 'register', enabled: true }
        }
        return registered ? { kind: 'remove' } : { kind: 'keep' }
    }

    const owned = ownedWindowsItems(current.launchItems, execPath)
    if (!settings.launchAtLogin) {
        return current.openAtLogin || owned.length > 0 ? { kind: 'remove' } : { kind: 'keep' }
    }

    const wantedArgs = windowsLoginItemArgs(settings.startHidden)
    const exe = normalizeWindowsPath(execPath)
    const registered =
        current.openAtLogin ||
        owned.some(
            item => normalizeWindowsPath(item.path) === exe && sameArgs(item.args, wantedArgs)
        )
    if (registered) return { kind: 'keep' }
    return { kind: 'register', enabled: !owned.some(item => !item.enabled) }
}

/** The OS entry as it reads under the arguments `settings` would register. */
function readLoginItem(settings: StartupSettings, platform: SupportedPlatform): LoginItemSettings {
    return platform === 'win32'
        ? app.getLoginItemSettings({ args: windowsLoginItemArgs(settings.startHidden) })
        : app.getLoginItemSettings()
}

/**
 * Whether this process manages the OS login item at all. An unpackaged build
 * writes nothing: there `process.execPath` is the Electron binary inside
 * `node_modules` and the AppUserModelId is that same path, so a `npm start` on a
 * machine where the installed app already launches at login would add a second
 * entry that starts a bare Electron at sign-in. The preference is still
 * persisted, and the installed build applies it.
 */
function managesLoginItem(): boolean {
    return isLoginItemSupported() && app.isPackaged
}

/**
 * Whether the OS will start this app at login, as Settings shows it.
 *
 * The OS is asked, not the stored preference, because the entry is not only ours
 * to change: on Windows the user can disable it in Task Manager or Windows
 * Settings, and on macOS in System Settings. A switch that kept reading the
 * preference would claim an autostart that never happens.
 *
 * - Windows: `executableWillLaunchAtLogin`, which is false for an entry the user
 *   disabled and ignores the arguments, so it holds whichever hidden-start
 *   setting the entry was written under.
 * - macOS: an entry exists, including one waiting on approval.
 * - Where {@link reconcileLoginItem} writes nothing (Linux, an unpackaged build)
 *   there is no OS entry of ours to report, so the stored preference is returned
 *   and the switch keeps what the user set.
 */
export function readLaunchAtLogin(settings: StartupSettings): boolean {
    if (!managesLoginItem()) return settings.launchAtLogin
    const platform = currentPlatform()
    const current = readLoginItem(settings, platform)
    return platform === 'win32' ? current.executableWillLaunchAtLogin : isMacItemRegistered(current)
}

/** Carry out `plan`, writing nothing for `keep`. */
function writeLoginItem(
    plan: LoginItemPlan,
    settings: StartupSettings,
    platform: SupportedPlatform
): void {
    switch (plan.kind) {
        case 'keep':
            break
        case 'remove':
            app.setLoginItemSettings({ openAtLogin: false })
            break
        case 'register':
            app.setLoginItemSettings(
                platform === 'win32'
                    ? {
                          openAtLogin: true,
                          args: windowsLoginItemArgs(settings.startHidden),
                          enabled: plan.enabled
                      }
                    : { openAtLogin: true }
            )
            break
    }
}

/**
 * Decide what the OS login item needs after the user changed a Startup switch.
 *
 * Unlike the launch reconcile this is the user acting, so it writes only for the
 * switch that was touched:
 *
 * - Launch at login, when the request differs from what the switch showed (the
 *   OS state {@link readLaunchAtLogin} reported, not the stored preference).
 *   Turning it on writes an enabled entry, which also re-enables one the user had
 *   disabled outside the app; that is what flipping the switch asks for. Turning
 *   it off removes the entry.
 * - Start minimized, on Windows only, because the argument lives in the Run
 *   entry. An entry that exists is rewritten with the new argument, keeping it
 *   disabled if the user disabled it. With no entry nothing is written: the
 *   preference is stored, and the next launch reconciles it.
 *
 * Anything else, such as a request that repeats the current value, writes
 * nothing.
 *
 * `registered` is what {@link readLaunchAtLogin} returned before the change;
 * `current` is the OS entry read under the arguments `next` would register.
 */
export function planStartupChange({
    requested,
    previous,
    next,
    registered,
    current,
    platform,
    execPath
}: {
    requested: Partial<StartupSettings>
    previous: StartupSettings
    next: StartupSettings
    registered: boolean
    current: LoginItemSettings
    platform: SupportedPlatform
    execPath: string
}): LoginItemPlan {
    if (typeof requested.launchAtLogin === 'boolean' && requested.launchAtLogin !== registered) {
        return requested.launchAtLogin ? { kind: 'register', enabled: true } : { kind: 'remove' }
    }

    const hiddenChanged =
        typeof requested.startHidden === 'boolean' && requested.startHidden !== previous.startHidden
    if (platform === 'win32' && hiddenChanged && next.launchAtLogin) {
        const hasEntry =
            current.openAtLogin || ownedWindowsItems(current.launchItems, execPath).length > 0
        if (hasEntry) return planLoginItem({ settings: next, current, platform, execPath })
    }
    return { kind: 'keep' }
}

/**
 * Write the login item for a change to the Startup switches. `previous` and
 * `registered` are read before the preference is stored, `next` is the stored
 * result. Returns what it did, `skipped` where nothing is managed.
 */
export function applyStartupChange({
    requested,
    previous,
    next,
    registered
}: {
    requested: Partial<StartupSettings>
    previous: StartupSettings
    next: StartupSettings
    registered: boolean
}): LoginItemPlan['kind'] | 'skipped' {
    if (!managesLoginItem()) return 'skipped'

    const platform = currentPlatform()
    const plan = planStartupChange({
        requested,
        previous,
        next,
        registered,
        current: readLoginItem(next, platform),
        platform,
        execPath: process.execPath
    })
    writeLoginItem(plan, next, platform)
    return plan.kind
}

/**
 * Bring the OS login item in line with `settings`, writing only what differs.
 * Returns what it did: `skipped` where there is nothing to write.
 *
 * Windows: Electron writes the `HKCU\...\Run` entry itself, defaulting the
 * executable to `process.execPath` and the value name to the AppUserModelId, so
 * neither `path` nor `name` is passed — pinning them would freeze the entry on a
 * path an app update moves. `args` carries the hidden-start argument.
 * {@link planLoginItem} decides whether to write at all.
 *
 * macOS: the entry is the app itself; the hidden start is decided at launch by
 * {@link shouldStartHidden}.
 *
 * Nothing is written where {@link managesLoginItem} is false.
 */
export function reconcileLoginItem(settings: StartupSettings): LoginItemPlan['kind'] | 'skipped' {
    if (!managesLoginItem()) return 'skipped'

    const platform = currentPlatform()
    const plan = planLoginItem({
        settings,
        current: readLoginItem(settings, platform),
        platform,
        execPath: process.execPath
    })
    writeLoginItem(plan, settings, platform)
    return plan.kind
}
