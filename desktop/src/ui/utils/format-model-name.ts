// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

const HF_CO_PREFIX = /^https:\/\/huggingface\.co\//i

function stripHfCoPrefix(name: string): string {
    return name.replace(HF_CO_PREFIX, '')
}

/**
 * Display name without redundant `author/` when it matches the HuggingFace org/user segment.
 */
export function formatModelName(name: string, author: string): string {
    let result = stripHfCoPrefix(name)
    const a = author.trim()

    if (!a) {
        return result
    }

    if (result.toLowerCase().startsWith(`${a.toLowerCase()}/`)) {
        result = result.slice(a.length + 1)
    }

    return result
}
