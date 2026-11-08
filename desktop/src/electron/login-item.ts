// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import path from 'path'
import { app } from 'electron'
import type { LaunchItems, LoginItemSettings } from 'electron'
import { APP_HIDDEN_ARGUMENT, APP_ID } from '@/shared/constants/app'
import type { StartupSettings } from '@/shared/types/ipc-channels'
import type { SupportedPlatform } from '@/shared/types/platform'
import { currentPlatform } from '@/shared/utils/platform'
import {
    autostartExecLine,
    autostartExecutable,
    readAutostartEntry,
    removeAutostartEntry,
    writeAutostartEntry,
    type AutostartEntry
} from '@/electron/linux-autostart'

/**
 * Whether this platform has a login item this module can write.
 *
 * Windows and macOS go through Electron's `setLoginItemSettings`, which is
 * documented for those two only. Linux is covered by an XDG autostart file
 * (see `linux-autostart.ts`), because Electron does nothing there.
 */
export function isLoginItemSupported(): boolean {
    return true
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

/**
 * What {@link reconcileLoginItem} does to the OS entry.
 *
 * `opt-out` writes nothing to the OS: the user has switched the entry off
 * outside the app (disabled it in Task Manager or System Settings, or deleted
 * it), so the OS already says no and the stored preference is the part that is
 * out of date. The caller stores `launchAtLogin: false`.
 */
type LoginItemPlan =
    | { kind: 'keep' }
    | { kind: 'remove' }
    | { kind: 'opt-out' }
    // `enabled` is always true: an entry the user disabled is an opt-out and is
    // never rewritten, so a write never has to preserve a disabled flag.
    | { kind: 'register'; enabled: boolean }

/**
 * Whether the OS will start the app at login on macOS. Only an enabled item
 * does: one the user switched off in System Settings reports `requires-approval`,
 * and one they removed there reports `not-registered` or `not-found`. Where the
 * status is absent the plain `openAtLogin` flag decides.
 */
function isMacItemEnabled(current: LoginItemSettings): boolean {
    return current.status ? current.status === 'enabled' : current.openAtLogin
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
 * One principle: the OS state wins over the stored preference. The user can turn
 * the entry off outside the app, and re-registering it at the next launch would
 * undo a choice they made on purpose. So an entry that is off in the OS is not
 * re-armed; it is an `opt-out`, and the caller stores the preference as off.
 * Nothing is written when an entry already matches the preference either, since
 * every write is a statement of intent the user may not have made (Electron's
 * Windows write defaults `enabled` to true, which clears the Task Manager flag).
 *
 * Windows:
 * - preference on, an entry of this app that the user disabled: opt out.
 * - preference on, an enabled entry with this executable and these arguments:
 *   keep it.
 * - preference on, entry missing or stale (other arguments, or an executable an
 *   update moved): register it.
 * - preference off, any entry of this app: remove it.
 *
 * macOS has no arguments, and an item is on only when its status is `enabled`:
 * - preference on, status `requires-approval` (switched off in System Settings),
 *   `not-registered` or `not-found` (removed there): opt out. A first-run item
 *   that is still waiting for approval reads the same way, and the user can turn
 *   Launch at login on again from Settings once it is approved.
 * - preference on, enabled: keep it.
 * - preference off, enabled: remove it. Any other state is already off.
 *
 * `current` must come from `getLoginItemSettings({ args })` with the arguments
 * the preference would register. `openAtLogin` alone is not trusted to mean
 * "registered": the documentation does not say whether a disabled entry still
 * reports it, so the Run entries in `launchItems` are read as well.
 *
 * Linux has no `LoginItemSettings`; see {@link planLinuxLoginItem}.
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
    if (platform === 'linux') return { kind: 'keep' }

    if (platform === 'darwin') {
        const enabled = isMacItemEnabled(current)
        if (settings.launchAtLogin) return enabled ? { kind: 'keep' } : { kind: 'opt-out' }
        return enabled ? { kind: 'remove' } : { kind: 'keep' }
    }

    const owned = ownedWindowsItems(current.launchItems, execPath)
    if (!settings.launchAtLogin) {
        return current.openAtLogin || owned.length > 0 ? { kind: 'remove' } : { kind: 'keep' }
    }

    if (owned.some(item => !item.enabled)) return { kind: 'opt-out' }

    const wantedArgs = windowsLoginItemArgs(settings.startHidden)
    const exe = normalizeWindowsPath(execPath)
    const registered =
        current.openAtLogin ||
        owned.some(
            item => normalizeWindowsPath(item.path) === exe && sameArgs(item.args, wantedArgs)
        )
    if (registered) return { kind: 'keep' }
    return { kind: 'register', enabled: true }
}

/**
 * The same decision for the Linux autostart file, which {@link readAutostartEntry}
 * reports. A file the user deleted, or disabled in their desktop's startup
 * settings, is an opt-out; a file whose `Exec` is not what this build would write
 * (an update moved the executable, or the hidden argument changed) is rewritten.
 */
function planLinuxLoginItem({
    settings,
    entry,
    execLine
}: {
    settings: StartupSettings
    entry: AutostartEntry
    execLine: string
}): LoginItemPlan {
    if (!settings.launchAtLogin) return entry.present ? { kind: 'remove' } : { kind: 'keep' }
    if (!entry.present || !entry.enabled) return { kind: 'opt-out' }
    return entry.exec === execLine ? { kind: 'keep' } : { kind: 'register', enabled: true }
}

/** What Electron reports for a platform that has no login item to ask about. */
const NO_LOGIN_ITEM: LoginItemSettings = {
    openAtLogin: false,
    openAsHidden: false,
    wasOpenedAtLogin: false,
    wasOpenedAsHidden: false,
    restoreState: false,
    status: 'not-registered',
    executableWillLaunchAtLogin: false,
    launchItems: []
}

/**
 * The OS entry as it reads under the arguments `settings` would register. Linux
 * has none: its entry is a file, read through {@link readAutostartEntry}.
 */
function readLoginItem(settings: StartupSettings, platform: SupportedPlatform): LoginItemSettings {
    if (platform === 'linux') return NO_LOGIN_ITEM
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
 * - macOS: the item is `enabled`. One switched off in System Settings
 *   (`requires-approval`) or removed there reads off.
 * - Linux: the autostart file exists and is not disabled. A file the user
 *   deleted reads off.
 * - Where {@link reconcileLoginItem} writes nothing (an unpackaged build) there
 *   is no OS entry of ours to report, so the stored preference is returned and
 *   the switch keeps what the user set.
 */
export function readLaunchAtLogin(settings: StartupSettings): boolean {
    if (!managesLoginItem()) return settings.launchAtLogin
    const platform = currentPlatform()
    if (platform === 'linux') return readAutostartEntry().enabled
    const current = readLoginItem(settings, platform)
    return platform === 'win32' ? current.executableWillLaunchAtLogin : isMacItemEnabled(current)
}

/** Carry out `plan`, writing nothing for `keep`. */
function writeLoginItem(
    plan: LoginItemPlan,
    settings: StartupSettings,
    platform: SupportedPlatform
): void {
    switch (plan.kind) {
        case 'keep':
        case 'opt-out':
            // The OS already says no; the caller records the preference.
            break
        case 'remove':
            if (platform === 'linux') removeAutostartEntry()
            else app.setLoginItemSettings({ openAtLogin: false })
            break
        case 'register':
            if (platform === 'linux') {
                writeAutostartEntry(settings.startHidden)
                break
            }
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
 * - Start minimized, on Windows and Linux, because the argument lives in the Run
 *   entry or the autostart file. An enabled entry is rewritten with the new
 *   argument. An entry the user disabled is left alone (the stored preference
 *   applies when they turn Launch at login back on), and with no entry nothing
 *   is written: the preference is stored, and the next launch reconciles it.
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
        if (hasEntry) {
            const plan = planLoginItem({ settings: next, current, platform, execPath })
            // A disabled entry is the user's choice, not something to rewrite.
            return plan.kind === 'opt-out' ? { kind: 'keep' } : plan
        }
    }
    if (platform === 'linux' && hiddenChanged && next.launchAtLogin && registered) {
        return { kind: 'register', enabled: true }
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
 * Linux: the entry is the autostart file; see {@link planLinuxLoginItem}.
 *
 * `opt-out` means the user switched the entry off outside the app. Nothing is
 * written; the caller stores `launchAtLogin: false` so the preference agrees.
 *
 * Nothing is written where {@link managesLoginItem} is false.
 */
export function reconcileLoginItem(settings: StartupSettings): LoginItemPlan['kind'] | 'skipped' {
    if (!managesLoginItem()) return 'skipped'

    const platform = currentPlatform()
    const plan =
        platform === 'linux'
            ? planLinuxLoginItem({
                  settings,
                  entry: readAutostartEntry(),
                  execLine: autostartExecLine(autostartExecutable(), settings.startHidden)
              })
            : planLoginItem({
                  settings,
                  current: readLoginItem(settings, platform),
                  platform,
                  execPath: process.execPath
              })
    writeLoginItem(plan, settings, platform)
    return plan.kind
}
