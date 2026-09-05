// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { appendFileSync, existsSync, mkdirSync, renameSync, statSync } from 'fs'
import { LogEntry, LogLevel, StructuredLogger, StructuredLogPayload } from '@/shared/types/log'
import { PathProvider } from '@/electron/path'
import { join } from 'path'

// Cap the active log file by size, not by entry count: the backend at debug
// level is high-volume and every entry that fits on disk is wanted. When
// nvpair.jsonl crosses this, it rotates to nvpair.1.jsonl (a single kept generation),
// so the on-disk total stays ~2x this. Rotation is a rename — O(1) even at
// hundreds of MB, no whole-file rewrite.
const MAX_LOG_FILE_BYTES = 1024 * 1024 * 100 // 100MB active (~200MB total with one rotation)
const LOG_FILE_NAME = 'nvpair.jsonl'
const ROTATED_LOG_FILE_NAME = 'nvpair.1.jsonl'

let logFilePath = ''
let rotatedLogFilePath = ''
let approxFileSize = 0

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

function errorToPlainObject(err: Error): Record<string, unknown> {
    const out: Record<string, unknown> = {
        name: err.name,
        message: err.message,
        ...(err.stack && { stack: err.stack })
    }
    if (err.cause !== undefined) {
        out.cause = err.cause instanceof Error ? errorToPlainObject(err.cause) : err.cause
    }
    return out
}

function normalizePayloadData(data: unknown): unknown {
    if (data instanceof Error) return errorToPlainObject(data)
    if (Array.isArray(data)) return data.map(normalizePayloadData)
    if (data !== null && typeof data === 'object') {
        const out: Record<string, unknown> = {}
        for (const [k, v] of Object.entries(data)) {
            out[k] = normalizePayloadData(v)
        }
        return out
    }
    return data
}

function rotateIfNeeded(): void {
    if (approxFileSize <= MAX_LOG_FILE_BYTES) return
    try {
        // Overwrite any previous rotated generation; only one is kept.
        renameSync(logFilePath, rotatedLogFilePath)
        approxFileSize = 0
    } catch {
        /* best-effort */
    }
}

let stdoutAvailable = true

function isBrokenPipe(error: unknown): boolean {
    return (
        typeof error === 'object' &&
        error !== null &&
        'code' in error &&
        (error as NodeJS.ErrnoException).code === 'EPIPE'
    )
}

// A piped development launch can lose its stdout consumer before Electron has
// finished shutting down. Treat EPIPE as console detachment: structured file
// logging remains available, and shutdown must not become an uncaught exception.
process.stdout.on('error', error => {
    if (isBrokenPipe(error)) {
        stdoutAvailable = false
        return
    }
    throw error
})

function writeEntry(scope: string, level: string, payload: StructuredLogPayload): void {
    const now = new Date()
    const normalizedData =
        payload.data !== undefined
            ? (normalizePayloadData(payload.data) as object | unknown[])
            : undefined

    const entry: LogEntry = {
        level,
        time: now.toISOString(),
        source: scope,
        sublevel: payload.sublevel,
        message: payload.message,
        data: normalizedData
    }

    const line = JSON.stringify(entry) + '\n'

    if (logFilePath) {
        try {
            appendFileSync(logFilePath, line, 'utf8')
            approxFileSize += Buffer.byteLength(line, 'utf8')
            rotateIfNeeded()
        } catch {
            /* best-effort */
        }
    }

    // Console output with scope prefix
    const prefix = `(${scope})`.padEnd(24)
    const msg = payload.message ?? ''
    const dataStr = payload.data ? ` ${JSON.stringify(payload.data)}` : ''
    if (!stdoutAvailable || process.stdout.destroyed || !process.stdout.writable) return

    try {
        process.stdout.write(`${now.toLocaleTimeString()} ${prefix} > ${msg}${dataStr}\n`)
    } catch (error) {
        if (isBrokenPipe(error)) {
            stdoutAvailable = false
            return
        }
        throw error
    }
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

export function getStructuredLogFilePath(): string {
    return logFilePath
}

export function initFileLogger(paths: PathProvider): void {
    const logDir = join(paths.getUserData(), 'logs')
    mkdirSync(logDir, { recursive: true })

    logFilePath = join(logDir, LOG_FILE_NAME)
    rotatedLogFilePath = join(logDir, ROTATED_LOG_FILE_NAME)

    // Migrate old .json -> .jsonl
    const oldPath = logFilePath.replace(/\.jsonl$/, '.json')
    if (existsSync(oldPath) && !existsSync(logFilePath)) {
        try {
            renameSync(oldPath, logFilePath)
        } catch {
            /* best-effort */
        }
    }

    // Seed the running size from disk so an already-oversized file rotates on the
    // first write after launch instead of growing further.
    try {
        const stat = statSync(logFilePath)
        approxFileSize = stat.size
    } catch {
        approxFileSize = 0
    }
}

export function createStructuredLogger(scope: string): StructuredLogger {
    const logWith = (level: LogLevel, payload: StructuredLogPayload) => {
        writeEntry(scope, level, payload)
    }
    return {
        info: p => logWith('info', p),
        warn: p => logWith('warn', p),
        error: p => logWith('error', p),
        verbose: p => logWith('verbose', p),
        debug: p => logWith('debug', p),
        silly: p => logWith('silly', p)
    }
}
