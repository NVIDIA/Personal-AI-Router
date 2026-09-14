// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { isProxyEngine, PROXY_ENGINES } from '@/electron/service-bridge/modular-state'
import { EngineTypes } from '@/shared/constants/engines'
import type { EngineType } from '@/shared/types/engines'

/**
 * `isProxyEngine` and `PROXY_ENGINES` are the same fact, and when they were
 * written out twice they drifted: MLX was added to the type and the list but not
 * to the predicate. TypeScript cannot catch that — a type guard's body is
 * unchecked, and the signature asserts the very thing being got wrong.
 *
 * The cost was invisible and specific: `emitRemoteEngineStatus` returns early
 * for a non-proxy engine, so a peer's MLX status was never pushed and its card
 * sat on the `initializing` placeholder forever, while Ollama and LM Studio —
 * which the predicate did name — rendered correctly on the very same card.
 */
describe('isProxyEngine agrees with PROXY_ENGINES', () => {
    it('accepts every engine in the list', () => {
        for (const engine of PROXY_ENGINES) {
            expect(isProxyEngine(engine), `${engine} is in PROXY_ENGINES`).toBe(true)
        }
    })

    it('accepts mlx, whose omission stranded the card on "Initializing…"', () => {
        expect(isProxyEngine('mlx')).toBe(true)
    })

    it('rejects every engine not in the list', () => {
        const proxies = new Set<string>(PROXY_ENGINES)
        for (const engine of EngineTypes as readonly EngineType[]) {
            if (proxies.has(engine)) continue
            expect(isProxyEngine(engine), `${engine} is not a proxy engine`).toBe(false)
        }
    })

    it('rejects a value that is not an engine at all', () => {
        expect(isProxyEngine('nonsense' as EngineType)).toBe(false)
    })
})
