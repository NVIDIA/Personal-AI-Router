// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Strips pairing secrets out of subprocess output before it reaches any log
 * sink.
 *
 * `nvpair-cluster-manager` returns the six-digit pairing PIN in its
 * `cluster:invite-node` result and carries it on the `Invite` payload of
 * `cluster:invite-received` / `cluster:invite-expired`. Those frames travel the
 * broker's stdout, which the supervisor mirrors into the in-memory service log,
 * the on-disk `nvpair.jsonl`, and the user-shareable debug-log export. The PIN
 * must not survive into any of them.
 */

const REDACTED = '[redacted]'

/** JSON keys whose values never belong in a log sink. */
const SENSITIVE_KEYS = new Set(['pin'])

/**
 * Linear-time (no nested quantifier) fallback for output that is not valid
 * JSON, so a malformed frame cannot smuggle a PIN past the parsed path.
 */
const QUOTED_SENSITIVE_VALUE = /"pin"(\s*):(\s*)"[^"]*"/g

function redactValue(value: unknown): unknown {
    if (Array.isArray(value)) return value.map(redactValue)
    if (value !== null && typeof value === 'object') {
        const out: Record<string, unknown> = {}
        for (const [key, inner] of Object.entries(value)) {
            // A null value means "no PIN on this payload"; keep that visible
            // rather than implying a secret was present.
            out[key] = SENSITIVE_KEYS.has(key) && inner !== null ? REDACTED : redactValue(inner)
        }
        return out
    }
    return value
}

export function redactSensitiveLogText(text: string): string {
    if (!text.includes('"pin"')) return text

    try {
        const parsed: unknown = JSON.parse(text)
        return JSON.stringify(redactValue(parsed))
    } catch {
        return text.replace(QUOTED_SENSITIVE_VALUE, `"pin"$1:$2"${REDACTED}"`)
    }
}
