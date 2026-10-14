// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { CSSProperties } from 'react'
import { type WorkloadEngine } from '@/shared/types/workloads'
import { engineBadge } from '@/ui/utils/engine-badge'
import ollamaIcon from '@/ui/assets/engine-icons/ollama.png?inline'
import lmStudioIcon from '@/ui/assets/engine-icons/lm-studio.png?inline'
import llamaCppIcon from '@/ui/assets/engine-icons/llama-cpp.svg?inline'

/**
 * Engine badge. PAIR's engines get their logo; any other identifier (a
 * third-party producer on the workload manager's local ingress, for example
 * `vllm` or `comfyui`) gets a monogram tile so its card still has a badge.
 */
export default function EngineIcon({ type, size = 32 }: { type: WorkloadEngine; size?: number }) {
    const dimension = `${size}px`
    const imgStyle: CSSProperties = { width: '100%', height: '100%', objectFit: 'contain' }
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

    if (type === 'ollama') {
        return (
            <div style={containerStyle}>
                <img src={ollamaIcon} alt="Ollama" style={imgStyle} />
            </div>
        )
    }

    if (type === 'lm-studio') {
        imgStyle.objectFit = 'cover'

        return (
            <div style={containerStyle}>
                <img src={lmStudioIcon} alt="LM Studio" style={imgStyle} />
            </div>
        )
    }

    if (type === 'llama-cpp') {
        return (
            <div style={containerStyle}>
                <img src={llamaCppIcon} alt="llama.cpp" style={imgStyle} />
            </div>
        )
    }

    const { monogram, name } = engineBadge(type)
    const monogramStyle: CSSProperties = {
        ...containerStyle,
        backgroundColor: '#3a3a3a',
        color: '#e6e6e6',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        fontSize: `${Math.max(8, Math.round(size * 0.42))}px`,
        fontWeight: 600,
        letterSpacing: '0.02em'
    }

    return (
        <div style={monogramStyle} role="img" title={name} aria-label={name}>
            {monogram}
        </div>
    )
}
