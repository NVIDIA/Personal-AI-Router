// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { EngineDisplayNames, EnabledEngineTypes, EngineTypes } from '@/shared/constants/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { MODULAR_RUNTIME_BINARIES } from '@/shared/constants/modular-binaries'
import { isExternalRuntime } from '@/ui/utils/engine-ownership'

describe('llamacpp engine', () => {
    it('is a enabled engine type', () => {
        expect(EngineTypes).toContain('llamacpp')
        expect(EnabledEngineTypes).toContain('llamacpp')
        expect(EngineDisplayNames.llamacpp).toBe('llama.cpp')
    })
    it('exposes managed install and model actions', () => {
        const caps = EngineCapabilities.llamacpp
        expect(caps.hasInstall).toEqual(['win32', 'darwin', 'linux'])
        expect(caps.hasEject).toBe(true)
        expect(caps.hasDeleteModel).toBe(true)
        expect(caps.modelOpsWhenStopped).toBe(true)
        expect(caps.hasEnginePort).toBe(true)
        expect(caps.engineHub).toBeUndefined()
    })
    // One predicate decides which surfaces offer controls for a llama.cpp
    // runtime PAIR only observes, so the node list and the engine row agree.
    it('treats a detected, unmanaged llama.cpp as external on every surface', () => {
        expect(isExternalRuntime('llamacpp', 'running', false)).toBe(true)
        expect(isExternalRuntime('llamacpp', 'running', undefined)).toBe(true)
        expect(isExternalRuntime('llamacpp', 'running', true)).toBe(false)
        expect(isExternalRuntime('llamacpp', 'not-installed', undefined)).toBe(false)
        expect(isExternalRuntime('lm-studio', 'running', undefined)).toBe(false)
    })
    // llama.cpp ships no binary of its own: one nvpair-proxy process hosts a
    // facade per engine, so adding an engine adds no runtime executable. This
    // asserts the absence, because a stray entry would mean packaging looking
    // for a file the build never produces.
    it('adds no runtime binary of its own', () => {
        const names = MODULAR_RUNTIME_BINARIES.map(b => b.processName)
        expect(names).not.toContain('llamacpp-proxy')
        expect(names).toContain('nvpair-proxy')
        expect(names.filter(n => n.endsWith('-proxy'))).toEqual(['nvpair-proxy'])
    })
})
