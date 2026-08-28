// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Best-effort detection of a license's SPDX identifier from the raw text of a
 * LICENSE file. Used to surface the application's own license type in the UI
 * and to label the third-party Go modules in the generated notice file.
 *
 * This is intentionally a small set of the licenses PAIR is likely to ship
 * under; anything unrecognized returns `'Unknown'`.
 *
 * A single LICENSE file can carry more than one grant. `howett.net/plist`, for
 * example, is BSD-2-Clause for the package itself with a BSD-3-Clause block for
 * incorporated Go Authors code appended. First-match-wins on the BSD variants
 * would silently drop the primary grant (the appended BSD-3 block contains
 * "neither the name of", so the whole file would read as BSD-3-Clause), so the
 * BSD detector counts blocks and emits a compound `A AND B` identifier when
 * both variants are present.
 */
interface LicenseMatcher {
    detect: (text: string) => string | null
}

const REDISTRIBUTION_CLAUSE = /redistribution and use in source and binary forms/gi
const BSD3_NAME_CLAUSE = /neither the name of/gi

function countMatches(text: string, pattern: RegExp): number {
    return text.match(pattern)?.length ?? 0
}

/**
 * Classifies the BSD-family grants in a LICENSE file. Each BSD block opens with
 * the "redistribution and use in source and binary forms" clause; a 3-clause
 * block additionally carries the "neither the name of" advertising clause. The
 * difference between the two counts is the number of 2-clause blocks, so a file
 * with one of each resolves to `BSD-2-Clause AND BSD-3-Clause`.
 */
function detectBsd(text: string): string | null {
    const bsdBlocks = countMatches(text, REDISTRIBUTION_CLAUSE)
    if (bsdBlocks === 0) return null
    const bsd3Blocks = countMatches(text, BSD3_NAME_CLAUSE)
    const ids: string[] = []
    if (bsdBlocks > bsd3Blocks) ids.push('BSD-2-Clause')
    if (bsd3Blocks > 0) ids.push('BSD-3-Clause')
    return ids.join(' AND ')
}

const MATCHERS: LicenseMatcher[] = [
    {
        detect: text =>
            /\bapache license\b/i.test(text) && /version\s*2\.0/i.test(text) ? 'Apache-2.0' : null
    },
    {
        detect: text => (/mozilla public license\s*version\s*2\.0/i.test(text) ? 'MPL-2.0' : null)
    },
    {
        detect: text =>
            /gnu general public license/i.test(text) && /version\s*3/i.test(text) ? 'GPL-3.0' : null
    },
    { detect: detectBsd },
    {
        detect: text => (/\bISC License\b/i.test(text) ? 'ISC' : null)
    },
    {
        detect: text =>
            /\bMIT License\b/i.test(text) ||
            /permission is hereby granted, free of charge/i.test(text)
                ? 'MIT'
                : null
    }
]

/** Returns the detected SPDX identifier, or `'Unknown'` when nothing matches. */
export function detectLicenseType(text: string): string {
    const normalized = text.trim()
    if (!normalized) return 'Unknown'
    for (const matcher of MATCHERS) {
        const id = matcher.detect(normalized)
        if (id) return id
    }
    return 'Unknown'
}
