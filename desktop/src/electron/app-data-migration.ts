// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'fs'
import path from 'path'

// LOCK_STALE_MS and LOCK_POLL_MS match lockStaleAfter and lockPoll in
// services/shared/appdir/appdir.go; the broker breaks a lock by the same rule.
const LOCK_STALE_MS = 2 * 60_000
const LOCK_POLL_MS = 100
const LOCK_HEARTBEAT_MS = LOCK_STALE_MS / 4
/**
 * Unlike the broker, Electron gives up after this long: it runs on the main
 * thread before any window exists, and the broker migrates again before it
 * starts a worker, so a node identity is never lost by skipping here.
 */
const LOCK_WAIT_MS = 10_000

/** What one previous directory's migration did. */
export interface AppDataDirectoryMigration {
    source: string
    moved: number
    /** Left in place because the destination already had an entry of that name. */
    kept: string[]
    /** Could not be moved (for example, a locked file); retried on the next launch. */
    failed: string[]
}

type LockOutcome = { acquired: true } | { acquired: false; reason: string }

function moveEntry(
    sourcePath: string,
    destinationPath: string,
    result: AppDataDirectoryMigration,
    heartbeat: () => void
): void {
    heartbeat()
    if (!fs.existsSync(destinationPath)) {
        try {
            fs.renameSync(sourcePath, destinationPath)
            result.moved++
        } catch {
            result.failed.push(sourcePath)
        }
        return
    }

    let sourceIsDirectory = false
    let destinationIsDirectory = false
    try {
        sourceIsDirectory = fs.lstatSync(sourcePath).isDirectory()
        destinationIsDirectory = fs.lstatSync(destinationPath).isDirectory()
    } catch {
        result.failed.push(sourcePath)
        return
    }

    if (!sourceIsDirectory || !destinationIsDirectory) {
        result.kept.push(sourcePath)
        return
    }

    let sourceEntries: string[]
    try {
        sourceEntries = fs.readdirSync(sourcePath)
    } catch {
        result.failed.push(sourcePath)
        return
    }

    for (const entry of sourceEntries) {
        moveEntry(
            path.join(sourcePath, entry),
            path.join(destinationPath, entry),
            result,
            heartbeat
        )
    }

    try {
        fs.rmdirSync(sourcePath)
    } catch {
        // Kept or failed entries still hold it open.
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
 * Must run inside `withAppDataMigrationLock`, passing its heartbeat: the
 * destination is merged without atomic no-clobber against a concurrent writer.
 */
export function migrateAppDataDirectory(
    previousRoot: string,
    previousDirectoryName: string,
    root: string,
    directoryName: string,
    skipEntries: readonly string[] = [],
    heartbeat: () => void = () => {}
): AppDataDirectoryMigration {
    const sourcePath = path.join(previousRoot, previousDirectoryName)
    const result: AppDataDirectoryMigration = { source: sourcePath, moved: 0, kept: [], failed: [] }
    if (!fs.existsSync(sourcePath)) return result

    const destinationPath = path.join(root, directoryName)
    if (isSameDirectory(sourcePath, destinationPath)) return result

    let sourceEntries: string[]
    try {
        fs.mkdirSync(destinationPath, { recursive: true })
        sourceEntries = fs.readdirSync(sourcePath)
    } catch {
        result.failed.push(sourcePath)
        return result
    }

    const skip = new Set(skipEntries)
    for (const entry of sourceEntries) {
        if (skip.has(entry)) continue
        moveEntry(
            path.join(sourcePath, entry),
            path.join(destinationPath, entry),
            result,
            heartbeat
        )
    }

    // Remove the previous directory only when empty; skipped, kept, and failed
    // entries keep it around, which is intentional.
    try {
        fs.rmdirSync(sourcePath)
    } catch {
        // Not empty or still in use — leave it for the uninstaller to clean up.
    }
    return result
}

function sleepSync(ms: number): void {
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

/** Creates `filePath` only if it does not exist; false when it already does. */
function createExclusive(filePath: string, contents: string): boolean {
    let fd: number
    try {
        fd = fs.openSync(filePath, 'wx')
    } catch (error) {
        if (error instanceof Error && 'code' in error && error.code === 'EEXIST') return false
        throw error
    }
    try {
        fs.writeSync(fd, contents)
    } finally {
        fs.closeSync(fd)
    }
    return true
}

function isStale(filePath: string): boolean {
    try {
        return Date.now() - fs.statSync(filePath).mtimeMs > LOCK_STALE_MS
    } catch {
        return false
    }
}

/**
 * Removes a lock whose holder stopped refreshing it. The breaker file serializes
 * removal, so two waiters that saw the same stale lock cannot have the second
 * delete the lock the first then created. Throws when a stale lock cannot be
 * removed.
 */
function breakIfStale(lockPath: string): void {
    if (!isStale(lockPath)) return
    const breaker = `${lockPath}.break`
    if (!createExclusive(breaker, '')) {
        if (isStale(breaker)) fs.rmSync(breaker, { force: true })
        return
    }
    try {
        if (isStale(lockPath)) fs.rmSync(lockPath, { force: true })
    } finally {
        fs.rmSync(breaker, { force: true })
    }
}

function readLock(lockPath: string): string | null {
    try {
        return fs.readFileSync(lockPath, 'utf8')
    } catch {
        return null
    }
}

/**
 * Runs `migrate` while holding `lockPath`, the same exclusive-create lock the Go
 * broker takes (`services/shared/appdir`), so the desktop app, the `nvpair` TUI,
 * and a second app launch never move the same directory at once. The lock file
 * holds this process's token, and `migrate` must call the heartbeat it is given
 * so a long migration is not mistaken for a crashed one.
 */
export function withAppDataMigrationLock(
    lockPath: string,
    migrate: (heartbeat: () => void) => void
): LockOutcome {
    const token = `${process.pid}-${Date.now()}`
    const deadline = Date.now() + LOCK_WAIT_MS
    try {
        fs.mkdirSync(path.dirname(lockPath), { recursive: true })
        while (!createExclusive(lockPath, token)) {
            breakIfStale(lockPath)
            if (Date.now() > deadline) {
                return { acquired: false, reason: `${lockPath} is held by another process` }
            }
            sleepSync(LOCK_POLL_MS)
        }
    } catch (error) {
        return {
            acquired: false,
            reason: `could not take ${lockPath}: ${error instanceof Error ? error.message : String(error)}`
        }
    }

    let lastBeat = Date.now()
    const heartbeat = (): void => {
        if (Date.now() - lastBeat < LOCK_HEARTBEAT_MS) return
        lastBeat = Date.now()
        try {
            const now = new Date()
            fs.utimesSync(lockPath, now, now)
        } catch {
            // The next heartbeat retries; a miss only shortens the stale margin.
        }
    }
    try {
        migrate(heartbeat)
        return { acquired: true }
    } finally {
        if (readLock(lockPath) === token) fs.rmSync(lockPath, { force: true })
    }
}
