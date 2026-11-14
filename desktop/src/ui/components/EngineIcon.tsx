// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { CSSProperties } from 'react'
import { type EngineType } from '@/shared/types/engines'
import { EngineDisplayNames } from '@/shared/constants/engines'
import ollamaIcon from '@/ui/assets/engine-icons/ollama.png?inline'
import lmStudioIcon from '@/ui/assets/engine-icons/lm-studio.png?inline'
import llamaCppIcon from '@/ui/assets/engine-icons/llama-cpp.svg?inline'
import vllmIcon from '@/ui/assets/engine-icons/vllm.svg?inline'

// Custom has no icon: it fronts user-configured endpoints, not a packaged
// engine, and its surfaces use the Custom endpoints vocabulary instead.
const engineIcons: Partial<Record<EngineType, string>> = {
    ollama: ollamaIcon,
    'lm-studio': lmStudioIcon,
    'llama-cpp': llamaCppIcon,
    vllm: vllmIcon
}

export default function EngineIcon({ type, size = 32 }: { type: EngineType; size?: number }) {
    const icon = engineIcons[type]
    if (!icon) return null

    const dimension = `${size}px`
    const imgStyle: CSSProperties = {
        width: '100%',
        height: '100%',
        objectFit: type === 'lm-studio' ? 'cover' : 'contain'
    }
    const containerStyle: CSSProperties = {
        width: dimension,
        minWidth: dimension,
        maxWidth: dimension,
        height: dimension,
        minHeight: dimension,
        maxHeight: dimension,
        backgroundColor: '#fff',
        borderRadius: '25%',
        overflow: 'hidden'
    }

    return (
        <div style={containerStyle}>
            <img src={icon} alt={EngineDisplayNames[type]} style={imgStyle} />
        </div>
    )
}
