// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it } from 'vitest'
import { createElement } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import TrayNodeRow from '@/ui/components/TrayApp/TrayNodeRow'
import { countNodeEngineServices } from '@/ui/utils/get-engines-for-node'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import type { CustomEndpointInfo } from '@/shared/types/custom-endpoints'
import type { NodeItem } from '@/shared/types/nodes'
import type {
    EngineProcessStatus,
    EngineStatusByNode,
    EngineStatusData,
    EngineType
} from '@/shared/types/engines'

/**
 * The tray's `Engines (n/m)` line once read a constant `n/3`: it counted the
 * padded manifest row list, which fabricates a placeholder per engine type and
 * never sees Custom endpoints. These tests pin the replacement contract —
 * engines a node actually has, including Custom — both for the counting helper
 * and for the rendered row. The suite runs without a DOM, so rows go through
 * React's server renderer like custom-endpoints-card-render.test.ts does.
 */

function engineStatus(
    nodeId: string,
    engineType: EngineType,
    processStatus: EngineProcessStatus
): EngineStatusData {
    return {
        engineType,
        nodeId,
        processStatus,
        enginePort: processStatus === 'not-installed' ? null : 1234,
        proxyPort: processStatus === 'not-installed' ? null : 54301
    }
}

function reportedFor(
    nodeId: string,
    entries: Array<[EngineType, EngineProcessStatus]>
): Map<EngineType, EngineStatusData> {
    const inner = new Map<EngineType, EngineStatusData>()
    for (const [engineType, processStatus] of entries) {
        inner.set(engineType, engineStatus(nodeId, engineType, processStatus))
    }
    return inner
}

const customOnline: CustomEndpointInfo = {
    id: 'custom:https://vllm.example:8000/v1',
    baseUrl: 'https://vllm.example:8000/v1',
    keySet: false,
    healthy: true
}

const secondCustomOnline: CustomEndpointInfo = {
    id: 'custom:https://other.example:8000/v1',
    baseUrl: 'https://other.example:8000/v1',
    keySet: false,
    healthy: true
}

const customOffline: CustomEndpointInfo = {
    id: 'custom:https://broken.example:9000/v1',
    baseUrl: 'https://broken.example:9000/v1',
    keySet: false,
    healthy: false
}

describe('countNodeEngineServices', () => {
    it('counts one online custom endpoint on a node with no managed engines', () => {
        expect(countNodeEngineServices(undefined, [customOnline])).toEqual({
            online: 1,
            total: 1
        })
    })

    it('counts zero engines when nothing is installed and no custom endpoint is configured', () => {
        expect(countNodeEngineServices(undefined, undefined)).toEqual({
            online: 0,
            total: 0
        })
    })

    it('excludes explicit not-installed reports from the total', () => {
        // A local node reports all three manifest engines even when none are
        // installed; `not-installed` proves absence, so the row reads 0/0.
        const reported = reportedFor('this-mac', [
            ['ollama', 'not-installed'],
            ['lm-studio', 'not-installed'],
            ['llama-cpp', 'not-installed']
        ])
        expect(countNodeEngineServices(reported, undefined)).toEqual({
            online: 0,
            total: 0
        })
    })

    it('excludes initializing placeholders from the total', () => {
        // `initializing` is the desktop's placeholder for a pair the service
        // never reported; counting it made remote nodes read N/3 for engines
        // that do not exist there.
        const reported = reportedFor('peer-mac', [
            ['lm-studio', 'running'],
            ['llama-cpp', 'initializing']
        ])
        expect(countNodeEngineServices(reported, undefined)).toEqual({
            online: 1,
            total: 1
        })
    })

    it('counts a stopped managed engine in the total but not online', () => {
        const reported = reportedFor('peer-mac', [['ollama', 'stopped']])
        expect(countNodeEngineServices(reported, undefined)).toEqual({
            online: 0,
            total: 1
        })
    })

    it('counts LM Studio running plus one of two custom endpoints online', () => {
        const reported = reportedFor('peer-mac', [
            ['ollama', 'not-installed'],
            ['lm-studio', 'running']
        ])
        expect(countNodeEngineServices(reported, [customOnline, customOffline])).toEqual({
            online: 2,
            total: 3
        })
    })

    it('ignores a facade-level custom status because endpoints are counted individually', () => {
        // The Custom facade can appear as one EngineStatusData entry for a
        // remote peer; the per-endpoint snapshot is the source of truth, so
        // the facade entry must not add a third engine beside its endpoints.
        const reported = reportedFor('peer-mac', [['custom', 'running']])
        expect(countNodeEngineServices(reported, [customOnline, secondCustomOnline])).toEqual({
            online: 2,
            total: 2
        })
    })
})

describe('TrayNodeRow Engines line', () => {
    /**
     * Under React's server renderer, zustand 5 serves every store read from
     * getInitialState() (its server snapshot), so seeding must mutate the
     * initial-state map; setState would be invisible to a static render.
     */
    function seedStatusByNode(statusByNode: EngineStatusByNode): void {
        const initial = useEngineStatusStore.getInitialState().statusByNode
        initial.clear()
        for (const [nodeId, inner] of statusByNode) initial.set(nodeId, inner)
    }

    function makeTrayNode(overrides: Partial<NodeItem> = {}): NodeItem {
        // Chart-related fields stay inert: without metrics the row renders the
        // chart placeholder, so the label markup is what these tests read.
        return {
            id: 'peer-mac',
            name: 'Peer-Mac',
            status: 'active',
            ipAddress: '192.168.1.50',
            port: 9111,
            allIpAddresses: ['192.168.1.50'],
            topology: {
                cpu: { model: 'Apple M4', cores: 10, threads: 10 },
                gpus: [],
                ram: 34359738368,
                storage: [{ name: 'Macintosh HD', capacity: 999653632000 }]
            },
            os: 'MacOS',
            ...overrides
        }
    }

    function renderTrayRow(
        node: NodeItem,
        customEntries: readonly CustomEndpointInfo[] | undefined
    ): string {
        return renderToStaticMarkup(
            createElement(TrayNodeRow, { node, activeWorkloads: [], customEntries })
        )
    }

    beforeEach(() => {
        seedStatusByNode(new Map())
    })

    it('shows Engines (1/1) for the user machine with only one online custom endpoint', () => {
        // The bug this file exists for: all three manifest engines reported
        // as not installed plus one online custom endpoint rendered
        // "Engines (0/3)" — the custom endpoint vanished and three phantom
        // engines appeared.
        seedStatusByNode(
            new Map([
                [
                    'peer-mac',
                    reportedFor('peer-mac', [
                        ['ollama', 'not-installed'],
                        ['lm-studio', 'not-installed'],
                        ['llama-cpp', 'not-installed']
                    ])
                ]
            ])
        )

        const html = renderTrayRow(makeTrayNode(), [customOnline])

        expect(html).toContain('Engines (1/1)')
        expect(html).not.toContain('Engines (0/3)')
    })

    it('shows Engines (0/0) for a node with nothing installed or configured', () => {
        seedStatusByNode(
            new Map([
                [
                    'peer-mac',
                    reportedFor('peer-mac', [
                        ['ollama', 'not-installed'],
                        ['lm-studio', 'not-installed'],
                        ['llama-cpp', 'not-installed']
                    ])
                ]
            ])
        )

        expect(renderTrayRow(makeTrayNode(), undefined)).toContain('Engines (0/0)')
    })

    it('shows Engines (2/3) with LM Studio running and one of two custom endpoints online', () => {
        seedStatusByNode(
            new Map([
                [
                    'peer-mac',
                    reportedFor('peer-mac', [
                        ['ollama', 'not-installed'],
                        ['lm-studio', 'running']
                    ])
                ]
            ])
        )

        expect(renderTrayRow(makeTrayNode(), [customOnline, customOffline])).toContain(
            'Engines (2/3)'
        )
    })

    it('renders a remote peer that reported no engine facts without phantom engines', () => {
        // Older peers never push per-engine state; the padded list once drew
        // "Engines (0/3)" for them from placeholders alone.
        expect(renderTrayRow(makeTrayNode(), undefined)).toContain('Engines (0/0)')
    })
})
