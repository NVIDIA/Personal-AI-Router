// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { formatModelDisplayName, modelWireNameSuffix } from '@/ui/utils/format-model-display-name'

// A model row leads with a friendly name, and a model alias has to be written
// with the name the engine actually answers to. The suffix is how a row hands
// over that name without moving it into the lead.
describe('modelWireNameSuffix', () => {
    it('names the wire model a friendly row hides', () => {
        expect(
            modelWireNameSuffix(
                'lmstudio-community/Meta-Llama-3.1-8B-Instruct-GGUF/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf',
                formatModelDisplayName(
                    'lmstudio-community/Meta-Llama-3.1-8B-Instruct-GGUF/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf',
                    'lm-studio'
                )
            )
        ).toBe(
            ' (lmstudio-community/Meta-Llama-3.1-8B-Instruct-GGUF/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf)'
        )
    })

    // Nothing is being hidden, and the row already spends its width on the name.
    it('says nothing when the friendly name is already the wire name', () => {
        expect(
            modelWireNameSuffix(
                'deepseek-r1:8b',
                formatModelDisplayName('deepseek-r1:8b', 'ollama')
            )
        ).toBe('')
    })

    it('says nothing when the engine reported no model name', () => {
        expect(modelWireNameSuffix('', 'Some Model')).toBe('')
    })

    it('says nothing when the row has no friendly name to explain', () => {
        expect(modelWireNameSuffix('org/model-GGUF', '')).toBe('')
    })

    // The friendly name is what the row prints, so that is what gets compared.
    it('names the wire model even when only the spacing differs', () => {
        expect(modelWireNameSuffix('Qwen3.5-35B-A3B', 'Qwen3.5 35B A3B')).toBe(' (Qwen3.5-35B-A3B)')
    })
})
