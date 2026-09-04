// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import fs from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import {
    EngineDefaultLinks,
    EngineDisplayNames,
    EngineTypes,
    EnabledEngineTypes
} from '@/shared/constants/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { WELCOME_ENGINE_DEFAULT_SELECTED, getWelcomeEngineCandidates } from '@/ui/constants/welcome'
import { isEngineType } from '@/shared/utils/engines'
import { platformDisplayName } from '@/shared/utils/platform'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'

const MANIFEST_DIR = path.resolve(process.cwd(), '../services/nvpair-engine-manager/manifests')

describe('vLLM engine registration', () => {
    it('is a known, enabled engine type spelled the same as its manifest', () => {
        expect(EngineTypes).toContain('vllm')
        expect(EnabledEngineTypes).toContain('vllm')
        expect(isEngineType('vllm')).toBe(true)
        // The engine-manager id and our EngineType are the same string, so every
        // name-translation helper is a pass-through. LM Studio is the only engine
        // whose two spellings differ.
        const manifest = JSON.parse(
            fs.readFileSync(path.join(MANIFEST_DIR, 'vllm.json'), 'utf8')
        ) as { engine: string; display_name: string }
        expect(manifest.engine).toBe('vllm')
        expect(EngineDisplayNames.vllm).toBe(manifest.display_name)
    })

    it('ships the vLLM documentation and install links', () => {
        expect(EngineDefaultLinks.vllm.docsUrl).toBe('https://docs.vllm.ai/')
        expect(EngineDefaultLinks.vllm.installUrl).toBe(
            'https://docs.vllm.ai/en/latest/getting_started/installation/'
        )
    })

    it('declares capabilities that match what vLLM can actually do', () => {
        const caps = EngineCapabilities.vllm
        // Linux only: vLLM publishes no Windows build and no macOS GPU build, and
        // its manifest ships Linux platform blocks only.
        expect(caps.hasInstall).toEqual(['linux'])
        expect(caps.hasEnginePort).toBe(true)
        // One model per process, chosen before start and resident for the life of
        // the process: nothing to eject, delete, or expire.
        expect(caps.hasEject).toBe(false)
        expect(caps.hasDeleteModel).toBe(false)
        expect(caps.hasExpiry).toBe(false)
        expect(caps.modelOpsWhenStopped).toBe(false)
        // The served model is a start-time setting, which is what gives the
        // engine settings its "Model to serve" field.
        expect(caps.hasServedModel).toBe(true)
        // No public catalog is wired up, so no hub source selector is offered.
        expect(caps.engineHub).toBeUndefined()
    })

    it('is the only engine that declares a served model', () => {
        const declaring = EngineTypes.filter(type => EngineCapabilities[type].hasServedModel)
        expect(declaring).toEqual(['vllm'])
    })

    it('is offered in onboarding on Linux only, and never pre-selected', () => {
        expect(WELCOME_ENGINE_DEFAULT_SELECTED.vllm).toBe(false)
        expect(getWelcomeEngineCandidates(platformDisplayName('linux'))).toContain('vllm')
        expect(getWelcomeEngineCandidates(platformDisplayName('darwin'))).not.toContain('vllm')
        expect(getWelcomeEngineCandidates(platformDisplayName('win32'))).not.toContain('vllm')
    })

    it('renders a Hugging Face repo id as a readable model name', () => {
        // vLLM's model ids are Hugging Face repo ids, which the shared formatter
        // already handles — the point of this case is that the engine falls into
        // that path rather than an engine-specific one.
        expect(formatModelDisplayName('Qwen/Qwen3-8B', 'vllm')).toBe(
            formatModelDisplayName('Qwen/Qwen3-8B', 'lm-studio')
        )
        expect(formatModelDisplayName('Qwen/Qwen3-8B', 'vllm')).not.toContain('/')
    })
})
