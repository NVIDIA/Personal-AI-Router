// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    EnabledEngineTypes,
    EngineDefaultLinks,
    EngineDisplayNames,
    EngineManagerNames,
    EnginesListingModelsWhenStopped,
    EnginesWithIdleTimeout,
    EngineTypes,
    IdleTimeoutExpiries
} from '@/shared/constants/engines'
import { engineManagerName, engineTypeFromManagerName, isEngineType } from '@/shared/utils/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { getWelcomeEngineCandidates, WELCOME_ENGINE_DEFAULT_SELECTED } from '@/ui/constants/welcome'
import { gatewayEndpointDisplayUrl } from '@/ui/utils/gateway-inference-paths'

describe('engine identity', () => {
    it('enables llama.cpp workflows on every platform', () => {
        expect(EngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm'])
        expect(EnabledEngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm'])
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

    it('offers vLLM only where it can be installed', () => {
        expect(getWelcomeEngineCandidates('Linux')).toContain('vllm')
        expect(getWelcomeEngineCandidates('Windows')).not.toContain('vllm')
        expect(getWelcomeEngineCandidates('MacOS')).not.toContain('vllm')
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
            modelOpsWhenStopped: true,
            hasModelSearchOnlyWhenRunning: false,
            hasDeleteModel: true,
            deleteBlockedWhileLoaded: true,
            hasPullModel: true,
            hubSearch: true,
            hasInstall: ['linux'],
            hasEnginePort: true,
            engineHub: { label: 'vLLM' }
        })
    })

    it('keeps the capability and the supervisor agreeing on which model lists survive a stop', () => {
        for (const engineType of EngineTypes) {
            expect(EngineCapabilities[engineType].modelOpsWhenStopped).toBe(
                EnginesListingModelsWhenStopped.includes(engineType)
            )
        }
    })

    it('keeps the expiry capability and the idle-limit list agreeing', () => {
        for (const engineType of EngineTypes) {
            expect(EngineCapabilities[engineType].hasExpiry).toBe(
                EnginesWithIdleTimeout.includes(engineType)
            )
        }
        expect(EngineCapabilities.vllm.expiryOptions).toEqual(IdleTimeoutExpiries)
        expect(EngineCapabilities.vllm.expiryText?.('-1')).toBe('Stays loaded')
        expect(EngineCapabilities.vllm.expiryText?.('10m')).toBe('Unloads after 10m idle')
    })

    it('offers Add model to every engine, vLLM included', () => {
        for (const engineType of EngineTypes) {
            expect(EngineCapabilities[engineType].hasPullModel).toBe(true)
        }
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
        expect(gatewayEndpointDisplayUrl(8080, 'llama-cpp')).toBe('http://127.0.0.1:8080')
    })
})
