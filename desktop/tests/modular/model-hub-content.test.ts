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
    ModelHubSearchBar: ({
        query,
        onQueryChange,
        onSearch
    }: ComponentProps<typeof ModelHubSearchBar>) =>
        createElement(
            'div',
            null,
            createElement('input', {
                value: query,
                'aria-label': 'Search for a model',
                onInput: event => onQueryChange(event.currentTarget.value)
            }),
            createElement('button', { onClick: () => onSearch(query) }, 'Search fixture')
        )
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

async function changeQuery(query: string) {
    const input = host.querySelector('input')
    if (!input) throw new Error('Missing model search input')
    await act(() => {
        input.value = query
        input.dispatchEvent(new Event('input', { bubbles: true }))
    })
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

    await changeQuery('small-model')
    await click('Search fixture')

    expect(host.textContent).toContain('org/small-model')
    expect(host.textContent).not.toContain("Couldn't load models")
    expect(searchHub).toHaveBeenLastCalledWith('vllm', 'fresh-node', 'small-model')
})

const testBrowseErrorAfterSearch = (
    name: string,
    engine: EngineType,
    searchResult: EngineHubSearchResponse,
    expectedSearchText: string
) => {
    it(name, async () => {
        searchHub
            .mockResolvedValueOnce({ models: [], searchable: null })
            .mockResolvedValueOnce(searchResult)
            .mockResolvedValueOnce(populatedCatalog)
        await openCatalog(engine)

        await changeQuery('small-model')
        await click('Search fixture')

        expect(host.textContent).toContain(expectedSearchText)
        expect(host.textContent).not.toContain("Couldn't load models")
        expect(host.querySelector('[role="timer"]')).toBeNull()
        await act(() => vi.advanceTimersByTimeAsync(60_000))
        expect(searchHub).toHaveBeenCalledTimes(2)

        await changeQuery('')

        expect(host.textContent).toContain("Couldn't load models")
        expect(host.textContent).not.toContain('No models found')
        expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 5s…')

        await click('Retry')

        expect(searchHub).toHaveBeenLastCalledWith(engine, 'fresh-node', undefined)
        expect(host.textContent).toContain('org/small-model')
        expect(host.textContent).not.toContain("Couldn't load models")
    })
}
testBrowseErrorAfterSearch(
    'restores vLLM browse Retry after clearing a successful search',
    'vllm',
    populatedCatalog,
    'org/small-model'
)
testBrowseErrorAfterSearch(
    'restores LM Studio browse Retry after clearing a successful search',
    'lm-studio',
    populatedCatalog,
    'org/small-model'
)
testBrowseErrorAfterSearch(
    'restores llama.cpp browse Retry after clearing a successful search',
    'llama-cpp',
    populatedCatalog,
    'org/small-model'
)
testBrowseErrorAfterSearch(
    'keeps an empty vLLM search distinct from a failed browse',
    'vllm',
    { models: [], searchable: true },
    'No models found'
)
testBrowseErrorAfterSearch(
    'keeps an empty LM Studio search distinct from a failed browse',
    'lm-studio',
    { models: [], searchable: true },
    'No models found'
)
testBrowseErrorAfterSearch(
    'keeps an empty llama.cpp search distinct from a failed browse',
    'llama-cpp',
    { models: [], searchable: true },
    'No models found'
)

const testFailedSearchRetry = (name: string, engine: EngineType) => {
    it(name, async () => {
        searchHub
            .mockResolvedValueOnce({ models: [], searchable: null })
            .mockResolvedValueOnce({ models: [], searchable: null })
            .mockResolvedValueOnce(populatedCatalog)
        await openCatalog(engine)
        await changeQuery('small-model')
        await click('Search fixture')

        expect(host.textContent).toContain("Couldn't search models")
        expect(host.textContent).not.toContain('No models found')
        expect(host.querySelector('[role="timer"]')).toBeNull()

        await click('Retry')

        expect(searchHub).toHaveBeenLastCalledWith(engine, 'fresh-node', 'small-model')
        expect(searchHub).toHaveBeenCalledTimes(3)
        expect(host.querySelector('input')?.value).toBe('small-model')
        expect(host.textContent).toContain('org/small-model')
        expect(host.querySelector('[role="alert"]')).toBeNull()

        await changeQuery('')
        expect(host.textContent).toContain("Couldn't load models")
    })
}
testFailedSearchRetry('retries a failed vLLM query after the initial browse fails', 'vllm')
testFailedSearchRetry(
    'retries a failed LM Studio query after the initial browse fails',
    'lm-studio'
)
testFailedSearchRetry(
    'retries a failed llama.cpp query after the initial browse fails',
    'llama-cpp'
)

const popularCatalog: EngineHubSearchResponse = {
    ...populatedCatalog,
    models: populatedCatalog.models.map(model => ({
        ...model,
        id: 'org/popular-model',
        name: 'org/popular-model'
    }))
}

it('retries a failed query instead of reloading a successful browse list', async () => {
    searchHub
        .mockResolvedValueOnce(popularCatalog)
        .mockRejectedValueOnce(new Error('Search temporarily unavailable'))
        .mockResolvedValueOnce(populatedCatalog)
    await openCatalog()
    await changeQuery('small-model')
    await click('Search fixture')

    expect(host.textContent).toContain("Couldn't search models")
    expect(host.textContent).toContain('Search temporarily unavailable')

    await click('Retry')

    expect(searchHub).toHaveBeenLastCalledWith('vllm', 'fresh-node', 'small-model')
    expect(searchHub).toHaveBeenCalledTimes(3)
    expect(host.textContent).toContain('org/small-model')
    expect(host.textContent).not.toContain('org/popular-model')
    expect(host.querySelector('[role="alert"]')).toBeNull()
})

it('keeps local matches available with a Retry action when an upstream search fails', async () => {
    searchHub
        .mockResolvedValueOnce(populatedCatalog)
        .mockRejectedValueOnce(new Error('Search temporarily unavailable'))
        .mockResolvedValueOnce(populatedCatalog)
    await openCatalog()
    await changeQuery('small-model')
    await click('Search fixture')

    expect(host.textContent).toContain('org/small-model')
    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
        'Search temporarily unavailable'
    )

    await click('Retry')

    expect(searchHub).toHaveBeenLastCalledWith('vllm', 'fresh-node', 'small-model')
    expect(host.textContent).toContain('org/small-model')
    expect(host.querySelector('[role="alert"]')).toBeNull()
})

it('keeps a failed search retryable until it returns a genuine empty result', async () => {
    searchHub
        .mockResolvedValueOnce(popularCatalog)
        .mockResolvedValueOnce({ models: [], searchable: null })
        .mockResolvedValueOnce({ models: [], searchable: null })
        .mockResolvedValueOnce({ models: [], searchable: true })
    await openCatalog()
    await changeQuery('small-model')
    await click('Search fixture')

    await click('Retry')
    expect(host.textContent).toContain("Couldn't search models")
    await click('Retry')

    expect(searchHub).toHaveBeenLastCalledWith('vllm', 'fresh-node', 'small-model')
    expect(searchHub).toHaveBeenCalledTimes(4)
    expect(host.textContent).toContain('No models found')
    expect(host.querySelector('[role="alert"]')).toBeNull()
})

it('clears the previous search error when the query changes', async () => {
    searchHub
        .mockResolvedValueOnce(popularCatalog)
        .mockRejectedValueOnce(new Error('Previous search failed'))
        .mockResolvedValueOnce(populatedCatalog)
    await openCatalog()
    await changeQuery('missing-model')
    await click('Search fixture')

    await changeQuery('small-model')

    expect(host.textContent).not.toContain('Previous search failed')
    expect(host.querySelector('[role="alert"]')).toBeNull()
    await click('Search fixture')
    expect(searchHub).toHaveBeenLastCalledWith('vllm', 'fresh-node', 'small-model')
    expect(host.textContent).toContain('org/small-model')
})

it('ignores a search retry failure after the query is cleared', async () => {
    let finishRequest: (value: EngineHubSearchResponse) => void = () => {
        throw new Error('Search retry has not started')
    }
    searchHub
        .mockResolvedValueOnce({ models: [], searchable: null })
        .mockResolvedValueOnce({ models: [], searchable: null })
        .mockReturnValueOnce(
            new Promise(resolve => {
                finishRequest = resolve
            })
        )
    await openCatalog()
    await changeQuery('small-model')
    await click('Search fixture')
    await click('Retry')

    expect(host.textContent).toContain('Searching models...')
    expect(searchHub).toHaveBeenLastCalledWith('vllm', 'fresh-node', 'small-model')
    await changeQuery('')
    await act(() => finishRequest({ models: [], searchable: null }))

    expect(host.textContent).toContain("Couldn't load models")
    expect(host.textContent).not.toContain("Couldn't search models")
    expect(host.querySelector('[role="timer"]')?.textContent).toBe('Retrying in 5s…')
})

it('shows a failed request when the rejection reaches the dialog', async () => {
    searchHub.mockRejectedValueOnce(new Error('Catalog temporarily unavailable'))

    await openCatalog()

    expect(host.querySelector('[role="alert"]')?.textContent).toContain(
        'Catalog temporarily unavailable'
    )
    expect(searchHub).toHaveBeenCalledTimes(1)
})
