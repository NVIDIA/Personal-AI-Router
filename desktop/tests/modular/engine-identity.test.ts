// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import {
    EnabledEngineTypes,
    EngineDefaultLinks,
    EngineDisplayNames,
    EngineManagerNames,
    EnginesListingModelsWhenStopped,
    EngineTypes
} from '@/shared/constants/engines'
import { engineManagerName, engineTypeFromManagerName, isEngineType } from '@/shared/utils/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { getWelcomeEngineCandidates, WELCOME_ENGINE_DEFAULT_SELECTED } from '@/ui/constants/welcome'
import { gatewayEndpointDisplayUrl } from '@/ui/utils/gateway-inference-paths'

describe('engine identity', () => {
    it('enables llama.cpp workflows on every platform', () => {
        expect(EngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm', 'custom'])
        expect(EnabledEngineTypes).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm', 'custom'])
        expect(getWelcomeEngineCandidates('Windows')).toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('MacOS')).toContain('llama-cpp')
        expect(getWelcomeEngineCandidates('Linux')).toContain('llama-cpp')
    })

    it('keeps Custom out of the installable welcome step', () => {
        // Custom is enabled (shipped) but never installable: it has no
        // installer to offer, so the welcome step must not list it.
        for (const os of ['Windows', 'MacOS', 'Linux'] as const) {
            expect(getWelcomeEngineCandidates(os)).not.toContain('custom')
        }
    })

    it('preselects every engine during onboarding', () => {
        expect(WELCOME_ENGINE_DEFAULT_SELECTED).toEqual({
            ollama: true,
            'lm-studio': true,
            'llama-cpp': true,
            vllm: true,
            custom: false
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
            modelOpsWhenStopped: true,
            hasModelSearchOnlyWhenRunning: false,
            hasDeleteModel: true,
            deleteBlockedWhileLoaded: true,
            hasInstall: ['linux', 'darwin'],
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

    it('maps the desktop id to the sole engine-manager wire id', () => {
        expect(EngineManagerNames['llama-cpp']).toBe('llamacpp')
        expect(engineManagerName('llama-cpp')).toBe('llamacpp')
        expect(engineTypeFromManagerName('llamacpp')).toBe('llama-cpp')
        expect(isEngineType('llama-cpp')).toBe(true)
        // Custom's wire name is its desktop id — it is the `modelsByEngine`
        // key for the custom bucket and there is no engine-manager manifest
        // to spell it differently.
        expect(EngineManagerNames.custom).toBe('custom')
        expect(engineManagerName('custom')).toBe('custom')
        expect(engineTypeFromManagerName('custom')).toBe('custom')
        expect(isEngineType('custom')).toBe(true)
    })

    it('provides complete display metadata and capabilities', () => {
        for (const engineType of EngineTypes) {
            expect(EngineDisplayNames[engineType]).toBeTruthy()
            expect(EngineCapabilities[engineType]).toBeDefined()
        }
        // Manifest engines link out to their vendor docs/installer. Custom has
        // no vendor — its whole point is user-configured endpoints, so empty
        // links are the honest value rather than an invented URL.
        for (const engineType of EngineTypes.filter(type => type !== 'custom')) {
            expect(EngineDefaultLinks[engineType].docsUrl).toMatch(/^https:\/\//)
            expect(EngineDefaultLinks[engineType].installUrl).toMatch(/^https:\/\//)
        }
        expect(EngineDefaultLinks.custom).toEqual({ docsUrl: '', installUrl: '' })

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
        // Custom fronts external endpoints: nothing to install, pull, delete,
        // restart, or hub-search — those controls must never render for it.
        expect(EngineCapabilities.custom).toMatchObject({
            hasExpiry: false,
            hasEject: false,
            hasInstall: [],
            hasEnginePort: false,
            hasInstallPath: false,
            hasProxyWebUI: false,
            hasPreferredNode: false,
            hasCrashAlert: false,
            hasModelSearchOnlyWhenRunning: false,
            modelOpsWhenStopped: false,
            hasDeleteModel: false
        })
        expect(EngineCapabilities.custom.engineHub).toBeUndefined()
        expect(EngineDisplayNames.custom).toBe('Custom')
        expect(gatewayEndpointDisplayUrl(8080, 'llama-cpp')).toBe('http://127.0.0.1:8080/v1')
    })
})
