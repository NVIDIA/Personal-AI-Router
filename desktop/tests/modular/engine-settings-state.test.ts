// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
    useEngineSettingsStore,
    engineSettingsKey,
    watchEngineSettings
} from '@/ui/stores/engine-settings.store'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'
import type { EngineSettingsSnapshot } from '@/shared/types/engine-settings'
import { redactSensitiveLogText } from '@/electron/redact-log'

const target = { nodeId: 'remote', engine: 'ollama' } as const
const baseline: EngineSettingsSnapshot = {
    ...target,
    revision: 1,
    appliedRevision: 1,
    epoch: 'one',
    sequence: 1,
    settings: { serverPort: 1235, proxyPort: 11434, launchText: 'managed serve' },
    effectiveServerPort: 1235,
    effectiveProxyPort: 11434,
    running: true,
    adopted: false,
    editable: true,
    reason: '',
    format: 'pair-arguments-v1',
    phase: 'idle',
    error: '',
    requestId: ''
}
const key = engineSettingsKey(target)
const store = useEngineSettingsStore
const getSettings = vi.fn()
const applySettings = vi.fn()

describe('engine settings snapshot ordering and pending operations', () => {
    beforeEach(() => {
        store.setState({ entries: {} })
        vi.stubGlobal('window', { pairApi: { engines: { getSettings, applySettings } } })
        getSettings.mockResolvedValue(baseline)
        applySettings.mockResolvedValue({ revision: 2, phase: 'succeeded' })
    })
    afterEach(() => vi.unstubAllGlobals())

    it('ignores older frames and marks identical reconnect baselines available', () => {
        store.getState().receive({ ...baseline, sequence: 3, revision: 2 })
        store.getState().receive(baseline)
        expect(store.getState().entries[key].snapshot?.revision).toBe(2)
        store.getState().disconnect('remote')
        expect(store.getState().entries[key].unavailable).toBeTruthy()
        store.getState().receive({ ...baseline, sequence: 3, revision: 2 })
        expect(store.getState().entries[key].unavailable).toBeUndefined()
    })

    it('a newer push epoch beats a delayed initial read', async () => {
        let finish: (snapshot: EngineSettingsSnapshot) => void = () => {}
        getSettings.mockImplementationOnce(
            () =>
                new Promise<EngineSettingsSnapshot>(resolve => {
                    finish = resolve
                })
        )
        const loading = store.getState().reload(target)
        store.getState().receive({ ...baseline, epoch: 'two', revision: 9 })
        finish(baseline)
        await loading
        expect(store.getState().entries[key].snapshot?.epoch).toBe('two')
    })

    it('acknowledgement alone cannot fabricate an applied snapshot', async () => {
        store.getState().receive(baseline)
        await store.getState().apply({
            ...target,
            expectedRevision: 1,
            requestId: 'request',
            settings: { ...baseline.settings, launchText: 'different' }
        })
        expect(store.getState().entries[key].snapshot?.revision).toBe(1)
        // The settings push carries the applied snapshot; re-reading here would
        // only race it.
        expect(getSettings).not.toHaveBeenCalled()
    })

    it('a failed transport reads back and exposes unavailable status', async () => {
        applySettings.mockRejectedValueOnce(new Error('disconnected'))
        getSettings.mockRejectedValueOnce(new Error('offline'))
        await expect(
            store.getState().apply({
                ...target,
                expectedRevision: 1,
                requestId: 'request',
                settings: baseline.settings
            })
        ).rejects.toThrow('disconnected')
        expect(store.getState().entries[key].unavailable).toContain('offline')
    })

    it('shares subscriptions and releases them when the last editor closes', () => {
        const stop = vi.fn()
        const subscribe = vi.fn(() => stop)
        vi.stubGlobal('window', {
            pairApi: {
                engines: { onSettingsChanged: subscribe, onSettingsDisconnected: subscribe }
            }
        })
        const a = watchEngineSettings(),
            b = watchEngineSettings()
        expect(subscribe).toHaveBeenCalledTimes(2)
        a()
        expect(stop).not.toHaveBeenCalled()
        b()
        expect(stop).toHaveBeenCalledTimes(2)
    })

    it('closing an editor leaves other devices editable and resubscribes on reopen', () => {
        const receivers: Array<(snapshot: EngineSettingsSnapshot) => void> = []
        vi.stubGlobal('window', {
            pairApi: {
                engines: {
                    onSettingsChanged: (fn: (snapshot: EngineSettingsSnapshot) => void) => {
                        receivers.push(fn)
                        return () => {}
                    },
                    onSettingsDisconnected: () => () => {}
                }
            }
        })
        store.getState().receive(baseline)
        // An engine that starts installing unmounts its own editor; that must
        // not report every device in the cluster as disconnected.
        watchEngineSettings()()
        expect(store.getState().entries[key].unavailable).toBeUndefined()
        expect(store.getState().entries[key].snapshot?.revision).toBe(1)

        const reopened = watchEngineSettings()
        receivers.at(-1)?.({ ...baseline, sequence: 2, revision: 4 })
        expect(store.getState().entries[key].snapshot?.revision).toBe(4)
        expect(store.getState().entries[key].unavailable).toBeUndefined()
        reopened()
    })
})

describe('settings applies use the shared pending-actions cover', () => {
    let deliver: (snapshot: EngineSettingsSnapshot) => void = () => {}

    beforeEach(() => {
        const noop = (): void => {}
        vi.stubGlobal('window', {
            pairApi: {
                engines: {
                    onStateChanged: () => noop,
                    onProgress: () => noop,
                    onSettingsChanged: (fn: (snapshot: EngineSettingsSnapshot) => void) => {
                        deliver = fn
                        return noop
                    }
                },
                errors: { onUpdate: () => noop }
            }
        })
        usePendingActionsStore.getState().initialize()
    })
    afterEach(() => {
        usePendingActionsStore.getState().cleanup()
        vi.unstubAllGlobals()
    })

    it('holds from the send until the first snapshot, then yields to phase', () => {
        const pending = usePendingActionsStore.getState()
        expect(pending.isSettingsPending('remote', 'ollama')).toBe(false)

        pending.beginSettings('remote', 'ollama', 'current-request')
        expect(usePendingActionsStore.getState().isSettingsPending('remote', 'ollama')).toBe(true)

        // Another engine on the same device is untouched.
        expect(usePendingActionsStore.getState().isSettingsPending('remote', 'lm-studio')).toBe(
            false
        )

        deliver({ ...baseline, phase: 'applying', requestId: 'older-request' })
        expect(usePendingActionsStore.getState().isSettingsPending('remote', 'ollama')).toBe(true)
        deliver({ ...baseline, phase: 'idle' })
        expect(usePendingActionsStore.getState().isSettingsPending('remote', 'ollama')).toBe(true)
        deliver({ ...baseline, phase: 'applying', requestId: 'current-request' })
        expect(usePendingActionsStore.getState().isSettingsPending('remote', 'ollama')).toBe(false)
    })

    it('releases the cover when validation refuses before sending', () => {
        const pending = usePendingActionsStore.getState()
        pending.beginSettings('remote', 'lm-studio', 'current-request')
        pending.endSettings('remote', 'lm-studio', 'older-request')
        expect(usePendingActionsStore.getState().isSettingsPending('remote', 'lm-studio')).toBe(
            true
        )
        pending.endSettings('remote', 'lm-studio', 'current-request')
        expect(usePendingActionsStore.getState().isSettingsPending('remote', 'lm-studio')).toBe(
            false
        )
    })
})

describe('launch text redaction', () => {
    const frame = JSON.stringify({
        result: {
            settings: { launchText: 'exe --unknown private-value' },
            launch_args: ['private-value'],
            launch_env: ['VALUE=private-value']
        }
    })

    it('omits launch text, arguments, and environment from process logs', () => {
        expect(redactSensitiveLogText(frame)).not.toContain('private-value')
    })

    it('omits them from a frame truncated mid-value', () => {
        expect(redactSensitiveLogText(frame.slice(0, -1))).not.toContain('private-value')
        expect(
            redactSensitiveLogText('{"result":{"launch_env":["VALUE=private-value"')
        ).not.toContain('private-value')
    })

    it('still redacts the pairing PIN', () => {
        expect(redactSensitiveLogText('{"result":{"pin":"123456"}}')).not.toContain('123456')
        expect(redactSensitiveLogText('garbage {"pin": "123456"')).not.toContain('123456')
    })

    // A worker's own command line is what a support log is for, so the
    // redaction must not reach a generic args/env key.
    it('keeps worker spawn diagnostics intact', () => {
        const spawn = JSON.stringify({
            msg: 'spawning worker',
            bin: 'nvpair-node-scanner',
            args: ['--ipc', '--log-level=debug'],
            env: { NVPAIR_LOG: 'debug' }
        })
        expect(redactSensitiveLogText(spawn)).toBe(spawn)
        expect(redactSensitiveLogText('plain text with no json at all')).toBe(
            'plain text with no json at all'
        )
    })
})
