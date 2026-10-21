// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import { IdleTimeoutExpiries } from '@/shared/constants/engines'
import { expiryToIdleSeconds, idleSecondsToExpiry } from '@/shared/utils/engines'

const NODE = 'idle-timeout-self'

function statusOf(engine: string, idle?: boolean, running = true) {
    return {
        engine,
        installed: true,
        running,
        healthy: running,
        port: 8001,
        ...(idle ? { idle } : {})
    }
}

function vllmStatus() {
    return getModularBridgeState()
        .getEngineInitialState()
        .statuses.find(status => status.nodeId === NODE && status.engineType === 'vllm')
}

function vllmExpiries() {
    return getModularBridgeState()
        .getEngineInitialState()
        .models.find(models => models.nodeId === NODE && models.engineType === 'vllm')
        ?.models.map(model => model.expiry)
}

describe('idle limit values', () => {
    it('turns every offered limit into seconds and back', () => {
        for (const expiry of IdleTimeoutExpiries) {
            const seconds = expiryToIdleSeconds(expiry)
            expect(seconds).toBeDefined()
            expect(idleSecondsToExpiry(seconds ?? -1)).toBe(expiry)
        }
        expect(expiryToIdleSeconds('10m')).toBe(600)
        expect(expiryToIdleSeconds('-1')).toBe(0)
    })

    it('never turns a limit reloading cannot pay off into seconds', () => {
        for (const expiry of ['0', '1s', '10s'] as const) {
            expect(expiryToIdleSeconds(expiry)).toBeUndefined()
        }
    })

    it('shows a limit that is not offered as the nearest one', () => {
        expect(idleSecondsToExpiry(120)).toBe('1m') // set by editing a file
        expect(idleSecondsToExpiry(1500)).toBe('30m')
        expect(idleSecondsToExpiry(0)).toBe('-1')
    })
})

describe('an engine that is on with nothing loaded', () => {
    beforeEach(() => {
        const state = getModularBridgeState()
        state.setSelfId(NODE)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [{ hostUuid: NODE, name: 'idle-host', ipAddress: '127.0.0.1', port: 14318 }]
            }
        })
    })

    it('is reported idle while it is running, and not otherwise', () => {
        const state = getModularBridgeState()
        state.applyEngineManagerStatus(statusOf('vllm', true))
        expect(vllmStatus()).toMatchObject({ processStatus: 'running', idle: true })

        state.applyEngineManagerStatus(statusOf('vllm'))
        expect(vllmStatus()?.processStatus).toBe('running')
        expect(vllmStatus()).not.toHaveProperty('idle')

        // An engine that is not running is not idle, whatever it last reported.
        state.applyEngineManagerStatus(statusOf('vllm', true, false))
        expect(vllmStatus()?.processStatus).toBe('stopped')
        expect(vllmStatus()).not.toHaveProperty('idle')
    })

    it('never marks another engine idle', () => {
        const state = getModularBridgeState()
        state.applyEngineManagerStatus(statusOf('ollama'))
        const ollama = state
            .getEngineInitialState()
            .statuses.find(status => status.nodeId === NODE && status.engineType === 'ollama')
        expect(ollama).not.toHaveProperty('idle')
    })

    it('shows the idle limit on the models, before and after the list changes', () => {
        const state = getModularBridgeState()
        state.applyEngineManagerStatus(statusOf('vllm'))
        state.setLocalEngineModels('vllm', ['Qwen/Qwen3-8B', 'Qwen/Qwen3-32B'])
        expect(vllmExpiries()).toEqual(['5m', '5m']) // the default, until the limit is read

        state.setEngineIdleSeconds('vllm', 1800)
        expect(vllmExpiries()).toEqual(['30m', '30m'])

        state.setLocalEngineModels('vllm', ['Qwen/Qwen3-8B'])
        expect(vllmExpiries()).toEqual(['30m'])

        state.setEngineIdleSeconds('vllm', 0)
        expect(vllmExpiries()).toEqual(['-1'])
    })

    it('reads the idle limit and the model list from the engine manager', async () => {
        const supervisor = getModularSupervisor()
        const hasProcess = vi.spyOn(supervisor, 'hasProcess').mockReturnValue(true)
        const call = vi
            .spyOn(supervisor, 'callProcess')
            .mockImplementation(
                async (_name, _method, params): Promise<JsonValue | undefined> =>
                    typeof params === 'object' &&
                    params &&
                    'action' in params &&
                    params.action === 'get_idle_timeout'
                        ? { seconds: 1800 }
                        : { data: [{ id: 'Qwen/Qwen3-8B' }] }
            )
        getModularBridgeState().applyEngineManagerStatus(statusOf('vllm', true))

        supervisor.refreshManagedEngineModels({ engine: 'vllm', running: true })
        await new Promise(resolve => setTimeout(resolve, 10))

        expect(call).toHaveBeenCalledWith('broker', 'engine:action', {
            engine: 'vllm',
            action: 'get_idle_timeout'
        })
        expect(vllmExpiries()).toEqual(['30m'])
        call.mockRestore()
        hasProcess.mockRestore()
    })
})
