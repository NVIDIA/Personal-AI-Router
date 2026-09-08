// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const HF_PREFIX = /^(https?:\/\/)?(hf\.co|huggingface\.co)\//
const HUB_REGISTRY_ARROW = ' \u2192 '

/**
 * Universal display formatter for model names. Display-only -- never
 * send the return value to the server; always use the raw model id for
 * wire operations.
 *
 * When engineType is unknown or omitted, applies generic HF/hub
 * heuristics only. Does not truncate; callers handle layout constraints
 * themselves.
 */
/** Upper bound on the length of any untrusted name fed to a formatter. Model /
 *  workload names arrive over the wire from cluster peers and engine model
 *  lists; capping length keeps every formatter linear-time on hostile input. */
const MAX_FORMAT_INPUT_CHARS = 256

export function formatModelDisplayName(name: string, engineType?: string | null): string {
    if (!name) {
        return name
    }

    const capped = name.length > MAX_FORMAT_INPUT_CHARS
    if (capped) {
        name = name.slice(0, MAX_FORMAT_INPUT_CHARS)
    }

    if (name.includes(HUB_REGISTRY_ARROW)) {
        return formatHubRegistryDisplayLabel(name)
    }

    let formatted = formatHuggingFaceModelUrl(name)
    const isHfModel = HF_PREFIX.test(formatted)

    if (isHfModel) {
        formatted = formatHuggingFaceModelName(formatted)
    }

    switch (engineType) {
        case 'lm-studio':
            return isHfModel ? formatted : formatLmStudioModelName(formatted)

        case 'ollama':
            return formatOllamaModelName(formatted)

        // vLLM and SGLang serve whatever their `--model` / `--model-path` was:
        // either a Hugging Face repo id verbatim (Qwen/Qwen3-8B), which the
        // shared Hugging Face formatter already renders, or the path of a local
        // model directory, which it would mangle into "models/my model".
        // Neither needs engine-specific rules beyond telling those two apart.
        default:
            if (isPathLikeModelId(formatted)) {
                // Naming the last segment is only truthful on a name the cap
                // left intact. The cap slices from the right, which is exactly
                // where a path keeps its last segment, so on a capped path the
                // rule would hand back a truncated *middle* segment as
                // confidently as a real directory name. Hand back the truncated
                // path instead — it reads as unfinished, which it is.
                return capped ? formatted : formatLocalModelPath(formatted)
            }
            if (formatted.includes('/')) {
                return formatHuggingFaceModelName(formatted)
            }
            return formatted
    }
}

/**
 * A local model directory rather than a Hugging Face repo id. The leading slash
 * is the whole test: a repo id is always `org/repo` with no leading separator,
 * so nothing that would otherwise reach the Hugging Face formatter is captured
 * here.
 */
function isPathLikeModelId(name: string): boolean {
    return name.startsWith('/')
}

/**
 * The last segment of a local model path — `/models/my-model` renders as
 * `my-model`. Kept verbatim rather than humanised the way a repo id is: a
 * directory name is what the operator typed into "Model to serve", and it is
 * only recognisable if we hand it back unchanged.
 */
function formatLocalModelPath(modelPath: string): string {
    const segments = modelPath.split('/').filter(segment => segment.length > 0)
    return segments.length > 0 ? segments[segments.length - 1] : modelPath
}

/**
 * Format an LM Studio / llama.cpp GGUF model path into a readable name.
 * "lmstudio-community/Meta-Llama-3.1-8B-Instruct-GGUF/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf"
 * -> "Meta Llama 3.1 8B Instruct (Q4_K_M)"
 */
function formatLmStudioModelName(modelPath: string): string {
    const segments = modelPath.split('/')
    let filename = segments[segments.length - 1]

    filename = filename.replace(/\.gguf$/i, '')

    // `_` is already a `\w` character, so the old `(?:_\w+)*` tail was redundant
    // and ambiguous — it made this pattern backtrack exponentially on names like
    // `m-Q1` + `_a`.repeat(n) + `!`. `\w*` alone matches the same quant suffixes
    // in linear time.
    const quantMatch = filename.match(/[-_](Q\d\w*)$/i)
    const quantization = quantMatch ? quantMatch[1] : ''
    if (quantMatch) {
        filename = filename.slice(0, quantMatch.index)
    }

    const name = filename.replace(/[-_]/g, ' ').replace(/\s+/g, ' ').trim()

    return quantization ? `${name} (${quantization})` : name
}

/**
 * Format a HuggingFace model ID into a readable name.
 * "hf.co/unsloth/Qwen3.5-35B-A3B-GGUF" -> "Qwen3.5 35B A3B"
 * "bartowski/Llama-3.2-1B-Instruct-GGUF" -> "Llama 3.2 1B Instruct"
 */
function formatHuggingFaceModelName(modelId: string): string {
    let name = modelId.replace(HF_PREFIX, '')

    const segments = name.split('/')
    name = segments.length > 1 ? segments.slice(1).join('/') : segments[0]

    name = name
        .replace(/-GGUF(:[^\s/]*)?$/i, '')
        .replace(/[-_]/g, ' ')
        .replace(/\s+/g, ' ')
        .trim()

    return name || modelId
}

function formatHuggingFaceModelUrl(modelUrl: string): string {
    return modelUrl.replace(/^(https:\/\/hf\.co|https:\/\/huggingface\.co)\//, '')
}

/**
 * `org/repo -> weight` from on-demand Hub search -- show repo + org only.
 */
function formatHubRegistryDisplayLabel(label: string): string {
    const arrowAt = label.indexOf(HUB_REGISTRY_ARROW)
    if (arrowAt === -1) {
        return label.includes('/') ? formatHuggingFaceModelName(label) : label
    }
    const repoPart = label.slice(0, arrowAt).trim()
    if (!repoPart) {
        return label.includes('/') ? formatHuggingFaceModelName(label) : label
    }
    const segments = repoPart.split('/')
    const author = segments[0] ?? ''
    const repoTail = segments.slice(1).join('/')
    const humanRepo = repoTail
        .replace(/-GGUF$/i, '')
        .replace(/[-_]/g, ' ')
        .replace(/\s+/g, ' ')
        .trim()
    if (author && humanRepo) return `${humanRepo} (${author})`
    return humanRepo || author || repoPart
}

function formatOllamaModelName(name: string): string {
    return name.replace(/:latest$/i, '')
}
