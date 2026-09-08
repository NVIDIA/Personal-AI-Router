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

describe('SGLang engine registration', () => {
    it('is a known, enabled engine type spelled the same as its manifest', () => {
        expect(EngineTypes).toContain('sglang')
        expect(EnabledEngineTypes).toContain('sglang')
        expect(isEngineType('sglang')).toBe(true)
        // The engine-manager id and our EngineType are the same string, so every
        // name-translation helper is a pass-through. LM Studio is the only engine
        // whose two spellings differ.
        const manifest = JSON.parse(
            fs.readFileSync(path.join(MANIFEST_DIR, 'sglang.json'), 'utf8')
        ) as { engine: string; display_name: string }
        expect(manifest.engine).toBe('sglang')
        expect(EngineDisplayNames.sglang).toBe(manifest.display_name)
    })

    it('ships the SGLang documentation and install links', () => {
        expect(EngineDefaultLinks.sglang.docsUrl).toBe('https://docs.sglang.ai/')
        expect(EngineDefaultLinks.sglang.installUrl).toBe(
            'https://docs.sglang.ai/get_started/install.html'
        )
    })

    it('declares capabilities that match what SGLang can actually do', () => {
        const caps = EngineCapabilities.sglang
        // Linux only: SGLang targets Linux GPUs and its manifest ships Linux
        // platform blocks only.
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

    it('is offered in onboarding on Linux only, and never pre-selected', () => {
        expect(WELCOME_ENGINE_DEFAULT_SELECTED.sglang).toBe(false)
        expect(getWelcomeEngineCandidates(platformDisplayName('linux'))).toContain('sglang')
        expect(getWelcomeEngineCandidates(platformDisplayName('darwin'))).not.toContain('sglang')
        expect(getWelcomeEngineCandidates(platformDisplayName('win32'))).not.toContain('sglang')
    })

    it('renders a Hugging Face repo id as a readable model name', () => {
        // SGLang's `/v1/models` id is its `--served-model-name`, which defaults to
        // `--model-path` verbatim. Given a repo id that is a Hugging Face id, and
        // the shared formatter already renders those — the point of this case is
        // that the engine falls into that path rather than an engine-specific one.
        expect(formatModelDisplayName('Qwen/Qwen3-8B', 'sglang')).toBe(
            formatModelDisplayName('Qwen/Qwen3-8B', 'vllm')
        )
        expect(formatModelDisplayName('Qwen/Qwen3-8B', 'sglang')).not.toContain('/')
    })

    it('renders a local model directory path as its last segment', () => {
        // The other id shape `--model-path` takes: a directory on the node. The
        // Hugging Face formatter would strip the leading segment and turn the
        // rest into "models/my model", so a path gets its own rule — the last
        // segment, verbatim, because that is the name the operator typed.
        expect(formatModelDisplayName('/models/my-model', 'sglang')).toBe('my-model')
        expect(formatModelDisplayName('/srv/weights/llama-3.1-8b/', 'sglang')).toBe('llama-3.1-8b')
        // vLLM takes local paths too, and renders them the same way.
        expect(formatModelDisplayName('/models/my-model', 'vllm')).toBe('my-model')
    })

    it('does not pass off a truncated path segment as the directory name', () => {
        // The 256-char input cap slices from the right, taking the last segment
        // with it. Naming a truncated middle segment would look exactly like a
        // real answer, so a capped path keeps the long string instead.
        const long = `/mnt/nvme0/models/vendor/${'y'.repeat(250)}/my-model`
        const out = formatModelDisplayName(long, 'sglang')
        expect(out).not.toBe('my-model')
        expect(out.startsWith('/mnt/nvme0/models/vendor/')).toBe(true)
        // A path that fits the cap is unaffected.
        expect(formatModelDisplayName('/mnt/nvme0/models/my-model', 'sglang')).toBe('my-model')
    })

    it('leaves the engines that format their own names untouched', () => {
        // The path rule lives in the shared default branch, so this is the guard
        // that it did not reach past the engines with rules of their own.
        expect(formatModelDisplayName('llama3.2:latest', 'ollama')).toBe('llama3.2')
        expect(
            formatModelDisplayName(
                'lmstudio-community/Meta-Llama-3.1-8B-Instruct-GGUF/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf',
                'lm-studio'
            )
        ).toBe('Meta Llama 3.1 8B Instruct (Q4_K_M)')
    })
})
