// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    EnabledEngineTypes,
    EngineDefaultLinks,
    EngineDisplayNames,
    EngineManagerNames,
    EngineTypes
} from '@/shared/constants/engines'
import { engineManagerName, engineTypeFromManagerName, isEngineType } from '@/shared/utils/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { getWelcomeEngineCandidates, WELCOME_ENGINE_DEFAULT_SELECTED } from '@/ui/constants/welcome'

describe('engine identity', () => {
    it('declares llama.cpp without enabling its desktop workflows', () => {
        expect(EngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp'])
        expect(EnabledEngineTypes).toEqual(['ollama', 'lm-studio'])
        expect(getWelcomeEngineCandidates('Windows')).not.toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('MacOS')).not.toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('Linux')).not.toContain('llama-cpp')
        expect(WELCOME_ENGINE_DEFAULT_SELECTED['llama-cpp']).toBe(false)
    })

    it('maps the desktop id to the sole engine-manager wire id', () => {
        expect(EngineManagerNames['llama-cpp']).toBe('llamacpp')
        expect(engineManagerName('llama-cpp')).toBe('llamacpp')
        expect(engineTypeFromManagerName('llamacpp')).toBe('llama-cpp')
        expect(isEngineType('llama-cpp')).toBe(true)
    })

    it('provides complete display metadata and capabilities', () => {
        for (const engineType of EngineTypes) {
            expect(EngineDisplayNames[engineType]).toBeTruthy()
            expect(EngineDefaultLinks[engineType].docsUrl).toMatch(/^https:\/\//)
            expect(EngineDefaultLinks[engineType].installUrl).toMatch(/^https:\/\//)
            expect(EngineCapabilities[engineType]).toBeDefined()
        }

        expect(EngineDisplayNames['llama-cpp']).toBe('llama.cpp')
        expect(EngineCapabilities['llama-cpp']).toMatchObject({
            hasExpiry: false,
            hasEject: true,
            hasInstall: ['win32', 'darwin', 'linux'],
            hasEnginePort: true,
            hasInstallPath: false,
            hasProxyWebUI: false,
            hasPreferredNode: false,
            hasCrashAlert: false,
            hasModelSearchOnlyWhenRunning: true,
            modelOpsWhenStopped: false,
            hasDeleteModel: false,
            engineHub: { label: 'llama.cpp' }
        })
    })
})
