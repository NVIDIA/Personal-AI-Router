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
import { gatewayEndpointDisplayUrl } from '@/ui/utils/gateway-inference-paths'

describe('engine identity', () => {
    it('enables llama.cpp workflows on every platform', () => {
        expect(EngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'colibri'])
        expect(EnabledEngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'colibri'])
        expect(getWelcomeEngineCandidates('Windows')).toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('MacOS')).toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('Linux')).toContain('llama-cpp')
    })

    // Colibri is offered but not preselected: its models are tens to hundreds of
    // gigabytes and its launcher needs Python.
    it('preselects every engine but Colibri during onboarding', () => {
        expect(WELCOME_ENGINE_DEFAULT_SELECTED).toEqual({
            ollama: true,
            'lm-studio': true,
            'llama-cpp': true,
            colibri: false
        })
        expect(getWelcomeEngineCandidates('Windows')).toContain('colibri')
    })

    it('integrates Colibri as an engine with a searchable Hugging Face hub', () => {
        expect(EngineManagerNames.colibri).toBe('colibri')
        expect(engineTypeFromManagerName('colibri')).toBe('colibri')
        expect(EngineDisplayNames.colibri).toBe('Colibri')
        expect(EngineCapabilities.colibri).toMatchObject({
            hasEject: false,
            hasDeleteModel: true,
            hasModelSearchOnlyWhenRunning: false,
            engineHub: { label: 'Colibri', searchable: true }
        })
        expect(EngineCapabilities['llama-cpp'].engineHub?.searchable).toBe(true)
        expect(EngineCapabilities.ollama.engineHub?.searchable).toBeFalsy()
        expect(EngineCapabilities['lm-studio'].engineHub?.searchable).toBeFalsy()
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
            hasDeleteModel: true,
            engineHub: { label: 'llama.cpp' }
        })
        expect(gatewayEndpointDisplayUrl(8080, 'llama-cpp')).toBe('http://127.0.0.1:8080/v1')
    })
})
