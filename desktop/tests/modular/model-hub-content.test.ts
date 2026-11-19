// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// @vitest-environment jsdom

import { act, createElement, type AriaRole, type ComponentProps, type ReactNode } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import type { BannerProps, ButtonProps, StatusMessage } from '@nvidia/foundations-react-core'
import type { EngineHubSearchResponse } from '@/shared/types/engine-api'
import type { IEngineApi } from '@/ui/api/engine-api'
import type { EngineType } from '@/shared/types/engines'
import type { ModelHubSearchBar } from '@/ui/components/ModelHub/ModelHubSearchBar'
import type EngineHubListRow from '@/ui/components/ModelHub/EngineHubListRow'
import { ModelHubContent } from '@/ui/components/ModelHub/ModelHubContent'

// Keep catalog state, loading, and list rendering real. Replace presentation
// controls so the checks do not depend on library layout or popover support.
vi.mock('@nvidia/foundations-react-core', () => {
    const container = ({ children, role }: { children?: ReactNode; role?: AriaRole }) =>
        createElement('div', { role }, children)
    return {
        Stack: container,
        Flex: container,
        Text: container,
        Divider: () => createElement('hr'),
        Button: ({ children, disabled, onClick }: ButtonProps) =>
            createElement('button', { disabled, onClick }, children),
        Banner: ({ children, role, slotActions }: BannerProps) =>
            createElement('div', { role }, children, slotActions),
        StatusMessage: ({
            slotHeading,
            slotSubheading,
            slotFooter
        }: ComponentProps<typeof StatusMessage>) =>
            createElement('div', null, slotHeading, slotSubheading, slotFooter)
    }
})
vi.mock('@/ui/components/ModelHub/ModelHubSearchBar', () => ({
    ModelHubSearchBar: ({ onSearch }: ComponentProps<typeof ModelHubSearchBar>) =>
        createElement('button', { onClick: () => onSearch('small-model') }, 'Search fixture')
}))
vi.mock('@/ui/components/ModelHub/EngineHubListRow', () => ({
    default: ({ model }: ComponentProps<typeof EngineHubListRow>) =>
        createElement('div', null, model.name)
}))

const searchHub = vi.fn<IEngineApi['searchHub']>()
const populatedCatalog: EngineHubSearchResponse = {
    models: [
        {
            id: 'org/small-model',
            name: 'org/small-model',
            author: 'org',
            url: 'https://example.invalid/model',
            downloads: 1,
            likes: 0,
            updatedAt: '2026-01-01T00:00:00Z',
            tags: []
        }
    ],
    searchable: true
}
let host: HTMLDivElement
let root: Root

beforeEach(() => {
    vi.useFakeTimers()
    vi.stubGlobal(
        'ResizeObserver',
        class implements ResizeObserver {
            observe = vi.fn<ResizeObserver['observe']>()
            unobserve = vi.fn<ResizeObserver['unobserve']>()
            disconnect = vi.fn<ResizeObserver['disconnect']>()
        }
    )
    searchHub.mockReset()
    Object.defineProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT', {
        value: true,
        configurable: true
    })
    Object.defineProperty(window, 'pairApi', {
        configurable: true,
        value: { engines: { searchHub } }
    })
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
})

afterEach(async () => {
    await act(() => root.unmount())
    host.remove()
    Reflect.deleteProperty(window, 'pairApi')
    Reflect.deleteProperty(globalThis, 'IS_REACT_ACT_ENVIRONMENT')
    vi.clearAllTimers()
    vi.useRealTimers()
    vi.unstubAllGlobals()
})

async function openCatalog(engine: EngineType = 'vllm', nodeId = 'fresh-node') {
    await act(() =>
        root.render(
            createElement(ModelHubContent, {
                engine,
                nodeId,
                multiple: false,
                downloadedModels: [],
                onSubmit: vi.fn()
            })
        )
    )
}

it('fills the first open after a delayed successful catalog request', async () => {
    let finishRequest: (value: EngineHubSearchResponse) => void = () => {
        throw new Error('Catalog request has not started')
    }
    searchHub.mockReturnValueOnce(
        new Promise(resolve => {
            finishRequest = resolve
        })
    )

    await openCatalog()
    expect(host.textContent).toContain('Searching models...')
    expect(host.textContent).not.toContain('No models found')

    await act(() => finishRequest(populatedCatalog))

    expect(host.textContent).toContain('org/small-model')
    expect(host.textContent).not.toContain('Searching models...')
    expect(searchHub).toHaveBeenCalledExactlyOnceWith('vllm', 'fresh-node', undefined)
})

async function click(text: string) {
    const button = Array.from(host.querySelectorAll('button')).find(
        candidate => candidate.textContent === text
    )
    if (!button) throw new Error(`Missing button: ${text}`)
    await act(() => button.click())
}

const testManualRecovery = (name: string, engine: EngineType) => {
    it(name, async () => {
        searchHub
            .mockResolvedValueOnce({ models: [], searchable: null })
            .mockResolvedValueOnce(populatedCatalog)

        await openCatalog(engine)

        expect(host.textContent).toContain("Couldn't load models")
        expect(host.textContent).not.toContain('No models found')
        expect(host.querySelector('[role="alert"]')).not.toBeNull()
        expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 5s…')

        await click('Retry')

        expect(host.textContent).toContain('org/small-model')
        expect(host.textContent).not.toContain("Couldn't load models")
        expect(host.querySelector('[role="timer"]')).toBeNull()
        expect(searchHub).toHaveBeenCalledTimes(2)
        await act(() => vi.advanceTimersByTimeAsync(60_000))
        expect(searchHub).toHaveBeenCalledTimes(2)
    })
}
testManualRecovery('retries an unread vLLM catalog without reopening', 'vllm')
testManualRecovery('retries an unread LM Studio catalog without reopening', 'lm-studio')
testManualRecovery('retries an unread llama.cpp catalog without reopening', 'llama-cpp')
testManualRecovery('retries an unread Ollama broker reply without reopening', 'ollama')

it('counts down to automatic recovery while keeping Retry available', async () => {
    searchHub
        .mockResolvedValueOnce({ models: [], searchable: null })
        .mockResolvedValueOnce(populatedCatalog)
    await openCatalog()

    expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 5s…')
    expect(
        Array.from(host.querySelectorAll('button')).find(button => button.textContent === 'Retry')
            ?.disabled
    ).toBe(false)

    await act(() => vi.advanceTimersByTimeAsync(1_000))
    expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 4s…')
    expect(searchHub).toHaveBeenCalledTimes(1)

    await act(() => vi.advanceTimersByTimeAsync(3_999))
    expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 1s…')
    expect(searchHub).toHaveBeenCalledTimes(1)
    await act(() => vi.advanceTimersByTimeAsync(1))

    expect(searchHub).toHaveBeenCalledTimes(2)
    expect(host.textContent).toContain('org/small-model')
    expect(host.textContent).not.toContain("Couldn't load models")
    expect(host.querySelector('[role="timer"]')).toBeNull()
})

it('stops automatic retries after two recovery attempts', async () => {
    searchHub.mockResolvedValue({ models: [], searchable: null })
    await openCatalog()
    await act(() => vi.advanceTimersByTimeAsync(5_000))
    expect(searchHub).toHaveBeenCalledTimes(2)
    expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 10s…')
    await act(() => vi.advanceTimersByTimeAsync(1_000))
    expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 9s…')
    await act(() => vi.advanceTimersByTimeAsync(9_000))
    expect(searchHub).toHaveBeenCalledTimes(3)

    await act(() => vi.advanceTimersByTimeAsync(300_000))

    expect(searchHub).toHaveBeenCalledTimes(3)
    expect(host.textContent).toContain("Couldn't load models")
    expect(host.querySelector('[role="timer"]')).toBeNull()
})

it('cancels scheduled retries when the dialog closes', async () => {
    searchHub.mockResolvedValue({ models: [], searchable: null })
    await openCatalog()

    await act(() => root.render(null))
    await act(() => vi.advanceTimersByTimeAsync(60_000))

    expect(searchHub).toHaveBeenCalledTimes(1)
})

it('cancels the old retry when the selected engine changes', async () => {
    searchHub
        .mockResolvedValueOnce({ models: [], searchable: null })
        .mockResolvedValueOnce(populatedCatalog)
    await openCatalog()

    await openCatalog('lm-studio')
    await act(() => vi.advanceTimersByTimeAsync(60_000))

    expect(host.textContent).toContain('org/small-model')
    expect(searchHub).toHaveBeenCalledTimes(2)
    expect(searchHub).toHaveBeenLastCalledWith('lm-studio', 'fresh-node', undefined)
})

it('ignores a late failed request after the destination changes', async () => {
    let finishRequest: (value: EngineHubSearchResponse) => void = () => {
        throw new Error('Catalog request has not started')
    }
    searchHub.mockReturnValueOnce(
        new Promise(resolve => {
            finishRequest = resolve
        })
    )
    searchHub.mockResolvedValueOnce(populatedCatalog)
    await openCatalog('vllm', 'first-node')
    await openCatalog('vllm', 'second-node')

    await act(() => finishRequest({ models: [], searchable: null }))
    await act(() => vi.advanceTimersByTimeAsync(60_000))

    expect(host.textContent).toContain('org/small-model')
    expect(host.textContent).not.toContain("Couldn't load models")
    expect(searchHub).toHaveBeenCalledTimes(2)
})

it('keeps cached catalogs separate for different destination nodes', async () => {
    const second = {
        ...populatedCatalog,
        models: populatedCatalog.models.map(model => ({ ...model, name: 'org/another-model' }))
    }
    searchHub.mockResolvedValueOnce(populatedCatalog).mockResolvedValueOnce(second)
    await openCatalog('vllm', 'first-node')
    await openCatalog('vllm', 'second-node')
    expect(host.textContent).toContain('org/another-model')

    await openCatalog('vllm', 'first-node')

    expect(host.textContent).toContain('org/small-model')
    expect(host.textContent).not.toContain('org/another-model')
    expect(searchHub).toHaveBeenCalledTimes(2)
})

it('keeps a genuine empty result distinct from an unavailable catalog', async () => {
    searchHub.mockResolvedValue({ models: [], searchable: true })
    await openCatalog()

    await act(() => vi.advanceTimersByTimeAsync(60_000))

    expect(host.textContent).toContain('No models found')
    expect(host.textContent).not.toContain("Couldn't load models")
    expect(searchHub).toHaveBeenCalledTimes(1)
})

it('permits a search to recover after the initial browse fails', async () => {
    searchHub
        .mockResolvedValueOnce({ models: [], searchable: null })
        .mockResolvedValueOnce(populatedCatalog)
    await openCatalog()

    await click('Search fixture')

    expect(host.textContent).toContain('org/small-model')
    expect(host.textContent).not.toContain("Couldn't load models")
    expect(searchHub).toHaveBeenLastCalledWith('vllm', 'fresh-node', 'small-model')
})

it('shows a failed request when the rejection reaches the dialog', async () => {
    searchHub.mockRejectedValueOnce(new Error('Catalog temporarily unavailable'))

    await openCatalog()

    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
        'Catalog temporarily unavailable'
    )
    expect(searchHub).toHaveBeenCalledTimes(1)
})
