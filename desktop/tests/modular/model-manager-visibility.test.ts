// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { afterEach, describe, expect, it } from 'vitest'
import { EngineDisplayNames, EngineTypes } from '@/shared/constants/engines'
import type { EngineType } from '@/shared/types/engines'
import { ModelManager } from '@/ui/components/ModelManager/ModelManager'
import { ModelSection } from '@/ui/components/ModelManager/ModelSection'
import { engineProgressKey } from '@/shared/utils/engine-progress'
import type { EngineProgress, ModelItemStatus } from '@/shared/types/engines'
import type { BackendInfo, ModelItem } from '@/ui/types/engine-info'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'

function cachedModel(): ModelItem {
    return {
        name: 'Qwen/Qwen2.5-1.5B-Instruct',
        size: 1024,
        downloaded: true,
        status: 'idle',
        parameterSize: '1.5B',
        quantization: '',
        family: 'qwen2',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: '10m',
        capabilities: []
    }
}

function engineInfo(type: EngineType, overrides: Partial<BackendInfo> = {}): BackendInfo {
    return {
        type,
        displayName: EngineDisplayNames[type],
        source: 'installed',
        processStatus: 'stopped',
        port: 8001,
        proxyPort: 8000,
        models: [],
        modelsKnown: true,
        ...overrides
    }
}

afterEach(() => {
    useEngineProgressStore.getState().cleanup()
    usePendingActionsStore.getState().cleanup()
})

describe.each(EngineTypes)('%s model visibility', engineType => {
    it('shows unavailable guidance for an unknown running inventory', () => {
        const html = renderToStaticMarkup(
            createElement(ModelManager, {
                backend: engineInfo(engineType, { processStatus: 'running', modelsKnown: false }),
                nodeId: 'local-node'
            })
        )

        expect(html).toContain('Downloaded models unavailable')
        expect(html).not.toContain('No model/weight files downloaded')
    })

    it.each(['local-node', 'peer-node'])('shows the start message when stopped on %s', nodeId => {
        const html = renderToStaticMarkup(
            createElement(ModelManager, { backend: engineInfo(engineType), nodeId })
        )

        expect(html).toContain('Start engine to see models')
        expect(html).not.toContain('No model/weight files downloaded')
        expect(html).not.toContain('Add model')
    })

    it.each(['local-node', 'peer-node'])(
        'hides cached models and controls when stopped on %s',
        nodeId => {
            const html = renderToStaticMarkup(
                createElement(ModelSection, {
                    backend: engineInfo(engineType, { models: [cachedModel()] }),
                    nodeId
                })
            )

            expect(html).toContain('Start engine to see models')
            expect(html).not.toContain(cachedModel().name)
            expect(html).not.toContain('Open ')
            expect(html).not.toContain('Add model')
        }
    )

    it.each(['local-node', 'peer-node'])(
        'shows the empty inventory and Add model when running on %s',
        nodeId => {
            const html = renderToStaticMarkup(
                createElement(ModelManager, {
                    backend: engineInfo(engineType, { processStatus: 'running' }),
                    nodeId
                })
            )

            expect(html).toContain('No model/weight files downloaded')
            expect(html).toContain('Add model')
            expect(html).not.toContain('Start engine to see models')
        }
    )

    it.each(['local-node', 'peer-node'])(
        'shows downloaded models and controls when running on %s',
        nodeId => {
            const html = renderToStaticMarkup(
                createElement(ModelManager, {
                    backend: engineInfo(engineType, {
                        processStatus: 'running',
                        models: [cachedModel()]
                    }),
                    nodeId
                })
            )

            expect(html).toContain(cachedModel().name)
            expect(html).toContain('actions')
            expect(html).toContain('Add model')
            expect(html).not.toContain('Start engine to see models')
            expect(html).not.toContain('No model/weight files downloaded')
        }
    )
})

describe.each(['local-node', 'peer-node'])('vLLM state details on %s', nodeId => {
    it('keeps models and controls visible when running with an unloaded model', () => {
        const html = renderToStaticMarkup(
            createElement(ModelManager, {
                backend: engineInfo('vllm', {
                    processStatus: 'running',
                    models: [cachedModel()]
                }),
                nodeId
            })
        )

        expect(html).toContain(cachedModel().name)
        expect(html).not.toContain('Unloads after')
        expect(html).toContain('Add model')
        expect(html).not.toContain('Start engine to see models')
    })

    it('shows an empty inventory when running with no cached models', () => {
        const html = renderToStaticMarkup(
            createElement(ModelManager, {
                backend: engineInfo('vllm', { processStatus: 'running' }),
                nodeId
            })
        )

        expect(html).toContain('No model/weight files downloaded')
        expect(html).toContain('Add model')
        expect(html).not.toContain('Start engine to see models')
    })

    it.each<ModelItemStatus>(['loaded', 'loading', 'ejecting', 'pulling'])(
        'hides a stale %s model when stopped',
        status => {
            const html = renderToStaticMarkup(
                createElement(ModelManager, {
                    backend: engineInfo('vllm', { models: [{ ...cachedModel(), status }] }),
                    nodeId
                })
            )

            expect(html).toContain('Start engine to see models')
            expect(html).not.toContain('role="status"')
            expect(html).not.toContain(cachedModel().name)
            expect(html).not.toContain('Add model')
        }
    )

    it('hides stale download progress while stopped and restores it when running', () => {
        const progress: EngineProgress = {
            nodeId,
            nodeName: 'Test node',
            engineType: 'vllm',
            operation: 'pull',
            model: 'org/incoming-model',
            status: 'downloading',
            percent: 25
        }
        useEngineProgressStore.setState({
            progress: new Map([[engineProgressKey(progress), progress]])
        })

        const stopped = renderToStaticMarkup(
            createElement(ModelManager, { backend: engineInfo('vllm'), nodeId })
        )
        expect(stopped).toContain('Start engine to see models')
        expect(stopped).not.toContain('org/incoming-model')
        expect(stopped).not.toContain('role="status"')

        const running = renderToStaticMarkup(
            createElement(ModelManager, {
                backend: engineInfo('vllm', { processStatus: 'running' }),
                nodeId
            })
        )
        expect(running).toContain('org/incoming-model')
        expect(running).toContain('role="status"')
        expect(running).not.toContain('No model/weight files downloaded')
    })

    it('does not use another node’s progress to hide the running empty-state message', () => {
        const progress: EngineProgress = {
            nodeId: 'unrelated-node',
            nodeName: 'Other node',
            engineType: 'vllm',
            operation: 'pull',
            model: 'org/other-node-model',
            status: 'downloading'
        }
        useEngineProgressStore.setState({
            progress: new Map([[engineProgressKey(progress), progress]])
        })

        const html = renderToStaticMarkup(
            createElement(ModelManager, {
                backend: engineInfo('vllm', { processStatus: 'running' }),
                nodeId
            })
        )

        expect(html).toContain('No model/weight files downloaded')
        expect(html).not.toContain('org/other-node-model')
    })
})

describe('enabled vLLM before model selection', () => {
    it('offers Add model and selection guidance when its cache is empty', () => {
        const html = renderToStaticMarkup(
            createElement(ModelManager, {
                backend: engineInfo('vllm', { processStatus: 'awaiting-model' }),
                nodeId: 'local-node'
            })
        )
        expect(html).toContain('Add model')
        expect(html).toContain('Select a downloaded model and choose Load')
        expect(html).not.toContain('Start engine to see models')
    })

    it('offers Load for cached models without a running server', () => {
        const html = renderToStaticMarkup(
            createElement(ModelManager, {
                backend: engineInfo('vllm', {
                    processStatus: 'awaiting-model',
                    models: [cachedModel()]
                }),
                nodeId: 'local-node'
            })
        )
        expect(html).toContain('Qwen2.5-1.5B-Instruct')
        expect(html).toContain('Load')
        expect(html).not.toContain('Start engine to see models')
    })
})
