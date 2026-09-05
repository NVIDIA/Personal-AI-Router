// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { EngineTypes, EnabledEngineTypes, EngineDisplayNames } from '@/shared/constants/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import {
    getModularBridgeState,
    isProxyEngine,
    PROXY_ENGINES
} from '@/electron/service-bridge/modular-state'

describe('llama.cpp Desktop proxy integration', () => {
    it('registers llama.cpp as an enabled proxy engine', () => {
        expect(EngineTypes).toContain('llamacpp')
        expect(EnabledEngineTypes).toContain('llamacpp')
        expect(PROXY_ENGINES).toContain('llamacpp')
        expect(isProxyEngine('llamacpp')).toBe(true)
        expect(EngineDisplayNames.llamacpp).toBe('llama.cpp')
        expect(EngineCapabilities.llamacpp.hasInstall).toEqual([])
        expect(EngineCapabilities.llamacpp.hasEnginePort).toBe(true)
    })

    it('records the broker-reported llama.cpp proxy port', () => {
        const state = getModularBridgeState()

        state.handleNotification({
            source: 'llamacpp-proxy',
            method: 'ready',
            params: { port: 18435 }
        })

        expect(state.getProxyPort('llamacpp')).toBe(18435)
    })
})
