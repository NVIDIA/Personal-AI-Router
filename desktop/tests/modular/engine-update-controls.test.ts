// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type ReactNode } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import type { EngineUpdateAvailable } from '@/shared/types/engines'
import type { ServiceError } from '@/shared/types/errors'
import type { IEngineApi } from '@/ui/api/engine-api'
import { BackendUpdateBanner } from '@/ui/components/BackendRow/BackendUpdateBanner'
import { ErrorModal } from '@/ui/components/ErrorModal'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useErrorsStore } from '@/ui/stores/errors.store'
import type { BackendInfo } from '@/ui/types/engine-info'

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
    useErrorsStore.setState({ errors: [] })
    useConnectionStore.setState({ selfId: null })
    vi.unstubAllGlobals()
})

function button(label: string): HTMLButtonElement | undefined {
    return Array.from(document.querySelectorAll('button')).find(
        el => el.textContent.trim() === label
    )
}

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
