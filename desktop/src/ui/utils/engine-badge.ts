// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Upper bound on the length of an engine identifier shown in a badge. A workload
 * can name any engine, and the name arrives over the wire from the workload
 * manager's local ingress or from a cluster peer, so it is capped before it
 * reaches the DOM.
 */
const MAX_ENGINE_NAME_CHARS = 64

/**
 * The badge for an engine PAIR has no logo for. `name` is the identifier,
 * capped, for the badge's tooltip and accessible name; `monogram` is the first
 * two letters or digits of it, upper-cased, or `?` when it has none.
 */
export function engineBadge(engine: string): { monogram: string; name: string } {
    const name = engine.slice(0, MAX_ENGINE_NAME_CHARS)
    const monogram = name
        .replace(/[^a-z0-9]/gi, '')
        .slice(0, 2)
        .toUpperCase()
    return { monogram: monogram || '?', name }
}
