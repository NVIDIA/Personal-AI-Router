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

it('deduplicates and withdraws local notices independently for each engine', () => {
    const unsubscribe = subscribeEngineUpdateNotifications(() => 'local-node')
    report({ ...update, nodeId: 'peer' })
    const lmStudioUpdate: EngineUpdateAvailable = {
        ...update,
        engineType: 'lm-studio',
        currentVersion: '0.10.0',
        releaseUrl: 'https://lmstudio.ai/changelog/lmstudio',
        installType: 'external',
        downloadUrl: 'https://lmstudio.ai/download#lm-studio-download-heading'
    }
    report(lmStudioUpdate)
    report(update)
    report(update)
    report({ ...update, latestVersion: '0.10.3' })
    report(lmStudioUpdate)
    publishPush('engines:state-changed', {
        nodeId: 'local-node',
        engineType: 'ollama',
        updateAvailable: null
    })
    unsubscribe()
    expect(vi.mocked(showOverviewMessage).mock.calls.map(([message]) => message.id)).toEqual([
        'engine-update:local-node:lm-studio:0.10.2',
        'engine-update:local-node:ollama:0.10.2',
        'engine-update:local-node:ollama:0.10.3'
    ])
    expect(vi.mocked(dismissOverviewMessage).mock.calls).toEqual([
        ['engine-update:local-node:ollama:0.10.2'],
        ['engine-update:local-node:ollama:0.10.3']
    ])
    expect(vi.mocked(showOverviewMessage).mock.calls[0][0].title).toBe('LM Studio update available')
})

it('opens the local engine settings from the vLLM notification', async () => {
    const unsubscribe = subscribeEngineUpdateNotifications(() => 'local-node')
    report({
        ...update,
        engineType: 'vllm',
        releaseUrl: 'https://github.com/vllm-project/vllm/releases/tag/v0.10.2'
    })
    unsubscribe()
    const [[message]] = vi.mocked(showOverviewMessage).mock.calls
    const onClose = vi.fn<() => void>()
    useOverviewUiStore.getState().openSettings('service')
    await act(() => root.render(createElement(AppMessageModal, { message, onClose })))
    expect(document.body.textContent).toContain('vLLM update available')
    await act(() => button('Open engine settings')?.click())

    expect(useOverviewUiStore.getState()).toMatchObject({
        activeTab: 'overview',
        focusNodeId: 'local-node'
    })
    expect(onClose).toHaveBeenCalledOnce()
})

it('shows the managed llama.cpp build tags and dispatches Update', async () => {
    const onUpdate = vi.fn<() => void>()
    const llama: BackendInfo = {
        ...engine,
        type: 'llama-cpp',
        displayName: 'llama.cpp',
        updateAvailable: {
            ...update,
            engineType: 'llama-cpp',
            currentVersion: 'b12001',
            latestVersion: 'b12002',
            releaseUrl: 'https://github.com/ggml-org/llama.cpp/releases/tag/b12002'
        }
    }
    await act(() =>
        root.render(
            createElement(BackendUpdateBanner, { backend: llama, disabled: false, onUpdate })
        )
    )
    expect(document.body.textContent).toContain('b12001 installed')
    expect(document.body.textContent).toContain('llama.cpp b12002 is available')
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

it('opens the headless LM Studio update guide for a newly detected external daemon', async () => {
    const onUpdate = vi.fn<() => void>()
    const guide = 'https://lmstudio.ai/docs/cli/daemon/daemon-update'
    const external: BackendInfo = {
        ...engine,
        type: 'lm-studio',
        displayName: 'LM Studio',
        // The update check may detect the daemon before the lifecycle snapshot refreshes.
        processStatus: 'not-installed',
        updateAvailable: {
            ...update,
            engineType: 'lm-studio',
            installType: 'external',
            releaseUrl: guide,
            downloadUrl: guide
        }
    }
    await act(() =>
        root.render(
            createElement(BackendUpdateBanner, {
                backend: external,
                disabled: false,
                onUpdate
            })
        )
    )
    await act(() => button('Get update')?.click())
    expect(openExternal.mock.calls).toEqual([['https://lmstudio.ai/docs/cli/daemon/daemon-update']])
    expect(button('Update details')).toBeUndefined()
    expect(onUpdate).not.toHaveBeenCalled()
})

it('retries a failed update only for the affected local engine', async () => {
    const retry = vi.fn<IEngineApi['update']>()
    vi.stubGlobal('pairApi', { engines: { update: retry }, errors: { clear: vi.fn() } })
    useConnectionStore.setState({ selfId: 'local-node' })
    const failure: ServiceError = {
        id: 'engine-manager:update-failed:lmstudio',
        timestamp: 1,
        action: 'retry',
        engineType: 'lm-studio',
        operation: 'update',
        message: 'LM Studio update failed',
        nodeId: 'peer-node'
    }
    useErrorsStore.setState({ errors: [failure] })
    await act(() => root.render(createElement(ErrorModal, {})))
    expect(button('Retry')).toBeUndefined()

    await act(() => useErrorsStore.setState({ errors: [{ ...failure, nodeId: 'local-node' }] }))
    await act(() => button('Retry')?.click())
    expect(retry.mock.calls).toEqual([['lm-studio', 'local-node']])
})
