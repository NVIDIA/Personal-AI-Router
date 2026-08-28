// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Payload required for typed logger calls (logger.info, logger.error, etc.).
 * Enforces a consistent shape so the logs view can filter by sublevel and render message + data (Prism).
 */
export interface StructuredLogPayload {
    sublevel: string
    message?: string
    data?: object | unknown[]
}

/**
 * Log entry shape used by the file logger transport and log store (main + renderer).
 * All entries use sublevel and optional message/data (strict structured format).
 */
export interface LogEntry {
    level: string
    time: string
    /** Logger scope (e.g. 'api-bridge'). */
    source?: string
    /** Filterable category. */
    sublevel: string
    /** Human-readable message (optional). */
    message?: string
    /** Optional payload, rendered with Prism. */
    data?: object | unknown[]
}

export interface LogPage {
    entries: LogEntry[]
    total: number
    levels: string[]
    sources: string[]
    sublevels: string[]
}

export type LogLevel = 'info' | 'warn' | 'error' | 'verbose' | 'debug' | 'silly'

export interface StructuredLogger {
    info(payload: StructuredLogPayload): void
    warn(payload: StructuredLogPayload): void
    error(payload: StructuredLogPayload): void
    verbose(payload: StructuredLogPayload): void
    debug(payload: StructuredLogPayload): void
    silly(payload: StructuredLogPayload): void
}
