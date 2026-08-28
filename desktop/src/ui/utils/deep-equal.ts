// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/** Deep equality for JSON-like values (primitives, arrays, plain objects, Maps). */
export default function deepEqual(a: unknown, b: unknown): boolean {
    if (Object.is(a, b)) return true
    if (a == null || b == null || typeof a !== 'object' || typeof b !== 'object') return false
    if (Array.isArray(a) !== Array.isArray(b)) return false
    if (Array.isArray(a) && Array.isArray(b)) {
        if (a.length !== b.length) return false
        return a.every((v, i) => deepEqual(v, b[i]))
    }
    if (a instanceof Map || b instanceof Map) {
        if (!(a instanceof Map) || !(b instanceof Map)) return false
        if (a.size !== b.size) return false
        for (const [key, val] of a) {
            if (!b.has(key) || !deepEqual(val, b.get(key))) return false
        }
        return true
    }
    const keysA = Object.keys(a as object).sort()
    const keysB = Object.keys(b as object).sort()
    if (keysA.length !== keysB.length || keysA.some((k, i) => k !== keysB[i])) return false
    return keysA.every(k =>
        deepEqual((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k])
    )
}
