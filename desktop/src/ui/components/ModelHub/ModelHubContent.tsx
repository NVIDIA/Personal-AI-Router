// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { startTransition, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Flex, Stack } from '@nvidia/foundations-react-core'
import { ModelHubSearchBar } from './ModelHubSearchBar'
import { ModelHubActions } from './ModelHubActions'
import { InlineErrorBanner } from '@/ui/components/InlineErrorBanner'
import { ModelEntry, SortState } from '@/ui/types/model-hub'
import { ModelHubList } from './ModelHubList'
import { mergeModelHubResults, searchEngineHub } from '@/ui/utils/model-hub-search'
import { resolveStoredSort, writeStoredSort } from '@/ui/utils/model-hub-content-storage'
import { isHubEntryDownloaded } from '@/ui/utils/match-downloaded-model'
import { EngineType } from '@/shared/types/engines'
import type { ModelItem } from '@/ui/types/engine-info'
import getErrorString from '@/shared/utils/get-error-string'

type ProviderFilter = 'all' | 'huggingface' | 'catalog'

type ModelHubContentProps = {
    engine: EngineType | null
    multiple: boolean
    onSubmit: (models: ModelEntry[]) => void
    /** Already-installed models for this engine; matching hub results are hidden. */
    downloadedModels?: readonly ModelItem[]
    onOpenCustomModel?: () => void
}

const SORT_DEFAULT_STATE: SortState = {
    sort: 'lastModified',
    sortDescending: false,
    allDirections: {
        lastModified: false,
        name: false,
        size: false
    }
}

export const ModelHubContent = ({
    engine,
    multiple,
    onSubmit,
    downloadedModels,
    onOpenCustomModel
}: ModelHubContentProps) => {
    const [disabledListClick, setDisabledListClick] = useState(false)
    const [loading, setLoading] = useState(true)
    const [allModels, setAllModels] = useState<ModelEntry[]>([])
    const [remoteModels, setRemoteModels] = useState<ModelEntry[]>([])
    const [query, setQuery] = useState('')
    const [searching, setSearching] = useState(false)
    const [selectedModels, setSelectedModels] = useState<Set<string>>(new Set())
    const [sort, setSort] = useState<SortState>(() =>
        engine ? resolveStoredSort(engine, SORT_DEFAULT_STATE) : { ...SORT_DEFAULT_STATE }
    )
    const [errors, setErrors] = useState<{ id: string; message: string }[]>([])
    const [providerFilter, setProviderFilter] = useState<ProviderFilter>('all')
    const [hfModels, setHfModels] = useState<ModelEntry[]>([])
    const [loadingHf, setLoadingHf] = useState(false)

    // Per-engine raw-response cache so reopening an engine is instant and
    // keystrokes filter locally instead of re-invoking the search IPC.
    const cacheRef = useRef<Map<EngineType, ModelEntry[]>>(new Map())
    // Monotonic generation guard so a stale in-flight fetch never overwrites
    // results for the engine the user has since switched to.
    const fetchGenRef = useRef(0)
    const searchGenRef = useRef(0)
    const submittedQueryRef = useRef('')

    const loadHfModels = useCallback(async () => {
        if (hfModels.length > 0) return
        setLoadingHf(true)
        try {
            const hf = await searchEngineHub('llama-cpp')
            setHfModels(hf)
        } catch (error) {
            console.warn('Failed to load Hugging Face models', error)
        } finally {
            setLoadingHf(false)
        }
    }, [hfModels.length])

    const handleProviderFilterChange = useCallback(
        (filter: ProviderFilter) => {
            setProviderFilter(filter)
            if (filter === 'huggingface') {
                void loadHfModels()
            }
        },
        [loadHfModels]
    )

    const loadModels = useCallback(async (backend: EngineType) => {
        const cached = cacheRef.current.get(backend)
        if (cached) {
            setAllModels(cached)
            setLoading(false)
            return
        }
        const gen = ++fetchGenRef.current
        setLoading(true)
        try {
            const result = await searchEngineHub(backend)
            if (fetchGenRef.current !== gen) return
            cacheRef.current.set(backend, result)
            setAllModels(result)
            setLoading(false)
        } catch (error) {
            if (fetchGenRef.current !== gen) return
            console.warn('Model hub load error', error)
            setErrors(prev => [
                ...prev,
                { id: performance.now().toString(), message: getErrorString(error) }
            ])
            setAllModels([])
            setLoading(false)
        }
    }, [])

    useEffect(() => {
        setRemoteModels([])
        setSearching(false)
        submittedQueryRef.current = ''
        setProviderFilter('all')
        if (!engine) {
            setAllModels([])
            setQuery('')
            setSort({ ...SORT_DEFAULT_STATE })
            return
        }
        setQuery('')
        setSelectedModels(new Set())
        setSort(resolveStoredSort(engine, SORT_DEFAULT_STATE))
        void loadModels(engine)
        return () => {
            // Invalidate in-flight population or search for the previous engine.
            fetchGenRef.current += 1
            searchGenRef.current += 1
        }
    }, [engine, loadModels])

    const currentBaseModels = useMemo(() => {
        if (providerFilter === 'huggingface') {
            const fromAll = allModels.filter(
                m =>
                    m.url?.includes('huggingface.co') ||
                    m.id.includes('/') ||
                    m.id.startsWith('hf.co/')
            )
            return mergeModelHubResults(fromAll, hfModels)
        }
        if (providerFilter === 'catalog') {
            return allModels.filter(
                m =>
                    !m.url?.includes('huggingface.co') &&
                    !m.id.includes('/') &&
                    !m.id.startsWith('hf.co/')
            )
        }
        return mergeModelHubResults(allModels, hfModels)
    }, [providerFilter, allModels, hfModels])

    const queryFiltered = useMemo(() => {
        const q = query.toLowerCase().trim()
        if (!q) return currentBaseModels
        const populatedMatches = currentBaseModels.filter(m => m.name.toLowerCase().includes(q))
        return mergeModelHubResults(populatedMatches, remoteModels)
    }, [currentBaseModels, query, remoteModels])

    const visibleModels = useMemo(() => {
        let models = queryFiltered
        if (providerFilter === 'huggingface') {
            models = models.filter(
                m =>
                    m.url?.includes('huggingface.co') ||
                    m.id.includes('/') ||
                    m.id.startsWith('hf.co/')
            )
        } else if (providerFilter === 'catalog') {
            models = models.filter(
                m =>
                    !m.url?.includes('huggingface.co') &&
                    !m.id.includes('/') &&
                    !m.id.startsWith('hf.co/')
            )
        }
        if (!engine || !downloadedModels || downloadedModels.length === 0) return models
        return models.filter(m => !isHubEntryDownloaded(engine, m, downloadedModels))
    }, [engine, queryFiltered, downloadedModels, providerFilter])

    const handleSortPersist = useCallback(
        (next: SortState) => {
            setSort(next)
            if (engine) {
                writeStoredSort(engine, next)
            }
        },
        [engine]
    )

    const modelsRef = useRef(visibleModels)
    modelsRef.current = visibleModels

    const handleSubmit = useCallback(
        (ids: string[]) => {
            const modelSelected: ModelEntry[] = []
            for (const id of ids) {
                const found = modelsRef.current.find(model => model.id === id)
                if (found !== undefined) {
                    modelSelected.push(found)
                }
            }

            if (modelSelected.length > 0) {
                onSubmit(modelSelected)
            }
        },
        [onSubmit]
    )

    const onToggleSelectModel = useCallback((modelId: string) => {
        startTransition(() => {
            setSelectedModels(prev => {
                const newSet = new Set(prev)
                newSet.has(modelId) ? newSet.delete(modelId) : newSet.add(modelId)
                return newSet
            })
        })
    }, [])

    const handleListToggle = useCallback(
        (modelId: string) => {
            if (multiple) {
                onToggleSelectModel(modelId)
            } else {
                handleSubmit([modelId])
            }
        },
        [multiple, onToggleSelectModel, handleSubmit]
    )

    const handleQueryChange = useCallback((nextQuery: string) => {
        setQuery(nextQuery)
        if (nextQuery.trim() === submittedQueryRef.current) return
        searchGenRef.current += 1
        submittedQueryRef.current = ''
        setRemoteModels([])
        setSearching(false)
    }, [])

    const handleSearch = useCallback(async (nextQuery: string) => {
        const normalized = nextQuery.trim()
        setQuery(nextQuery)
        if (normalized.length === 0) return

        const generation = ++searchGenRef.current
        submittedQueryRef.current = normalized
        setRemoteModels([])
        setSearching(true)
        try {
            const result = await searchEngineHub('llama-cpp', normalized)
            if (searchGenRef.current !== generation) return
            setRemoteModels(result)
        } catch (error) {
            if (searchGenRef.current !== generation) return
            setErrors(prev => [
                ...prev,
                { id: performance.now().toString(), message: getErrorString(error) }
            ])
        } finally {
            if (searchGenRef.current === generation) setSearching(false)
        }
    }, [])

    return (
        <Stack gap="0" className="overflow-hidden min-h-0 min-w-0 grow w-full">
            <Stack gap="4" className="overflow-hidden min-h-0 min-w-0 grow w-full">
                <Stack gap="1">
                    {errors.length > 0 && (
                        <Stack gap="1" className="mb-4">
                            {errors.map(error => (
                                <InlineErrorBanner
                                    key={error.id}
                                    message={error.message}
                                    onClose={() =>
                                        setErrors(prev => prev.filter(e => e.id !== error.id))
                                    }
                                />
                            ))}
                        </Stack>
                    )}
                    <ModelHubSearchBar
                        onMenuOpenChange={setDisabledListClick}
                        query={query}
                        searching={searching}
                        onQueryChange={handleQueryChange}
                        onSearch={handleSearch}
                        sort={sort}
                        onSort={handleSortPersist}
                    />
                    <Flex align="center" justify="between" className="pt-1.5 px-0.5">
                        <Flex align="center" gap="1">
                            <button
                                type="button"
                                onClick={() => handleProviderFilterChange('all')}
                                className={`px-2.5 py-1 text-xs font-medium rounded-md transition-colors ${
                                    providerFilter === 'all'
                                        ? 'bg-white/15 text-white shadow-sm'
                                        : 'text-subtle-color hover:text-white hover:bg-white/5'
                                }`}
                            >
                                All Providers
                            </button>
                            <button
                                type="button"
                                onClick={() => handleProviderFilterChange('huggingface')}
                                className={`flex items-center gap-1.5 px-2.5 py-1 text-xs font-medium rounded-md transition-colors ${
                                    providerFilter === 'huggingface'
                                        ? 'bg-amber-500/20 text-amber-300 border border-amber-500/40 shadow-sm'
                                        : 'text-subtle-color hover:text-white hover:bg-white/5'
                                }`}
                            >
                                <span>🤗 Hugging Face</span>
                                {loadingHf && (
                                    <span
                                        className="spinner-element inline-block"
                                        style={{ width: 10, height: 10 }}
                                    />
                                )}
                            </button>
                            <button
                                type="button"
                                onClick={() => handleProviderFilterChange('catalog')}
                                className={`px-2.5 py-1 text-xs font-medium rounded-md transition-colors ${
                                    providerFilter === 'catalog'
                                        ? 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/40 shadow-sm'
                                        : 'text-subtle-color hover:text-white hover:bg-white/5'
                                }`}
                            >
                                Engine Catalog
                            </button>
                        </Flex>
                        {onOpenCustomModel && (
                            <button
                                type="button"
                                onClick={onOpenCustomModel}
                                className="text-xs text-subtle-color hover:text-white underline underline-offset-2 transition-colors"
                            >
                                Add custom model...
                            </button>
                        )}
                    </Flex>
                </Stack>
                <ModelHubList
                    className="grow min-h-0"
                    disabled={disabledListClick}
                    multiple={multiple}
                    loading={loading}
                    models={visibleModels}
                    selectedModels={selectedModels}
                    onToggleSelectModel={handleListToggle}
                    sort={sort}
                    onOpenCustomModel={onOpenCustomModel}
                />

                {!!multiple && (
                    <ModelHubActions
                        onSubmit={() => handleSubmit(Array.from(selectedModels))}
                        disabled={selectedModels.size === 0 || loading}
                        count={selectedModels.size}
                    />
                )}
            </Stack>
        </Stack>
    )
}
