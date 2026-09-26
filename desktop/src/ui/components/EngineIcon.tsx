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

    switch (type) {
        case 'ollama':
            return (
                <div style={containerStyle}>
                    <img src={ollamaIcon} alt="Ollama" style={imgStyle} />
                </div>
            )
        case 'lm-studio':
            imgStyle.objectFit = 'cover'
            return (
                <div style={containerStyle}>
                    <img src={lmStudioIcon} alt="LM Studio" style={imgStyle} />
                </div>
            )
        case 'llamacpp':
            return (
                <div style={containerStyle} title="llama.cpp">
                    <span
                        style={{
                            display: 'flex',
                            alignItems: 'center',
                            justifyContent: 'center',
                            width: '100%',
                            height: '100%',
                            fontSize: size * 0.32,
                            fontWeight: 700,
                            color: '#111',
                            lineHeight: 1
                        }}
                    >
                        cpp
                    </span>
                </div>
            )
    }
}
