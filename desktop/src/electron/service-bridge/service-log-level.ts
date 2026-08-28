// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * The severity a backend log line reports about itself.
 *
 * Every service writes through one handler with a fixed shape:
 *
 *     HH:MM:SS.mmm [component] LEVEL message key=value
 *
 * so the level is stated in the line and does not have to be guessed. It used
 * to be taken from the stream instead, and since the services log everything —
 * including debug traffic — to stderr, that filed the great majority of the
 * debug panel as warnings and left a real warning indistinguishable from a
 * routine one.
 *
 * The pattern is anchored so only the handler's own prefix can match. A message
 * that merely contains the word ERROR is not a claim about its own severity.
 */
const LEVEL_LINE = /^\d{2}:\d{2}:\d{2}\.\d{3} \[[^\]]+] (DEBUG|INFO|WARN|ERROR) /

type ServiceLogLevel = 'verbose' | 'info' | 'warn' | 'error'

/**
 * Classify one line of a service's output.
 *
 * Lines that do not carry the handler's prefix are not ours: a Go runtime
 * panic, a standard-library message, or a third-party tool's output. Nothing in
 * them states a severity, so the stream is the only signal left — unstructured
 * stderr is the shape a crash takes and stays visible at `warn`, while stdout
 * is the JSON-RPC channel and is noise at anything above `verbose`.
 */
export function serviceLogLevel(stream: 'stdout' | 'stderr', text: string): ServiceLogLevel {
    switch (LEVEL_LINE.exec(text)?.[1]) {
        case 'DEBUG':
            return 'verbose'
        case 'INFO':
            return 'info'
        case 'WARN':
            return 'warn'
        case 'ERROR':
            return 'error'
        default:
            return stream === 'stderr' ? 'warn' : 'verbose'
    }
}
