// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'

/**
 * Regression test for ReDoS finding A4 (scan report). Model and workload names
 * arrive from cluster peers, so they are attacker-influenced. The fix is a hard
 * input-length cap plus an unambiguous regex, making matching linear; a
 * catastrophic backtrack would blow past these generous budgets by orders of
 * magnitude.
 *
 * Finding A5 covered `humanizeInferenceError`, which was chat-only and went
 * with the chat window. That code path no longer exists to regress.
 */
describe('formatModelDisplayName — ReDoS + input cap (A4)', () => {
    it('is linear-time on a hostile quant-suffix name', () => {
        // The old /[-_](Q\d\w*(?:_\w+)*)$/ backtracked exponentially on
        // `m-Q1` + `_a`*n + `!`. The unambiguous /[-_](Q\d\w*)$/ + 256-char cap
        // make it linear.
        const hostile = `model-Q1${'_a'.repeat(50_000)}!`
        const start = performance.now()
        const out = formatModelDisplayName(hostile, 'lm-studio')
        const elapsed = performance.now() - start
        expect(elapsed).toBeLessThan(500)
        expect(typeof out).toBe('string')
    })

    it('caps untrusted input length before formatting', () => {
        const out = formatModelDisplayName('x'.repeat(10_000))
        // Nothing to strip on a bare token, so the result is the 256-char slice.
        expect(out.length).toBeLessThanOrEqual(256)
    })

    it('is linear-time on a full-cap (256-char) worst case with a failing anchor', () => {
        // The 50k-repeat case above proves the cap truncates, but slicing to 256
        // chops the trailing `!` off the very end, leaving the regex an immediate
        // match — NOT the adversarial case. Here the failing anchor sits *inside*
        // the 256-char window (exactly the cap, so no truncation reshapes it), so
        // `[-_](Q\d\w*)$` must fail and backtrack over every `[-_]` start — the
        // exact shape the old nested `(?:_\w+)*` blew up on.
        const worst = `x-Q1${'_9'.repeat(200)}`.slice(0, 255) + '!'
        expect(worst.length).toBe(256)
        const start = performance.now()
        const out = formatModelDisplayName(worst, 'lm-studio')
        const elapsed = performance.now() - start
        expect(elapsed).toBeLessThan(200)
        expect(typeof out).toBe('string')
    })

    it('still extracts a multi-underscore quant suffix (behavior preserved)', () => {
        expect(
            formatModelDisplayName(
                'lmstudio-community/Meta-Llama-3.1-8B-Instruct-GGUF/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf',
                'lm-studio'
            )
        ).toBe('Meta Llama 3.1 8B Instruct (Q4_K_M)')
    })
})
