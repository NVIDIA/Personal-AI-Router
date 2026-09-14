// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { deleteModelAction } from '@/electron/service-bridge/model-delete-action'

/**
 * The delete that could not delete. A locally built MLX model is advertised by
 * absolute path and is not in the Hugging Face cache, so the cache delete fails
 * with `Cache directory not found` and the model stays in the list forever --
 * the models-directory scan keeps finding it on disk.
 */
describe('deleteModelAction', () => {
    it('sends an MLX path model to the delete that removes files', () => {
        expect(deleteModelAction('mlx', '/Users/me/models/Qwen3.8-27B-3bit')).toBe(
            'delete_model_path'
        )
    })

    it('sends an MLX repo id to the cache delete', () => {
        expect(deleteModelAction('mlx', 'mlx-community/Llama-3.2-1B-Instruct-4bit')).toBe(
            'delete_model'
        )
    })

    it('leaves other engines on the cache delete, even for a path', () => {
        expect(deleteModelAction('lmstudio', '/Users/me/models/thing')).toBe('delete_model')
        expect(deleteModelAction('ollama', 'llama3')).toBe('delete_model')
    })
})
