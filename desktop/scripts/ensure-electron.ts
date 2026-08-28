// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Self-healing Electron binary install, wired as part of the npm `postinstall`
 * hook.
 *
 * `electron` ships its prebuilt runtime via its own `postinstall` (`install.js`),
 * which extracts the platform binary into `node_modules/electron/dist/` and
 * writes `node_modules/electron/path.txt`. npm records per-package build state in
 * its content-addressable cache, and that state is NOT cleared when you delete
 * `node_modules`. After Electron's script has run once, a later reinstall links
 * the package from cache and treats the build as already done — so it skips the
 * extraction, leaving `dist/` absent. `electron-vite dev` then fails with
 * `Error: Electron uninstall` from `getElectronPath()`.
 *
 * This step re-runs Electron's own installer when (and only when) the extracted
 * binary is missing, so a fresh clone or a `node_modules` wipe always ends up
 * with a runnable Electron.
 *
 * It is intentionally a no-op — never a hard failure — when:
 *   - `electron` is not installed (e.g. a production `--omit=dev` install), or
 *   - the binary is already extracted and present.
 */

import { spawnSync } from 'node:child_process'
import { existsSync, readFileSync } from 'node:fs'
import path from 'node:path'

const REPO_ROOT = path.resolve(__dirname, '..')
const ELECTRON_DIR = path.join(REPO_ROOT, 'node_modules', 'electron')
const INSTALL_SCRIPT = path.join(ELECTRON_DIR, 'install.js')
const PATH_TXT = path.join(ELECTRON_DIR, 'path.txt')

/** True when Electron's prebuilt binary is already extracted and on disk. */
function binaryPresent(): boolean {
    if (!existsSync(PATH_TXT)) return false
    const rel = readFileSync(PATH_TXT, 'utf8').trim()
    if (!rel) return false
    return existsSync(path.join(ELECTRON_DIR, 'dist', rel))
}

function main(): void {
    if (!existsSync(INSTALL_SCRIPT)) return // electron not installed (prod install)
    if (binaryPresent()) return // already runnable

    console.log('[ensure-electron] Electron binary missing; extracting…')
    const res = spawnSync(process.execPath, [INSTALL_SCRIPT], {
        cwd: ELECTRON_DIR,
        encoding: 'utf8',
        stdio: 'inherit'
    })

    if (res.status !== 0 || !binaryPresent()) {
        console.warn(
            '[ensure-electron] Could not install the Electron binary automatically.\n' +
                '                  Run it manually:  npm rebuild electron'
        )
        return
    }

    console.log('[ensure-electron] Electron binary ready.')
}

main()
