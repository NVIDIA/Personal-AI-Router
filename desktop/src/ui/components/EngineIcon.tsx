// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { CSSProperties } from 'react'
import { type EngineType } from '@/shared/types/engines'
import ollamaIcon from '@/ui/assets/engine-icons/ollama.png?inline'
import lmStudioIcon from '@/ui/assets/engine-icons/lm-studio.png?inline'

export default function EngineIcon({ type, size = 32 }: { type: EngineType; size?: number }) {
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

    if (type === 'mlx') {
        // Drawn inline rather than shipped as an asset: MLX publishes no icon
        // for third parties to bundle, and a lettermark states what the engine
        // is without borrowing someone's mark. Same white rounded square as the
        // other two so the row reads as one set.
        return (
            <div style={containerStyle}>
                <svg viewBox="0 0 64 64" style={imgStyle} role="img" aria-label="MLX">
                    <rect width="64" height="64" fill="#fff" />
                    <text
                        x="32"
                        y="32"
                        textAnchor="middle"
                        dominantBaseline="central"
                        fontFamily="system-ui, -apple-system, sans-serif"
                        fontSize="20"
                        fontWeight="700"
                        letterSpacing="-0.5"
                        fill="#111"
                    >
                        MLX
                    </text>
                </svg>
            </div>
        )
    }

    return null
}
