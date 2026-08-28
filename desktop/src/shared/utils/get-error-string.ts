// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * How far down a `cause` chain to read.
 *
 * `fetch` reports every transport failure as the same "fetch failed" and puts
 * the syscall that actually failed in `cause`. Reading only the top message
 * makes a refused connection, an unreachable host, a reset and a timeout one
 * indistinguishable string — which is how a field report of an endpoint that
 * had stopped answering arrived with seventy-four samples and no way to tell
 * whether the packets ever left the host.
 *
 * Bounded because a chain can be cyclic (`err.cause === err`), and because past
 * the first couple of links the text is longer than it is informative.
 */
const CAUSE_DEPTH_LIMIT = 4

/** The message an error carries itself, ignoring any cause behind it. */
function ownMessage(err: unknown): string {
    if (err instanceof Error) {
        return err.message
    }
    if (err && typeof err === 'object') {
        if ('stderr' in err && err.stderr) {
            return String(err.stderr)
        }
        if ('message' in err && err.message) {
            return String(err.message)
        }
    }
    return String(err ?? '')
}

/**
 * A Node system error's `code` (`ECONNREFUSED`, `EHOSTUNREACH`, …). It is the
 * part worth reading, and it is absent from the message on some platforms.
 */
function systemCode(err: unknown): string {
    if (!err || typeof err !== 'object' || !('code' in err)) return ''
    const code = err.code
    return typeof code === 'string' ? code : ''
}

function describe(err: unknown, depth: number): string {
    const message = ownMessage(err)
    const code = systemCode(err)
    const head = code && !message.includes(code) ? `${message} (${code})` : message

    if (depth >= CAUSE_DEPTH_LIMIT || !(err instanceof Error) || err.cause === undefined) {
        return head
    }
    const cause = describe(err.cause, depth + 1)
    // A cause that only repeats its parent adds length and no meaning.
    if (!cause || cause === head) return head
    return `${head}: ${cause}`
}

export default function getErrorString(err: unknown): string {
    return describe(err, 0)
}
