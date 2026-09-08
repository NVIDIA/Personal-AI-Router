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

    // vLLM's own logo is not redistributable here, so its tile is drawn rather
    // than shipped: a wordmark on the project's colours, in the same rounded
    // square the other engines use so the row stays visually even.
    if (type === 'vllm') {
        return (
            <div style={{ ...containerStyle, backgroundColor: '#30a2ff' }}>
                <svg
                    viewBox="0 0 32 32"
                    width="100%"
                    height="100%"
                    role="img"
                    aria-label="vLLM"
                    focusable="false"
                >
                    <text
                        x="16"
                        y="21"
                        textAnchor="middle"
                        fontFamily="system-ui, -apple-system, Segoe UI, sans-serif"
                        fontSize="13"
                        fontWeight="700"
                        fill="#ffffff"
                    >
                        vL
                    </text>
                </svg>
            </div>
        )
    }

    // SGLang's own logo is not redistributable here either, so its tile is drawn
    // the same way vLLM's is. The colour is deliberately across the wheel from
    // vLLM's blue: these two engines sit next to each other in the backend list
    // and the tile is the only thing telling them apart at a glance, and
    // orange-vs-blue survives the common colour-vision deficiencies that would
    // collapse a violet-vs-blue pair. #c2410c carries white text at 5.2:1, past
    // the 4.5:1 contrast floor.
    if (type === 'sglang') {
        return (
            <div style={{ ...containerStyle, backgroundColor: '#c2410c' }}>
                <svg
                    viewBox="0 0 32 32"
                    width="100%"
                    height="100%"
                    role="img"
                    aria-label="SGLang"
                    focusable="false"
                >
                    <text
                        x="16"
                        y="21"
                        textAnchor="middle"
                        fontFamily="system-ui, -apple-system, Segoe UI, sans-serif"
                        fontSize="13"
                        fontWeight="700"
                        fill="#ffffff"
                    >
                        SG
                    </text>
                </svg>
            </div>
        )
    }

    return null
}
