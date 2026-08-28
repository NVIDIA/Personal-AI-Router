// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { Divider, Text } from '@nvidia/foundations-react-core'
import type { BackendInfo } from '@/ui/types/engine-info'
import { ModelManager } from './ModelManager'

type EditState = {
    serverPort: string
    proxyPort: string
    serverLoading: boolean
    proxyLoading: boolean
}

export function ModelSection({
    backend,
    nodeId,
    disabled = false
}: {
    backend: BackendInfo
    nodeId: string
    /** Remote node: render the model list read-only (no pull/load/delete). */
    disabled?: boolean
}) {
    const [edit, setEdit] = useState<EditState>({
        serverPort: String(backend.port ?? ''),
        proxyPort: String(backend.proxyPort ?? ''),
        serverLoading: false,
        proxyLoading: false
    })

    useEffect(() => {
        setEdit(prev => ({ ...prev, serverPort: String(backend.port ?? '') }))
    }, [backend.port])

    useEffect(() => {
        setEdit(prev => ({ ...prev, proxyPort: String(backend.proxyPort ?? '') }))
    }, [backend.proxyPort])

    const isTransitioning =
        backend.processStatus === 'installing' ||
        backend.processStatus === 'uninstalling' ||
        backend.processStatus === 'starting' ||
        backend.processStatus === 'stopping'

    const anyLoading = edit.serverLoading || edit.proxyLoading || isTransitioning || disabled

    return (
        <details className="pair-accordion translucent-bg-accordion">
            <summary className="pair-accordion-summary">
                <Text kind="body/semibold/sm">Models</Text>
            </summary>
            <div
                style={{
                    opacity: anyLoading ? 0.5 : 1,
                    pointerEvents: anyLoading ? 'none' : 'auto'
                }}
            >
                <Divider />
                <div className="p-3">
                    <ModelManager backend={backend} nodeId={nodeId} />
                </div>
            </div>
        </details>
    )
}
