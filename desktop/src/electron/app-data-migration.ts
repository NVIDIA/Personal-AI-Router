// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'

const LOCK_STALE_MS = 2 * 60_000
const LOCK_WAIT_MS = 10_000
const LOCK_POLL_MS = 100

function moveEntry(sourcePath: string, destinationPath: string): boolean {
    if (!fs.existsSync(destinationPath)) {
        try {
            fs.renameSync(sourcePath, destinationPath)
            return true
        } catch (error) {
            console.warn(`Could not migrate ${sourcePath}:`, error)
            return false
        }
    }

    let sourceIsDirectory = false
    let destinationIsDirectory = false
    try {
        sourceIsDirectory = fs.lstatSync(sourcePath).isDirectory()
        destinationIsDirectory = fs.lstatSync(destinationPath).isDirectory()
    } catch (error) {
        console.warn(`Could not inspect app data migration paths:`, error)
        return false
    }

    if (!sourceIsDirectory || !destinationIsDirectory) return false

    let sourceEntries: string[]
    try {
        sourceEntries = fs.readdirSync(sourcePath)
    } catch (error) {
        console.warn(`Could not read app data migration directory ${sourcePath}:`, error)
        return false
    }

    for (const entry of sourceEntries) {
        moveEntry(path.join(sourcePath, entry), path.join(destinationPath, entry))
    }

    try {
        fs.rmdirSync(sourcePath)
        return true
    } catch {
        return false
    }
}

/** True when both paths exist and name one directory (a case-only difference). */
function isSameDirectory(a: string, b: string): boolean {
    try {
        const aStat = fs.statSync(a)
        const bStat = fs.statSync(b)
        return aStat.dev === bStat.dev && aStat.ino === bStat.ino
    } catch {
        return false
    }
}

/**
 * Moves existing app data into the renamed directory without overwriting files
 * already created there. Conflicting source files remain in the previous
 * directory so migration cannot destroy either copy.
 *
 * `skipEntries` are top-level source entries that are intentionally left in the
 * previous directory (e.g. the generated `nvpair` launcher `bin`, whose absolute
 * path is baked into the user's PATH — moving it would break `nvpair` in
 * already-open terminals). See `src/electron/nvpair-command.ts`.
 *
 * Must run inside `withAppDataMigrationLock`: the destination is created and
 * merged without atomic no-clobber against a concurrent writer.
 */
export function migrateAppDataDirectory(
    previousRoot: string,
    previousDirectoryName: string,
    root: string,
    directoryName: string,
    skipEntries: readonly string[] = []
): void {
    if (previousRoot === root && previousDirectoryName === directoryName) return

    const sourcePath = path.join(previousRoot, previousDirectoryName)
    if (!fs.existsSync(sourcePath)) return

    const destinationPath = path.join(root, directoryName)
    if (isSameDirectory(sourcePath, destinationPath)) return

    // Fast path: nothing to skip and no destination yet — move the whole tree
    // atomically once the destination's parent exists.
    if (skipEntries.length === 0 && !fs.existsSync(destinationPath)) {
        try {
            fs.mkdirSync(root, { recursive: true })
        } catch (error) {
            console.warn(`Could not create app data destination ${root}:`, error)
            return
        }
        moveEntry(sourcePath, destinationPath)
        return
    }

    // Merge path: ensure the destination exists, then move each non-skipped
    // entry into it without overwriting anything already there.
    try {
        fs.mkdirSync(destinationPath, { recursive: true })
    } catch (error) {
        console.warn(`Could not create app data destination ${destinationPath}:`, error)
        return
    }

    let sourceEntries: string[]
    try {
        sourceEntries = fs.readdirSync(sourcePath)
    } catch (error) {
        console.warn(`Could not read app data migration directory ${sourcePath}:`, error)
        return
    }

    const skip = new Set(skipEntries)
    for (const entry of sourceEntries) {
        if (skip.has(entry)) continue
        moveEntry(path.join(sourcePath, entry), path.join(destinationPath, entry))
    }

    // Remove the previous directory only when empty; skipped entries (and any
    // still-locked file) keep it around, which is intentional.
    try {
        fs.rmdirSync(sourcePath)
    } catch {
        // Not empty or still in use — leave it for the uninstaller to clean up.
    }
}

function isFileExistsError(error: unknown): boolean {
    return error instanceof Error && 'code' in error && error.code === 'EEXIST'
}

function sleepSync(ms: number): void {
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

/**
 * Runs `migrate` while holding `lockPath`, the same exclusive-create lock the Go
 * broker takes (`services/shared/appdir`), so the desktop app, the `nvpair` TUI,
 * and a second app launch never move the same directory at once. A lock older
 * than two minutes is a crashed holder's and is broken. Returns false without
 * migrating when a live holder keeps it past the wait.
 */
export function withAppDataMigrationLock(lockPath: string, migrate: () => void): boolean {
    try {
        fs.mkdirSync(path.dirname(lockPath), { recursive: true })
    } catch (error) {
        console.warn(`Could not create app data migration lock directory:`, error)
        return false
    }

    const deadline = Date.now() + LOCK_WAIT_MS
    for (;;) {
        try {
            fs.closeSync(fs.openSync(lockPath, 'wx'))
            break
        } catch (error) {
            if (!isFileExistsError(error)) {
                console.warn(`Could not create app data migration lock ${lockPath}:`, error)
                return false
            }
        }
        try {
            if (Date.now() - fs.statSync(lockPath).mtimeMs > LOCK_STALE_MS) {
                fs.rmSync(lockPath, { force: true })
                continue
            }
        } catch {
            continue
        }
        if (Date.now() > deadline) {
            console.warn(`App data migration lock ${lockPath} is held by another process`)
            return false
        }
        sleepSync(LOCK_POLL_MS)
    }

    try {
        migrate()
        return true
    } finally {
        fs.rmSync(lockPath, { force: true })
    }
}
