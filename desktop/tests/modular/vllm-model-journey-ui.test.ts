// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement, type ComponentType } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it } from 'vitest'
import SetupServingCard from '@/ui/components/ClusterSettings/SetupServingCard'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useVllmGroupStore } from '@/ui/stores/vllm-group.store'
import { useOnboardingStore } from '@/ui/stores/onboarding.store'
import { useEngineProgressStore } from '@/ui/stores/engine-progress.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import type { NodeItem } from '@/shared/types/nodes'
import type { EngineProgress, EngineStatusData } from '@/shared/types/engines'
import type { OnboardingHistorySummary } from '@/shared/types/onboarding-history'
import type { VllmServingReadiness } from '@/ui/utils/vllm-serving-readiness'
import { VLLM_QWEN38_MODEL } from '@/shared/constants/vllm'
import type { VllmModelReceipt, VllmRuntimePrepareResult } from '@/shared/types/vllm-model-journey'
import { engineProgressKey } from '@/shared/utils/engine-progress'
import { qwenGroupReview } from '../fixtures/vllm-group'

const A_CLOSURE = '8396cb056ada51dee9f92eeb11469c4573d967b2861b5c3c1ef5f7b4c1ec3c61'
const BC_CLOSURE = '0265d190e9bd5c90ac4492a1c811056dc4b0ed6814bd797c5d65cac075f5d3c2'
const DIGEST = 'a'.repeat(64)
const SetupCard = SetupServingCard as ComponentType<{
    initialHistory: OnboardingHistorySummary
    initialReadiness: VllmServingReadiness
    initialJourney?: {
        selectedNodeIds: string[]
        sourceNodeId: string
        model: string
        modelReceipts: Record<string, VllmModelReceipt>
        runtimeResults: Record<string, VllmRuntimePrepareResult>
    }
}>

const history: OnboardingHistorySummary = {
    total: 0,
    historyOnly: 0,
    current: 0,
    invalid: 0,
    recoveryRequired: false,
    diagnosticRecoveryRequired: false,
    discoveryBlocked: false,
    mutationSupported: false,
    operations: []
}

function node(id: string): NodeItem {
    return {
        id,
        name: id === 'node-a' ? 'Spark A' : id === 'node-b' ? 'Spark B' : 'Spark C',
        status: 'active',
        ipAddress: '192.0.2.1',
        port: 0,
        allIpAddresses: [],
        topology: {
            cpu: { model: '', cores: 0, threads: 0 },
            gpus: [{ id: 'gpu0', name: 'GB10', vramTotal: 1 }],
            ram: 1,
            storage: []
        },
        os: 'Linux'
    }
}

const readiness: VllmServingReadiness = {
    nodes: [],
    servingReplicas: 0,
    reusableModels: 1,
    replicatedModels: 1,
    controlsNodeId: 'node-a',
    distributionSupported: true
}

const receipt = {
    id: VLLM_QWEN38_MODEL,
    source: 'huggingface' as const,
    revision: VLLM_QWEN38_MODEL.slice(-40),
    license: 'nvidia-open-model-license',
    metadataSha256: DIGEST,
    digest: 'b'.repeat(64),
    bytes: 132_734_505_212
}

function runtime(nodeId: string, closure: string) {
    return {
        nodeId,
        operationId: nodeId === 'node-a' ? '1'.repeat(32) : '2'.repeat(32),
        state: 'ready',
        recipeId: 'qwen38-recipe',
        recipeSha256: DIGEST,
        runtimeVersion: '0.28.1rc1.dev361+gd4d703caf',
        artifactCount: 196,
        artifactBytes: 3_930_666_769,
        resumed: false,
        provider: {
            qualified: true,
            profileId:
                nodeId === 'node-a' ? 'spark-a-libc8.6-r580.95.05' : 'spark-bc-libc8.8-r580.173.02',
            expectedClosureSha256: closure,
            observedClosureSha256: closure,
            allowedClosureSha256: [A_CLOSURE, BC_CLOSURE],
            mismatches: []
        }
    }
}

describe('vLLM model/runtime journey UI', () => {
    beforeEach(() => {
        useConnectionStore.setState({
            connected: true,
            selfId: 'node-a',
            clusterId: 'cluster-a'
        })
        useNodesStore.setState({
            nodes: new Map([
                ['node-a', node('node-a')],
                ['node-b', node('node-b')]
            ]),
            fetchedNodes: true
        })
        useNodesStore.getInitialState().nodes = new Map()
        useVllmGroupStore.setState({
            known: true,
            status: { activationEnabled: false, reserved: false, reason: '' },
            error: null,
            lastRun: null,
            review: null,
            check: null,
            pending: null,
            uncertainStart: false,
            owner: 'cluster-a:node-a',
            current: true,
            actionError: ''
        })
        useVllmGroupStore.getInitialState().review = null
        useOnboardingStore.setState({
            candidates: [],
            artifacts: [],
            review: null,
            operation: null,
            pending: null,
            error: null
        })
        useEngineProgressStore.setState({ progress: new Map() })
        useEngineProgressStore.getInitialState().progress = new Map()
        useEngineStatusStore.setState({ statusByNode: new Map() })
        useEngineStatusStore.getInitialState().statusByNode = new Map()
    })

    it('shows the full typed journey and holds mixed provider closures with the official guide action', () => {
        const markup = renderToStaticMarkup(
            createElement(SetupCard, {
                initialHistory: history,
                initialReadiness: readiness,
                initialJourney: {
                    selectedNodeIds: ['node-a', 'node-b'],
                    sourceNodeId: 'node-a',
                    model: VLLM_QWEN38_MODEL,
                    modelReceipts: { 'node-a': receipt, 'node-b': receipt },
                    runtimeResults: {
                        'node-a': runtime('node-a', A_CLOSURE),
                        'node-b': runtime('node-b', BC_CLOSURE)
                    }
                }
            })
        )
        for (const text of [
            'Model &amp; runtime preparation',
            'Verify / resume on source',
            'Prepare exact runtime on selected nodes',
            'Distribute to missing selected nodes',
            'Cancel source pull',
            'Cancel runtime preparation',
            'different qualified provider closures',
            'Open DGX Dashboard update guide',
            'Fleet compatibility',
            'Provider values are last-prepared session evidence',
            'group review and Start recheck live provider facts',
            'Provider (last prepare)',
            'No prepared drift known',
            'Update selected nodes',
            'Not reported is unknown',
            'nvidia-open-model-license'
        ])
            expect(markup).toContain(text)
        expect(markup).not.toContain('Automatic distribution unavailable')
        expect(markup).not.toContain('/usr/lib/')
        expect(markup).not.toContain('apt ')
    })

    it('holds the fixed Qwen runtime preparation action for another exact model', () => {
        const otherModel = `owner/other-model@${'c'.repeat(40)}`
        const otherReceipt = { ...receipt, id: otherModel, revision: 'c'.repeat(40) }
        const markup = renderToStaticMarkup(
            createElement(SetupCard, {
                initialHistory: history,
                initialReadiness: readiness,
                initialJourney: {
                    selectedNodeIds: ['node-a', 'node-b'],
                    sourceNodeId: 'node-a',
                    model: otherModel,
                    modelReceipts: { 'node-a': otherReceipt, 'node-b': otherReceipt },
                    runtimeResults: {}
                }
            })
        )
        expect(markup).toContain(
            'Automatic runtime preparation is currently fixed to the supported Qwen3.8 profile.'
        )
        expect(markup).toMatch(
            /<button[^>]*disabled=""[^>]*>.*Prepare exact runtime on selected nodes.*<\/button>/s
        )
    })

    it('shows the exact reviewed two-Spark RoCE routing policy', () => {
        const nodes = new Map([
            ['node-a', node('node-a')],
            ['node-b', node('node-b')]
        ])
        useNodesStore.getInitialState().nodes = nodes
        useNodesStore.setState({ nodes })
        const review = qwenGroupReview()
        useVllmGroupStore.getInitialState().review = review
        useVllmGroupStore.setState({ review })
        const markup = renderToStaticMarkup(
            createElement(SetupCard, { initialHistory: history, initialReadiness: readiness })
        )
        expect(markup).toContain('host-buffer RoCE · merged NICs on · socket payload fallback off')
    })

    it('shows recovered per-node progress without claiming readiness or exposing an unbound cancel', () => {
        const nodes = new Map([
            ['node-a', node('node-a')],
            ['node-b', node('node-b')],
            ['node-c', node('node-c')],
            ['node-d', node('node-d')]
        ])
        useNodesStore.getInitialState().nodes = nodes
        useNodesStore.setState({ nodes })
        const progress: EngineProgress[] = [
            {
                engineType: 'vllm',
                nodeId: 'node-a',
                nodeName: 'Spark A',
                operation: 'prepare',
                operationId: '1'.repeat(32),
                status: 'installing-runtime',
                percent: 15
            },
            {
                engineType: 'vllm',
                nodeId: 'node-b',
                nodeName: 'Spark B',
                operation: 'pull',
                model: VLLM_QWEN38_MODEL,
                status: 'downloading',
                percent: 42
            },
            {
                engineType: 'vllm',
                nodeId: 'node-c',
                nodeName: 'Spark C',
                operation: 'distribute',
                operationId: '3'.repeat(32),
                model: VLLM_QWEN38_MODEL,
                status: 'receiving',
                percent: 26,
                network: 'fabric'
            },
            {
                engineType: 'vllm',
                nodeId: 'node-a',
                nodeName: 'Spark A',
                operation: 'pull',
                model: VLLM_QWEN38_MODEL,
                status: 'complete',
                percent: 100
            },
            {
                engineType: 'vllm',
                nodeId: 'node-d',
                nodeName: 'Other node',
                operation: 'pull',
                model: `owner/other-model@${'c'.repeat(40)}`,
                status: 'downloading',
                percent: 87
            }
        ]
        const activeProgress = new Map(progress.map(entry => [engineProgressKey(entry), entry]))
        useEngineProgressStore.getInitialState().progress = activeProgress
        useEngineProgressStore.setState({ progress: activeProgress })
        const markup = renderToStaticMarkup(
            createElement(SetupCard, { initialHistory: history, initialReadiness: readiness })
        )
        for (const text of [
            'Preparation in progress',
            'installing-runtime · 15%',
            'downloading · 42%',
            'receiving · 26% · over the fabric',
            'Active on an unselected node'
        ])
            expect(markup).toContain(text)
        expect(markup).not.toContain('downloading · 42% · over')
        expect(markup).not.toContain('Ready for review')
        expect(markup).not.toContain('100%')
        expect(markup).not.toContain('87%')
        expect(markup).not.toMatch(/>Cancel<\/button>/)
        expect(markup).not.toContain('>not ready<')
        expect(markup).not.toContain('Cancel source pull')
        expect(markup).toMatch(/<button[^>]*disabled=""[^>]*>Use fixed Qwen3\.8<\/button>/)
    })

    it('does not present unrelated model progress as Qwen preparation', () => {
        const otherModel = `owner/other-model@${'c'.repeat(40)}`
        const unrelated: EngineProgress = {
            engineType: 'vllm',
            nodeId: 'node-a',
            nodeName: 'Spark A',
            operation: 'pull',
            model: otherModel,
            status: 'downloading',
            percent: 87
        }
        const unrelatedPrepare: EngineProgress = {
            engineType: 'vllm',
            nodeId: 'node-b',
            nodeName: 'Spark B',
            operation: 'prepare',
            operationId: 'f'.repeat(32),
            model: otherModel,
            status: 'installing-runtime',
            percent: 88
        }
        useEngineProgressStore.getInitialState().progress = new Map(
            [unrelated, unrelatedPrepare].map(entry => [engineProgressKey(entry), entry])
        )
        const markup = renderToStaticMarkup(
            createElement(SetupCard, { initialHistory: history, initialReadiness: readiness })
        )
        expect(markup).toContain('Preparation required')
        expect(markup).not.toContain('87%')
        expect(markup).not.toContain('88%')
        expect(markup).not.toContain('Preparation in progress')
        expect(markup).toMatch(/<button[^>]*disabled=""[^>]*>Use fixed Qwen3\.8<\/button>/)
    })

    it('replaces a ready claim with progress during a recovered repeat prepare', () => {
        const preparedA = runtime('node-a', A_CLOSURE)
        const preparedB = runtime('node-b', A_CLOSURE)
        const status = (nodeId: string, version: string): EngineStatusData => ({
            engineType: 'vllm',
            nodeId,
            processStatus: 'stopped',
            enginePort: null,
            proxyPort: null,
            installedVersion: version,
            managed: true
        })
        useEngineStatusStore.getInitialState().statusByNode = new Map([
            ['node-a', new Map([['vllm', status('node-a', preparedA.runtimeVersion)]])],
            ['node-b', new Map([['vllm', status('node-b', preparedB.runtimeVersion)]])]
        ])
        const props = {
            initialHistory: history,
            initialReadiness: readiness,
            initialJourney: {
                selectedNodeIds: ['node-a', 'node-b'],
                sourceNodeId: 'node-a',
                model: VLLM_QWEN38_MODEL,
                modelReceipts: { 'node-a': receipt, 'node-b': receipt },
                runtimeResults: { 'node-a': preparedA, 'node-b': preparedB }
            }
        }
        const ready = renderToStaticMarkup(createElement(SetupCard, props))
        expect(ready).toContain('Ready for review')
        expect(ready.match(/>prepared</g)).toHaveLength(2)

        const repeatPrepare: EngineProgress = {
            engineType: 'vllm',
            nodeId: 'node-a',
            nodeName: 'Spark A',
            operation: 'prepare',
            operationId: '1'.repeat(32),
            status: 'installing-runtime',
            percent: 15
        }
        useEngineProgressStore.getInitialState().progress = new Map([
            [engineProgressKey(repeatPrepare), repeatPrepare]
        ])
        const preparing = renderToStaticMarkup(createElement(SetupCard, props))
        expect(preparing).toContain('Preparation in progress')
        expect(preparing).toContain('installing-runtime · 15%')
        expect(preparing).not.toContain('Ready for review')
        expect(preparing.match(/>prepared</g)).toHaveLength(1)
    })
})
