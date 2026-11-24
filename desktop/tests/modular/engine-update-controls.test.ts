// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type ReactNode } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { subscribeEngineUpdateNotifications } from '@/electron/updater/engine-update-notification'
import { publishPush } from '@/electron/service-bridge/push-bus'
import { dismissOverviewMessage, showOverviewMessage } from '@/electron/window'
import type { EngineUpdateAvailable } from '@/shared/types/engines'
import type { ServiceError } from '@/shared/types/errors'
import type { IEngineApi } from '@/ui/api/engine-api'
import { AppMessageModal } from '@/ui/components/AppMessageModal'
import { BackendUpdateBanner } from '@/ui/components/BackendRow/BackendUpdateBanner'
import { ErrorModal } from '@/ui/components/ErrorModal'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import type { BackendInfo } from '@/ui/types/engine-info'

vi.mock('@/electron/window', () => ({
    showOverviewMessage: vi.fn<typeof showOverviewMessage>(),
    dismissOverviewMessage: vi.fn<typeof dismissOverviewMessage>()
}))

// JSDOM does not implement the native modal dialog lifecycle.
vi.mock('@nvidia/foundations-react-core', async importOriginal => {
    const kit = await importOriginal<typeof import('@nvidia/foundations-react-core')>()
    const container = ({ children }: { children?: ReactNode }) =>
        createElement('div', null, children)
    return { ...kit, ModalRoot: container, ModalDialog: container, ModalContent: container }
})

const update: EngineUpdateAvailable = {
    nodeId: 'local-node',
    engineType: 'ollama',
    currentVersion: '0.10.1',
    latestVersion: '0.10.2',
    releaseUrl: 'https://github.com/ollama/ollama/releases/tag/v0.10.2',
    installType: 'managed'
}

const engine: BackendInfo = {
    type: 'ollama',
    displayName: 'Ollama',
    source: 'detected',
    processStatus: 'stopped',
    port: null,
    proxyPort: null,
    models: [],
    modelsKnown: true,
    updateAvailable: update
}

let root: Root
const openExternal = vi.fn<(url: string) => Promise<void>>()

beforeEach(() => {
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
    vi.stubGlobal('windowApi', { window: { openExternal } })
    root = createRoot(document.body.appendChild(document.createElement('div')))
    vi.clearAllMocks()
})

afterEach(async () => {
    await act(() => root.unmount())
    document.body.replaceChildren()
    useOverviewUiStore.setState({ activeTab: 'overview', focusNodeId: null })
    useErrorsStore.setState({ errors: [] })
    useConnectionStore.setState({ selfId: null })
    vi.unstubAllGlobals()
})

function button(label: string): HTMLButtonElement | undefined {
    return Array.from(document.querySelectorAll('button')).find(
        el => el.textContent.trim() === label
    )
}

function report(updateAvailable: EngineUpdateAvailable): void {
    const { nodeId, engineType } = updateAvailable
    publishPush('engines:state-changed', { nodeId, engineType, updateAvailable })
}

it('shows a local Ollama notification once per release and withdraws it when cleared', () => {
    const unsubscribe = subscribeEngineUpdateNotifications(() => 'local-node')
    report({ ...update, nodeId: 'peer' })
    report(update)
    report(update)
    report({ ...update, latestVersion: '0.10.3' })
    publishPush('engines:state-changed', {
        nodeId: 'local-node',
        engineType: 'ollama',
        updateAvailable: null
    })
    unsubscribe()
    expect(vi.mocked(showOverviewMessage).mock.calls.map(([message]) => message.id)).toEqual([
        'engine-update:local-node:ollama:0.10.2',
        'engine-update:local-node:ollama:0.10.3'
    ])
    expect(vi.mocked(dismissOverviewMessage).mock.calls).toEqual([
        ['engine-update:local-node:ollama:0.10.2'],
        ['engine-update:local-node:ollama:0.10.3']
    ])
    expect(vi.mocked(showOverviewMessage).mock.calls[0][0].title).toBe('Ollama update available')
})

it('opens the local engine settings from the Ollama notification', async () => {
    const unsubscribe = subscribeEngineUpdateNotifications(() => 'local-node')
    report(update)
    unsubscribe()
    const [[message]] = vi.mocked(showOverviewMessage).mock.calls
    const onClose = vi.fn<() => void>()
    useOverviewUiStore.getState().openSettings('service')
    await act(() => root.render(createElement(AppMessageModal, { message, onClose })))
    expect(document.body.textContent).toContain('Ollama update available')
    await act(() => button('Open engine settings')?.click())

    expect(useOverviewUiStore.getState()).toMatchObject({
        activeTab: 'overview',
        focusNodeId: 'local-node'
    })
    expect(onClose).toHaveBeenCalledOnce()
})

it('shows the installed and available Ollama versions and dispatches Update', async () => {
    const onUpdate = vi.fn<() => void>()
    await act(() =>
        root.render(
            createElement(BackendUpdateBanner, { backend: engine, disabled: false, onUpdate })
        )
    )
    expect(document.body.textContent).toContain('v0.10.1 installed')
    expect(document.body.textContent).toContain('Ollama v0.10.2 is available')
    await act(() => button('Update')?.click())

    expect(onUpdate).toHaveBeenCalledOnce()
    expect(openExternal).not.toHaveBeenCalled()
})

it('opens the official download and release notes for an externally installed Ollama', async () => {
    const onUpdate = vi.fn<() => void>()
    const external: BackendInfo = {
        ...engine,
        updateAvailable: {
            ...update,
            installType: 'external',
            downloadUrl: 'https://ollama.com/download'
        }
    }
    await act(() =>
        root.render(
            createElement(BackendUpdateBanner, { backend: external, disabled: false, onUpdate })
        )
    )
    await act(() => button('Get update')?.click())
    await act(() => button('Update details')?.click())

    expect(openExternal.mock.calls).toEqual([
        ['https://ollama.com/download'],
        ['https://github.com/ollama/ollama/releases/tag/v0.10.2']
    ])
    expect(button('Update')).toBeUndefined()
    expect(onUpdate).not.toHaveBeenCalled()
})

it('retries a failed update only for the affected local engine', async () => {
    const retry = vi.fn<IEngineApi['update']>()
    vi.stubGlobal('pairApi', { engines: { update: retry }, errors: { clear: vi.fn() } })
    useConnectionStore.setState({ selfId: 'local-node' })
    const failure: ServiceError = {
        id: 'engine-manager:update-failed:ollama',
        timestamp: 1,
        action: 'retry',
        engineType: 'ollama',
        operation: 'update',
        message: 'Ollama update failed',
        nodeId: 'peer-node'
    }
    useErrorsStore.setState({ errors: [failure] })
    await act(() => root.render(createElement(ErrorModal, {})))
    expect(button('Retry')).toBeUndefined()

    await act(() => useErrorsStore.setState({ errors: [{ ...failure, nodeId: 'local-node' }] }))
    await act(() => button('Retry')?.click())
    expect(retry.mock.calls).toEqual([['ollama', 'local-node']])
})
