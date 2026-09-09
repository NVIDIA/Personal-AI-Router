// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { resolveLookupQuery } from '@/ui/utils/model-hub-lookup'

/**
 * The Ollama hub browses a committed snapshot, so a model published after that
 * snapshot cannot be found by searching. When a search matches nothing, the hub
 * resolves the typed name against the registry instead. These cover which
 * queries earn that request.
 */

describe('resolveLookupQuery', () => {
    it('accepts a well-formed model reference', () => {
        expect(resolveLookupQuery('ollama', 'qwen3.5:32b')).toBe('qwen3.5:32b')
        expect(resolveLookupQuery('ollama', 'gemma3')).toBe('gemma3')
        expect(resolveLookupQuery('ollama', 'some-namespace/some-model:q4_K_M')).toBe(
            'some-namespace/some-model:q4_K_M'
        )
    })

    it('trims surrounding whitespace from a pasted name', () => {
        expect(resolveLookupQuery('ollama', '  gemma3:27b \n')).toBe('gemma3:27b')
    })

    it('declines a query that is not a model reference', () => {
        // Free-text searching is what the local filter is for; only an exact
        // name is worth a request, because the registry has no search endpoint.
        for (const query of [
            '',
            '   ',
            'a model with spaces',
            'has:two:colons',
            '/leading-slash',
            'trailing-slash/',
            '-leading-punctuation',
            'inject`whoami`',
            'x'.repeat(257)
        ]) {
            expect(resolveLookupQuery('ollama', query)).toBeNull()
        }
    })

    it('declines cloud-only tags, which cannot be pulled to a local engine', () => {
        // Same rule the catalog scraper applies when it strips these rows.
        expect(resolveLookupQuery('ollama', 'qwen3-vl:235b-cloud')).toBeNull()
        expect(resolveLookupQuery('ollama', 'gpt-oss:120b-cloud')).toBeNull()
        expect(resolveLookupQuery('ollama', 'deepseek-v3.1:cloud-preview')).toBeNull()
        // The token is delimited, so a name that merely contains "cloud" stays.
        expect(resolveLookupQuery('ollama', 'icloud-summarizer:7b')).toBe('icloud-summarizer:7b')
    })

    it('declines engines whose catalog is already fetched live', () => {
        // LM Studio refetches from Hugging Face on a six-hour TTL, so it never
        // falls behind a release and has no gap for a lookup to close.
        expect(resolveLookupQuery('lm-studio', 'lmstudio-community/Qwen3-8B-GGUF')).toBeNull()
        expect(resolveLookupQuery(null, 'qwen3:8b')).toBeNull()
    })

    it('matches in linear time on an adversarial query', () => {
        // Neither `/` nor `:` is in the segment character class, so each
        // separator is the only place a repetition can start. A catastrophic
        // backtrack would blow past this budget by orders of magnitude.
        //
        // Kept under the 256-char cap on purpose: a longer string would be
        // rejected on length alone and would never reach the matcher.
        const hostile = `${'a/'.repeat(60)}${'b.'.repeat(60)}!`
        expect(hostile.length).toBeLessThanOrEqual(256)
        const start = performance.now()
        expect(resolveLookupQuery('ollama', hostile)).toBeNull()
        expect(performance.now() - start).toBeLessThan(200)
    })
})
