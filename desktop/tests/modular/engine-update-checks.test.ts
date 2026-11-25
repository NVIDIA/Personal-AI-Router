// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { watchEngineUpdates } from '@/electron/service-bridge/engine-update-checks'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { EngineType } from '@/shared/types/engines'
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))

const available = {
    currentVersion: '1.0.0',
    latestVersion: '1.2.0',
    releaseUrl: 'https://github.com/ollama/ollama/releases/tag/v1.2.0',
    installType: 'managed'
}

describe('local engine update checks', () => {
    const state = getModularBridgeState()
    const check = vi.fn<(engine: EngineType) => Promise<JsonValue | undefined>>()
    const sixHours = 6 * 60 * 60 * 1000
    let checks: ReturnType<typeof watchEngineUpdates>
    let unsubscribe: (() => void) | undefined

    beforeEach(() => {
        vi.useFakeTimers()
        vi.clearAllMocks()
        state.setSelfId('local')
        state.applyEngineManagerList({ engines: [{ engine: 'ollama', installed: true }] })
        check.mockReset().mockResolvedValue({ updateAvailable: available })
    })
    afterEach(() => {
        unsubscribe?.()
        unsubscribe = undefined
        checks.stop()
        vi.useRealTimers()
    })

    it('publishes the startup offer once and clears it when a later check finds no update', async () => {
        const patches: JsonValue[] = []
        unsubscribe = subscribePush(event => {
            if (
                event.channel === 'engines:state-changed' &&
                event.payload.updateAvailable !== undefined
            ) {
                patches.push(event.payload.updateAvailable?.latestVersion ?? null)
            }
        })
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        const offer = { ...available, nodeId: 'local', engineType: 'ollama' }
        expect(state.getEngineInitialState().updateAvailable).toEqual([offer])

        await vi.advanceTimersByTimeAsync(sixHours)
        expect(check).toHaveBeenCalledTimes(2)
        expect(patches).toEqual(['1.2.0'])

        check.mockResolvedValue({ updateAvailable: null })
        await checks.refresh()
        expect(state.getEngineInitialState().updateAvailable).toEqual([])
        expect(patches).toEqual(['1.2.0', null])
    })

    it('checks only the engine that started or stopped, once per transition', async () => {
        const stopped = { engine: 'lmstudio', installed: true, running: false }
        const running = { ...stopped, running: true }
        state.applyEngineManagerList({ engines: [stopped, { engine: 'ollama', installed: true }] })
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        check.mockClear()

        state.applyEngineManagerStatus({ ...stopped, starting: true })
        expect(check).not.toHaveBeenCalled()
        state.applyEngineManagerStatus(running)
        await vi.advanceTimersByTimeAsync(0)
        state.applyEngineManagerStatus({ ...running, healthy: true })
        expect(check.mock.calls).toEqual([['lm-studio']])

        state.applyEngineManagerStatus(stopped)
        await vi.advanceTimersByTimeAsync(0)
        state.applyEngineManagerStatus(stopped)
        expect(check.mock.calls).toEqual([['lm-studio'], ['lm-studio']])
    })

    it('checks vLLM when enabled or stopped without loading a model', async () => {
        const stopped = { engine: 'vllm', installed: true, running: false, awaiting_model: false }
        state.applyEngineManagerList({ engines: [stopped] })
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        check.mockClear()

        state.applyEngineManagerStatus({ ...stopped, awaiting_model: true })
        await vi.advanceTimersByTimeAsync(0)
        expect(check.mock.calls).toEqual([['vllm']])

        state.applyEngineManagerStatus(stopped)
        expect(check.mock.calls).toEqual([['vllm'], ['vllm']])
    })

    it('publishes a completed start and keeps the previous offer when its update check fails', async () => {
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        const offline = Promise.withResolvers<JsonValue>()
        check.mockClear().mockReturnValueOnce(offline.promise)

        state.applyEngineManagerStatus({ engine: 'ollama', installed: true, running: true })
        expect(check.mock.calls).toEqual([['ollama']])
        expect(state.getEngineInitialState().statuses).toContainEqual(
            expect.objectContaining({
                nodeId: 'local',
                engineType: 'ollama',
                processStatus: 'running'
            })
        )

        offline.reject(new Error('offline'))
        await vi.advanceTimersByTimeAsync(0)

        expect(state.getEngineInitialState().updateAvailable).toEqual([
            { ...available, nodeId: 'local', engineType: 'ollama' }
        ])
    })

    it('checks each supported engine and keeps its offers independent of other engines', async () => {
        state.applyEngineManagerList({
            engines: [
                { engine: 'ollama', installed: true },
                { engine: 'lmstudio', installed: false },
                { engine: 'llamacpp', installed: true }
            ]
        })
        const lmStudio = {
            currentVersion: '0.4.0',
            latestVersion: '0.4.1',
            releaseUrl: 'https://lmstudio.ai/changelog',
            downloadUrl: 'https://lmstudio.ai/download',
            installType: 'external'
        }
        check.mockImplementation(async engine => {
            if (engine === 'llama-cpp') throw new Error('offline')
            return { updateAvailable: engine === 'lm-studio' ? lmStudio : available }
        })
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)

        expect(check.mock.calls).toEqual([['ollama'], ['lm-studio'], ['llama-cpp']])
        expect(state.getEngineInitialState().updateAvailable).toEqual([
            { ...available, nodeId: 'local', engineType: 'ollama' },
            { ...lmStudio, nodeId: 'local', engineType: 'lm-studio' }
        ])

        check.mockClear().mockResolvedValue({ updateAvailable: null })
        await checks.refresh('lm-studio')
        expect(check.mock.calls).toEqual([['lm-studio']])
        expect(state.getEngineInitialState().updateAvailable).toEqual([
            { ...available, nodeId: 'local', engineType: 'ollama' }
        ])
    })

    it('requires a valid vendor download link for external installations', async () => {
        check.mockResolvedValue({ updateAvailable: { ...available, installType: 'external' } })
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        expect(state.getEngineInitialState().updateAvailable).toEqual([])

        check.mockResolvedValue({
            updateAvailable: {
                ...available,
                installType: 'external',
                downloadUrl: 'file:///installer'
            }
        })
        await checks.refresh('ollama')
        expect(state.getEngineInitialState().updateAvailable).toEqual([])
    })

    it('coalesces refreshes after a lifecycle change without showing the old offer', async () => {
        const previous = Promise.withResolvers<JsonValue>()
        check.mockReturnValueOnce(previous.promise).mockResolvedValue({ updateAvailable: null })
        const offeredVersions: string[] = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:state-changed' && event.payload.updateAvailable) {
                offeredVersions.push(event.payload.updateAvailable.latestVersion)
            }
        })
        checks = watchEngineUpdates(check)

        void checks.refresh()
        void checks.refresh()
        expect(check).toHaveBeenCalledOnce()
        previous.resolve({ updateAvailable: available })
        await vi.advanceTimersByTimeAsync(0)

        expect(check).toHaveBeenCalledTimes(2)
        expect(offeredVersions).toEqual([])
        expect(state.getEngineInitialState().updateAvailable).toEqual([])
    })

    it('clears the installed offer on completion before a failed release check', async () => {
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        expect(state.getEngineInitialState().updateAvailable).toHaveLength(1)
        check.mockRejectedValue(new Error('release feed unavailable'))

        state.applyEngineManagerProgress({ engine: 'ollama', stage: 'done' })
        expect(state.getEngineInitialState().updateAvailable).toEqual([])
        await vi.advanceTimersByTimeAsync(0)

        expect(check).toHaveBeenCalledTimes(2)
        expect(state.getEngineInitialState().updateAvailable).toEqual([])
    })

    it('keeps the offer when the installer finds Ollama already installed', async () => {
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        check.mockRejectedValue(new Error('release feed unavailable'))

        state.applyEngineManagerProgress({ engine: 'ollama', stage: 'already-installed' })
        await vi.advanceTimersByTimeAsync(0)

        expect(check).toHaveBeenCalledTimes(2)
        expect(state.getEngineInitialState().updateAvailable).toEqual([
            { ...available, nodeId: 'local', engineType: 'ollama' }
        ])
    })

    it('checks the new local identity and removes the old node offer', async () => {
        checks = watchEngineUpdates(check)
        await vi.advanceTimersByTimeAsync(0)
        state.setSelfId('replacement')

        expect(state.getEngineInitialState().updateAvailable).toEqual([])
        await vi.advanceTimersByTimeAsync(0)
        expect(state.getEngineInitialState().updateAvailable).toEqual([
            { ...available, nodeId: 'replacement', engineType: 'ollama' }
        ])
    })

    it('ignores a check that completes after shutdown', async () => {
        const pending = Promise.withResolvers<JsonValue>()
        check.mockReturnValueOnce(pending.promise)
        checks = watchEngineUpdates(check)

        checks.stop()
        pending.resolve({ updateAvailable: available })
        await vi.advanceTimersByTimeAsync(sixHours)

        expect(check).toHaveBeenCalledOnce()
        expect(state.getEngineInitialState().updateAvailable).toEqual([])
    })
})
