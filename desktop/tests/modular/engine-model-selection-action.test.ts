// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'
import { afterEach, expect, it } from 'vitest'
import NodeEnginesInline from '@/ui/components/NodeList/NodeEnginesInline'
import type { EngineStatusData, EngineType } from '@/shared/types/engines'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'

const nodeId = 'selection-action-self'

afterEach(() => {
    useConnectionStore.setState({ selfId: null })
    useEngineStatusStore.setState({ statusByNode: new Map() })
    useOverviewUiStore.setState({ focusNodeId: null })
    Reflect.deleteProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT')
})

it('offers model selection and an enabled Stop switch before vLLM serves a model', async () => {
    Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
        value: true,
        configurable: true
    })
    const status: EngineStatusData = {
        engineType: 'vllm',
        nodeId,
        processStatus: 'awaiting-model',
        enginePort: 8001,
        proxyPort: 8000
    }
    useConnectionStore.setState({ selfId: nodeId })
    useEngineStatusStore.setState({
        statusByNode: new Map([[nodeId, new Map<EngineType, EngineStatusData>([['vllm', status]])]])
    })
    const host = document.createElement('div')
    document.body.append(host)
    const root = createRoot(host)
    try {
        await act(() => root.render(createElement(NodeEnginesInline, { nodeId })))
        expect(host.querySelector('[aria-label="Stop vLLM"]')).not.toBeNull()
        expect(host.querySelector('[aria-label="Start vLLM"]')).toBeNull()
        const choose = [...host.querySelectorAll('button')].find(
            button => button.textContent?.trim() === 'Select a model'
        )
        expect(choose).toBeDefined()
        await act(() => choose?.click())
        expect(useOverviewUiStore.getState().focusNodeId).toBe(nodeId)
    } finally {
        await act(() => root.unmount())
        host.remove()
    }
})
