// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import type { NodeItem } from '@/shared/types/nodes'
import type { EngineProcessStatus, EngineStatusData, EngineType } from '@/shared/types/engines'
import { buildOverviewNodes } from '@/ui/utils/overview-nodes'
import {
    areWelcomeEnginesInstalled,
    getWelcomeInstallOutcome,
    hasWelcomeEngineStatuses,
    isWelcomeEngineInstallable
} from '@/ui/utils/welcome-install'
import { completeFirstRun, isFirstRun, loadUiConfig } from '@/electron/config/ui-config'
import { initPlatform } from '@/electron/globals'
import { assertIsolated } from '../fixtures/isolation'

function engineStatus(
    engineType: EngineType,
    processStatus: EngineProcessStatus
): EngineStatusData {
    return {
        engineType,
        nodeId: 'self',
        processStatus,
        enginePort: null,
        proxyPort: null
    }
}

describe('first launch', () => {
    it('never treats an unknown or existing engine as an install target', () => {
        expect(isWelcomeEngineInstallable(undefined)).toBe(false)
        expect(isWelcomeEngineInstallable('running')).toBe(false)
        expect(isWelcomeEngineInstallable('stopped')).toBe(false)
        expect(isWelcomeEngineInstallable('not-installed')).toBe(true)
    })

    it('waits for every local welcome-engine status before onboarding', () => {
        const local = new Map<EngineType, unknown>()
        const statuses = new Map<string, ReadonlyMap<EngineType, unknown>>([['self', local]])
        const candidates: EngineType[] = ['ollama', 'lm-studio']

        expect(hasWelcomeEngineStatuses(statuses, 'self', candidates)).toBe(false)
        local.set('ollama', {})
        expect(hasWelcomeEngineStatuses(statuses, 'self', candidates)).toBe(false)
        local.set('lm-studio', {})
        expect(hasWelcomeEngineStatuses(statuses, 'self', candidates)).toBe(true)
    })

    it('only treats a complete authoritative snapshot as all engines installed', () => {
        const local = new Map<EngineType, EngineStatusData>()
        const statuses = new Map<string, ReadonlyMap<EngineType, EngineStatusData>>([
            ['self', local]
        ])
        const candidates: EngineType[] = ['ollama', 'lm-studio']

        expect(areWelcomeEnginesInstalled(statuses, 'self', candidates)).toBe(false)
        local.set('ollama', engineStatus('ollama', 'running'))
        expect(areWelcomeEnginesInstalled(statuses, 'self', candidates)).toBe(false)
        local.set('lm-studio', engineStatus('lm-studio', 'stopped'))
        expect(areWelcomeEnginesInstalled(statuses, 'self', candidates)).toBe(true)
        local.set('lm-studio', engineStatus('lm-studio', 'not-installed'))
        expect(areWelcomeEnginesInstalled(statuses, 'self', candidates)).toBe(false)
    })

    it('persists first-run completion only after onboarding finishes', () => {
        // Guard against a developer-set PAIR_USER_DATA: this test writes real
        // UI config + first-run state, so it must resolve to a tmpdir.
        assertIsolated()
        const userData = process.env.PAIR_USER_DATA ?? ''
        initPlatform({
            getUserData: () => userData,
            getTemp: () => userData,
            getResourcesPath: () => process.cwd(),
            getAppName: () => 'Personal AI Router'
        })
        loadUiConfig()
        expect(isFirstRun()).toBe(true)
        expect(isFirstRun()).toBe(true)

        completeFirstRun()
        loadUiConfig()
        expect(isFirstRun()).toBe(false)
    })

    it('keeps onboarding open until every selected engine is installed', () => {
        const started = new Set<EngineType>(['ollama', 'lm-studio'])

        expect(
            getWelcomeInstallOutcome(
                [
                    { engineType: 'ollama', status: 'stopped' },
                    { engineType: 'lm-studio', status: 'installing' }
                ],
                started
            )
        ).toBe('pending')
        expect(
            getWelcomeInstallOutcome(
                [
                    { engineType: 'ollama', status: 'running' },
                    { engineType: 'lm-studio', status: 'stopped' }
                ],
                started
            )
        ).toBe('complete')
    })

    it('allows retry when an installation returns to not-installed', () => {
        expect(
            getWelcomeInstallOutcome(
                [{ engineType: 'lm-studio', status: 'not-installed' }],
                new Set<EngineType>(['lm-studio'])
            )
        ).toBe('failed')
        expect(
            getWelcomeInstallOutcome(
                [{ engineType: 'lm-studio', status: 'not-installed' }],
                new Set<EngineType>()
            )
        ).toBe('pending')
    })

    it('keeps a partial failure locked until the other engine finishes', () => {
        const started = new Set<EngineType>(['ollama', 'lm-studio'])
        const failed = new Set<EngineType>(['lm-studio'])
        expect(
            getWelcomeInstallOutcome(
                [
                    { engineType: 'ollama', status: 'installing' },
                    { engineType: 'lm-studio', status: 'not-installed' }
                ],
                started,
                failed
            )
        ).toBe('pending')
        expect(
            getWelcomeInstallOutcome(
                [
                    { engineType: 'ollama', status: 'stopped' },
                    { engineType: 'lm-studio', status: 'not-installed' }
                ],
                started,
                failed
            )
        ).toBe('failed')
    })

    it('shows a local card while fresh discovery is still empty', () => {
        const nodes = buildOverviewNodes(new Map(), [], 'local-node', 'Windows')
        expect(nodes).toHaveLength(1)
        expect(nodes[0]).toMatchObject({ id: 'local-node', status: 'active', os: 'Windows' })
    })

    it('replaces the temporary local card with discovered data', () => {
        const discovered: NodeItem = {
            id: 'local-node',
            name: 'My PC',
            status: 'active',
            ipAddress: '192.168.1.2',
            port: 8080,
            allIpAddresses: ['192.168.1.2'],
            topology: {
                cpu: { model: 'CPU', cores: 8, threads: 16 },
                gpus: [],
                ram: 32,
                storage: []
            },
            os: 'Windows'
        }
        expect(
            buildOverviewNodes(new Map([[discovered.id, discovered]]), [], discovered.id, 'Windows')
        ).toEqual([discovered])
    })

    it('keeps clustered self active while discovery is empty', () => {
        const members = [
            {
                id: 'local-node',
                nodeUuid: 'uuid',
                name: 'My PC',
                ipAddress: '192.168.1.2',
                port: 8080,
                clusterId: 'cluster',
                state: 'member' as const,
                joinedAt: 1,
                lastSeen: 1
            }
        ]
        // Self is keyed by the node UUID (`nodeUuid`), not the hostname; the
        // placeholder still surfaces the hostname as the display `name`.
        expect(buildOverviewNodes(new Map(), members, 'uuid', 'Windows')[0]).toMatchObject({
            id: 'uuid',
            name: 'My PC',
            status: 'active'
        })
    })
})
