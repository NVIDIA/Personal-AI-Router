// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

/**
 * Which delete action an engine needs for a given model.
 *
 * MLX lists two kinds of model. A Hugging Face repo id lives in the shared cache
 * and is deleted from it. A locally built model -- a quantization, say -- never
 * enters the cache, has no repo id, and is advertised by absolute path; it is
 * deleted from disk instead.
 *
 * Sending a path to the cache delete asks `hf` to remove a repo that was never
 * cached:
 *
 *	Failed to delete /Users/…/models/Qwen3.8-27B-3bit:
 *	  Error: Cache directory not found: /Users/…/.cache/huggingface/hub
 *
 * and the model stays in the list forever, because the models-directory scan
 * keeps finding it on disk. There is no other way to remove it.
 *
 * `nvpair-engine-manager` makes the same choice for callers that go through its
 * own model ops (the remote path does). This is the local path, which names the
 * action directly and so has to decide for itself.
 *
 * Its own module, rather than a private function in the supervisor, only so a
 * test can reach it without pulling the Electron module graph in behind it.
 */
export function deleteModelAction(engineManagerEngine: string, model: string): string {
    return engineManagerEngine === 'mlx' && model.startsWith('/')
        ? 'delete_model_path'
        : 'delete_model'
}
