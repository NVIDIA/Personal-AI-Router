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
        expect(EngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm', 'mtplx'])
        expect(EnabledEngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm', 'mtplx'])
        expect(getWelcomeEngineCandidates('Windows')).toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('MacOS')).toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('Linux')).toContain('llama-cpp')
    })

    it('preselects every engine during onboarding', () => {
        expect(WELCOME_ENGINE_DEFAULT_SELECTED).toEqual({
            ollama: true,
            'lm-studio': true,
            'llama-cpp': true,
            vllm: true
        })
    })

    it('offers vLLM on Linux and macOS, not Windows', () => {
        expect(getWelcomeEngineCandidates('Linux')).toContain('vllm')
        expect(getWelcomeEngineCandidates('Windows')).not.toContain('vllm')
        expect(getWelcomeEngineCandidates('MacOS')).toContain('vllm')
    })

    it('maps vLLM to its engine-manager wire id and one-model-per-process capabilities', () => {
        expect(EngineManagerNames.vllm).toBe('vllm')
        expect(engineManagerName('vllm')).toBe('vllm')
        expect(engineTypeFromManagerName('vllm')).toBe('vllm')
        expect(isEngineType('vllm')).toBe(true)
        expect(EngineDisplayNames.vllm).toBe('vLLM')
        // vLLM cannot unload a model while running, so Eject stops the engine; it
        // downloads from and deletes in the Hugging Face cache, except the model it
        // is serving, and its model list is on disk, so it works while stopped.
        expect(EngineCapabilities.vllm).toMatchObject({
            hasEject: true,
            hasModelSearchOnlyWhenRunning: false,
            hasDeleteModel: true,
            deleteBlockedWhileLoaded: true,
            hasInstall: ['linux', 'darwin'],
            hasEnginePort: true,
            engineHub: { label: 'vLLM' }
        })
    })

    it('maps MTPLX to its engine-manager wire id and one-pack-per-process capabilities', () => {
        expect(EngineManagerNames.mtplx).toBe('mtplx')
        expect(engineManagerName('mtplx')).toBe('mtplx')
        expect(engineTypeFromManagerName('mtplx')).toBe('mtplx')
        expect(isEngineType('mtplx')).toBe(true)
        expect(EngineDisplayNames.mtplx).toBe('MTPLX')
        // MTPLX serves one pack per process, the way vLLM serves one model: Eject
        // stops the engine and Load starts it again on the chosen pack. Its manifest
        // declares only darwin/arm64 and declares no installer, so PAIR drives an
        // engine the user installed and never offers to install one.
        expect(EngineCapabilities.mtplx).toMatchObject({
            hasEject: true,
            hasModelSearchOnlyWhenRunning: false,
            hasDeleteModel: true,
            deleteBlockedWhileLoaded: true,
            loadRestartsEngine: true,
            hasInstall: [],
            hasEnginePort: true,
            engineHub: { label: 'MTPLX' }
        })
    })

    it('keeps MTPLX out of onboarding because nothing is installable for it', () => {
        // A source with no installable platform is dropped from the onboarding list
        // rather than shown as a row the user cannot act on, and nothing is
        // preselected for it.
        for (const platform of ['Windows', 'MacOS', 'Linux'] as const) {
            expect(getWelcomeEngineCandidates(platform)).not.toContain('mtplx')
        }
        expect(WELCOME_ENGINE_DEFAULT_SELECTED.mtplx).toBeUndefined()
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
            hasDeleteModel: true,
            engineHub: { label: 'llama.cpp' }
        })
        expect(gatewayEndpointDisplayUrl(8080, 'llama-cpp')).toBe('http://127.0.0.1:8080/v1')
    })
})
