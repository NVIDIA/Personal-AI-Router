// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { EngineDisplayNames, EnabledEngineTypes, EngineTypes } from '@/shared/constants/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { MODULAR_RUNTIME_BINARIES } from '@/shared/constants/modular-binaries'

describe('llamacpp engine', () => {
    it('is a enabled engine type', () => {
        expect(EngineTypes).toContain('llamacpp')
        expect(EnabledEngineTypes).toContain('llamacpp')
        expect(EngineDisplayNames.llamacpp).toBe('llama.cpp')
    })
    it('cannot install, load, eject, or delete', () => {
        const caps = EngineCapabilities.llamacpp
        expect(caps.hasInstall).toEqual([])
        expect(caps.hasEject).toBe(false)
        expect(caps.hasDeleteModel).toBe(false)
        expect(caps.hasEnginePort).toBe(true)
        expect(caps.engineHub).toBeUndefined()
    })
    it('ships llamacpp-proxy as a broker-owned binary', () => {
        const bin = MODULAR_RUNTIME_BINARIES.find(b => b.processName === 'llamacpp-proxy')
        expect(bin?.baseName).toBe('llamacpp-proxy')
        expect(bin?.launchOwner).toBe('broker')
        expect(bin?.optional).toBe(true)
    })
})
