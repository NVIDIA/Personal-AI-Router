// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Strips pairing secrets and engine launch text out of subprocess output
 * before it reaches any log sink.
 *
 * `nvpair-cluster-manager` returns the six-digit pairing PIN in its
 * `cluster:invite-node` result and carries it on the `Invite` payload of
 * `cluster:invite-received` / `cluster:invite-expired`. Engine settings carry
 * the user's launch command and environment on `launchText`, `launch_args`,
 * and `launch_env` — a hand-written command line that can name private paths
 * or credentials. Those frames travel the broker's stdout, which the
 * supervisor mirrors into the in-memory service log, the on-disk
 * `nvpair.jsonl`, and the user-shareable debug-log export. None of them may
 * survive into any of those.
 *
 * The key names are deliberately specific. A worker-spawn diagnostic logs its
 * own `bin` and `args`, which are PAIR's binaries and flags and are exactly
 * what a support log is for, so this must not match a generic `args` or `env`.
 */

const REDACTED = '[redacted]'

/** JSON keys whose values never belong in a log sink. */
const SENSITIVE_KEYS = new Set(['pin', 'launchText', 'launch_args', 'launch_env'])

/** Quoted key forms, so a line with none of them is returned untouched. */
const SENSITIVE_KEY_HINTS = [...SENSITIVE_KEYS].map(key => `"${key}"`)

/**
 * In malformed JSON, the end of a value cannot be trusted: quotes may be
 * escaped, arrays may be nested, and any value may be truncated. Preserve only
 * the diagnostic prefix before the first sensitive field and redact its entire
 * tail. Matching the key alone also keeps this fallback linear-time.
 */
const QUOTED_SENSITIVE_KEY = new RegExp(`"(${[...SENSITIVE_KEYS].join('|')})"\\s*:`)

function redactValue(value: unknown): unknown {
    if (Array.isArray(value)) return value.map(redactValue)
    if (value !== null && typeof value === 'object') {
        const out: Record<string, unknown> = {}
        for (const [key, inner] of Object.entries(value)) {
            // A null value means the payload carried nothing here; keep that
            // visible rather than implying a secret was present.
            out[key] = SENSITIVE_KEYS.has(key) && inner !== null ? REDACTED : redactValue(inner)
        }
        return out
    }
    return value
}

export function redactSensitiveLogText(text: string): string {
    if (!SENSITIVE_KEY_HINTS.some(hint => text.includes(hint))) return text

    try {
        const parsed: unknown = JSON.parse(text)
        return JSON.stringify(redactValue(parsed))
    } catch {
        const match = QUOTED_SENSITIVE_KEY.exec(text)
        return match ? `${text.slice(0, match.index)}"${match[1]}":"${REDACTED}"` : text
    }
}
