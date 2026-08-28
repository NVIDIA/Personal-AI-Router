// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createStructuredLogger } from '@/shared/utils/log'

const log = createStructuredLogger('app')

const SAMPLE_INTERVAL_MS = 500

// Report only stalls long enough to be user-visible or to distort a timing
// measurement. Ordinary scheduling jitter sits in the tens of milliseconds.
const STALL_REPORT_THRESHOLD_MS = 1_000

let timer: ReturnType<typeof setInterval> | null = null

/**
 * Report when the main process's event loop stops running.
 *
 * A blocked main process is invisible in the structured log by construction:
 * every backend line reaches the log through this process, so while it is stuck
 * the whole tree appears silent, and shutdown or startup timings measured with
 * setTimeout are inflated by however long the loop was away. That produces field
 * logs where a gap looks like a backend hang and a grace period looks like a
 * child refusing to exit.
 *
 * The interval is deliberately the instrument: when the loop is blocked this
 * callback runs late, and how late is the measurement. It is unref'd so it never
 * holds the process open, and it is never stopped — teardown is exactly when the
 * measurement matters most.
 */
export function startEventLoopMonitor(): void {
    if (timer) return

    let lastTickAt = Date.now()
    timer = setInterval(() => {
        const now = Date.now()
        const blockedMs = now - lastTickAt - SAMPLE_INTERVAL_MS
        lastTickAt = now
        if (blockedMs >= STALL_REPORT_THRESHOLD_MS) {
            log.warn({
                sublevel: 'lifecycle',
                message: `Main process event loop blocked for ${blockedMs}ms`
            })
        }
    }, SAMPLE_INTERVAL_MS)
    timer.unref()
}
