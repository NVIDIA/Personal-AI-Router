// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { CSSProperties } from 'react'
import { type EngineType } from '@/shared/types/engines'
import type { WorkloadEngine } from '@/shared/types/workloads'
import { EngineDisplayNames } from '@/shared/constants/engines'
import { isWorkloadEngineClass } from '@/shared/utils/workloads'
import { CircleHelp, Route } from '@/ui/components/icons'
import ollamaIcon from '@/ui/assets/engine-icons/ollama.png?inline'
import lmStudioIcon from '@/ui/assets/engine-icons/lm-studio.png?inline'
import llamaCppIcon from '@/ui/assets/engine-icons/llama-cpp.svg?inline'
import vllmIcon from '@/ui/assets/engine-icons/vllm.svg?inline'

const engineIcons: Record<EngineType, string> = {
    ollama: ollamaIcon,
    'lm-studio': lmStudioIcon,
    'llama-cpp': llamaCppIcon,
    vllm: vllmIcon
}

// A job's class has no logo to show, so it takes the space one would with the
// app's own mark: `Route` for work the cluster router placed on another machine,
// a question mark for a name no engine manager in this build uses.
const classMarks = {
    cluster: Route,
    unknown: CircleHelp
} as const

/** `type` widens `EngineType` with the two classes a routed job can carry. */
export default function EngineIcon({ type, size = 32 }: { type: WorkloadEngine; size?: number }) {
    const dimension = `${size}px`
    const boxStyle: CSSProperties = {
        width: dimension,
        minWidth: dimension,
        maxWidth: dimension,
        height: dimension,
        minHeight: dimension,
        maxHeight: dimension
    }

    if (isWorkloadEngineClass(type)) {
        const Mark = classMarks[type]
        return (
            <div
                style={{
                    ...boxStyle,
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'center'
                }}
                className="text-subtle-color"
            >
                <Mark size={size} />
            </div>
        )
    }

    if (!Object.hasOwn(engineIcons, type)) return null
    const icon = engineIcons[type]

    const imgStyle: CSSProperties = {
        width: '100%',
        height: '100%',
        objectFit: type === 'lm-studio' ? 'cover' : 'contain'
    }

    return (
        <div
            style={{
                ...boxStyle,
                backgroundColor: '#fff',
                borderRadius: '25%',
                overflow: 'hidden'
            }}
        >
            <img src={icon} alt={EngineDisplayNames[type]} style={imgStyle} />
        </div>
    )
}
