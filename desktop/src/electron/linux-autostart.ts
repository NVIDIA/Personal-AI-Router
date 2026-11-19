// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import os from 'os'
import path from 'path'
import { APP_DISPLAY_NAME, APP_HIDDEN_ARGUMENT, APP_ID } from '@/shared/constants/app'

/**
 * Launch at login on Linux, as an XDG autostart entry.
 *
 * Electron's `setLoginItemSettings` does nothing on Linux, where the desktop
 * environment starts whatever `.desktop` files sit in the user's autostart
 * directory. The entry is therefore a file this module writes, reads back, and
 * removes. The file is the whole state: the user can disable it from their
 * desktop's startup settings (which set `Hidden=true` or
 * `X-GNOME-Autostart-enabled=false`) or simply delete it, and both read as off.
 */

const DESKTOP_GROUP = '[Desktop Entry]'

/** The state of the autostart file as the desktop environment will read it. */
export interface AutostartEntry {
    /** The file exists. */
    present: boolean
    /** It exists and is not disabled by `Hidden=true` or `X-GNOME-Autostart-enabled=false`. */
    enabled: boolean
    /** The `Exec` value as written, or null when the file has none. */
    exec: string | null
}

/**
 * `$XDG_CONFIG_HOME/autostart`, falling back to `~/.config/autostart`. The
 * specification says a relative `XDG_CONFIG_HOME` is invalid and must be ignored.
 */
function autostartDirectory(): string {
    const configured = process.env['XDG_CONFIG_HOME']
    const configHome =
        configured && path.isAbsolute(configured) ? configured : path.join(os.homedir(), '.config')
    return path.join(configHome, 'autostart')
}

function autostartFilePath(): string {
    return path.join(autostartDirectory(), `${APP_ID}.desktop`)
}

/**
 * The executable the entry starts. An AppImage runs from a temporary mount, so
 * `process.execPath` points inside it and is gone after the app exits;
 * `$APPIMAGE` is the file the user launched.
 */
export function autostartExecutable(): string {
    const appImage = process.env['APPIMAGE']
    return appImage ? appImage : process.execPath
}

/**
 * Quote one `Exec` argument per the Desktop Entry specification: wrapped in
 * double quotes, with `"`, `` ` ``, `$` and `\` backslash-escaped inside them,
 * and `%` doubled so it is not read as a field code. The backslashes are then
 * doubled once more, because the file format unescapes `\\` in a string value
 * before the `Exec` quoting rules apply to it.
 */
function quoteExecArgument(argument: string): string {
    const quoted = argument.replace(/["`$\\]/g, match => `\\${match}`).replace(/%/g, '%%')
    return `"${quoted}"`.replace(/\\/g, '\\\\')
}

/** The `Exec` value for the autostart entry. */
export function autostartExecLine(executable: string, startHidden: boolean): string {
    const parts = [quoteExecArgument(executable)]
    if (startHidden) parts.push(APP_HIDDEN_ARGUMENT)
    return parts.join(' ')
}

/** The full contents of the autostart file. */
function renderAutostartEntry(executable: string, startHidden: boolean): string {
    return [
        DESKTOP_GROUP,
        'Type=Application',
        `Name=${APP_DISPLAY_NAME}`,
        `Exec=${autostartExecLine(executable, startHidden)}`,
        'Terminal=false',
        'X-GNOME-Autostart-enabled=true',
        'NoDisplay=false',
        ''
    ].join('\n')
}

/**
 * Read the `[Desktop Entry]` group of a `.desktop` file. Only the plain keys
 * are looked at (a localized `Name[de]` is irrelevant here), and the first
 * occurrence of a key wins.
 */
function parseAutostartEntry(contents: string): AutostartEntry {
    const keys = new Map<string, string>()
    let inDesktopGroup = false
    for (const rawLine of contents.split(/\r?\n/)) {
        const line = rawLine.trim()
        if (line === '' || line.startsWith('#')) continue
        if (line.startsWith('[')) {
            inDesktopGroup = line === DESKTOP_GROUP
            continue
        }
        if (!inDesktopGroup) continue
        const separator = line.indexOf('=')
        if (separator < 0) continue
        const key = line.slice(0, separator).trim()
        if (!keys.has(key)) keys.set(key, line.slice(separator + 1).trim())
    }
    const disabled =
        keys.get('Hidden')?.toLowerCase() === 'true' ||
        keys.get('X-GNOME-Autostart-enabled')?.toLowerCase() === 'false'
    return { present: true, enabled: !disabled, exec: keys.get('Exec') ?? null }
}

/** The autostart file as it is on disk now. A missing or unreadable file is absent. */
export function readAutostartEntry(): AutostartEntry {
    try {
        return parseAutostartEntry(fs.readFileSync(autostartFilePath(), 'utf8'))
    } catch {
        return { present: false, enabled: false, exec: null }
    }
}

/** Write the entry, creating the autostart directory if the desktop has none yet. */
export function writeAutostartEntry(startHidden: boolean): void {
    fs.mkdirSync(autostartDirectory(), { recursive: true })
    fs.writeFileSync(
        autostartFilePath(),
        renderAutostartEntry(autostartExecutable(), startHidden),
        {
            encoding: 'utf8',
            mode: 0o644
        }
    )
}

/** Remove the entry. A file that is already gone is the goal, not an error. */
export function removeAutostartEntry(): void {
    fs.rmSync(autostartFilePath(), { force: true })
}
